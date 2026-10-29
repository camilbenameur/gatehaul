package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/lock"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/patch"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// patchTimeout covers waking a stopped host and receiving the patch.
const patchTimeout = 5 * time.Minute

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// GenOptions configures Gen: the options of Run, and whether the sanitiser
// lets the patch touch sensitive paths (gatehaul.toml, workflows, hooks).
type GenOptions struct {
	RunOptions
	AllowSensitive bool
}

// Gen is Run for a command that writes files: when the job succeeds, the patch
// of the files it changed is fetched, checked and applied to the working tree.
// A patch that is refused or does not apply exits 6.
func (c *Controller) Gen(ctx context.Context, o GenOptions) int {
	rs, code := c.dispatch(ctx, o.RunOptions, runner.KindGen, o.AllowSensitive)
	if rs == nil {
		return code
	}
	c.applyFinished(ctx, rs, o.AllowSensitive)
	return c.report(rs, false, o.JSON)
}

// ApplyOptions configures Apply. An empty ID means the latest gen job of HEAD,
// whatever its state: one that has not ended is reported as such.
type ApplyOptions struct {
	ID             string
	AllowSensitive bool
	JSON           bool
}

// Apply brings back the patch of a gen job that succeeded, from the local copy
// when it is there, and applies it unless it already was.
func (c *Controller) Apply(ctx context.Context, o ApplyOptions) int {
	var d job.Dispatch
	var err error
	if o.ID == "" {
		sha, _, e := c.commit(ctx, "")
		if e != nil {
			return c.fail(o.JSON, e)
		}
		if d, err = c.Cache.FindLatestGen(c.Repo, sha); err != nil {
			if isNotFound(err) {
				return c.fail(o.JSON, &out.Error{Code: out.CodeNoSuchJob, Message: "no gen job was dispatched for " + short(sha),
					Fix: "gatehaul gen -- <cmd> starts one, or pass the id of a job: gatehaul apply <id>", Exit: out.ExitInfra})
			}
			return c.fail(o.JSON, c.lookupError(err, "a gen job of "+short(sha)))
		}
	} else if d, err = c.Cache.Resolve(o.ID); err != nil {
		return c.fail(o.JSON, c.lookupError(err, o.ID))
	}
	if e := c.checkRepo(d); e != nil {
		return c.fail(o.JSON, e)
	}
	if d.Kind != runner.KindGen {
		return c.fail(o.JSON, &out.Error{Code: out.CodeNoPatch, Message: fmt.Sprintf("job %s is a %s job, and only gen jobs have a patch", d.ID, d.Kind),
			Fix: "gatehaul gen -- <cmd> runs a command and applies what it changed", Exit: out.ExitUsage, ID: d.ID})
	}
	// A job that is not known to be finished is polled once, without waiting.
	rs, e := c.waitJobs(ctx, []job.Dispatch{d}, true, 0)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	r := rs[0]
	switch {
	case !r.done:
		return c.fail(o.JSON, &out.Error{Code: out.CodeJobNotDone, Message: fmt.Sprintf("job %s is still %s, so it has no patch yet", d.ID, r.state),
			Fix: "gatehaul wait " + d.ID, Exit: out.ExitRunning, ID: d.ID})
	case r.state != job.Succeeded:
		return c.fail(o.JSON, &out.Error{Code: out.CodeNoPatch, Message: fmt.Sprintf("job %s ended %s, so it has no patch", d.ID, r.state),
			Fix: "gatehaul log " + d.ID, Exit: r.state.ExitCode(), ID: d.ID})
	}
	c.applyPatch(ctx, r, o.AllowSensitive)
	return c.report(rs, false, o.JSON)
}

// PatchReport is the "patch" object of a gen job in the JSON output.
type PatchReport struct {
	SHA256         string          `json:"sha256"`
	Files          []patch.File    `json:"files"`
	SensitivePaths []string        `json:"sensitive_paths"`
	Problems       []patch.Problem `json:"problems"`
	Applied        bool            `json:"applied"`
	AlreadyApplied bool            `json:"already_applied,omitempty"`
}

// patchOutcome is what became of the patch of one gen job.
type patchOutcome struct {
	sha       string
	files     []patch.File
	sensitive []string
	problems  []patch.Problem
	conflicts []string
	none      bool // the command changed nothing
	applied   bool // in the working tree, now or before
	already   bool // it was applied by an earlier call
	next      string
	err       *out.Error
}

func (p *patchOutcome) report() *PatchReport {
	rep := &PatchReport{SHA256: p.sha, Files: p.files, SensitivePaths: p.sensitive, Problems: p.problems,
		Applied: p.applied, AlreadyApplied: p.already}
	if rep.Files == nil {
		rep.Files = []patch.File{}
	}
	if rep.SensitivePaths == nil {
		rep.SensitivePaths = []string{}
	}
	if rep.Problems == nil {
		rep.Problems = []patch.Problem{}
	}
	return rep
}

// applyFinished applies the patch of every gen job in rs that succeeded.
func (c *Controller) applyFinished(ctx context.Context, rs []*jobResult, allowSensitive bool) {
	for _, r := range rs {
		if r.done && r.state == job.Succeeded && r.d.Kind == runner.KindGen {
			c.applyPatch(ctx, r, allowSensitive)
		}
	}
}

func (c *Controller) applyPatch(ctx context.Context, r *jobResult, allowSensitive bool) {
	r.patch = c.patchOf(ctx, r, allowSensitive)
}

// applyLockWait bounds the wait for the lock of a job: the process that holds
// it may be fetching the patch and writing it.
const applyLockWait = patchTimeout + 3*time.Minute

// refusal is an error about the patch of job id, which was not applied.
func refusal(code out.Code, id, msg, fix string) *out.Error {
	return &out.Error{Code: code, Message: msg, Fix: fix, Exit: out.ExitPatchRefused, ID: id}
}

// appliedOutcome is a patch that jobs/<id>/applied.json says was applied.
func appliedOutcome(a job.Applied) *patchOutcome {
	p := &patchOutcome{sha: a.SHA256, applied: true, already: true}
	for _, f := range a.Files {
		p.files = append(p.files, patch.File{Path: f})
	}
	return p
}

// recordedPatch returns what applied.json says about job id: the patch as it
// was applied, an error when the record cannot be read, or nil when the patch
// was not applied.
func (c *Controller) recordedPatch(id string) *patchOutcome {
	a, ok, err := c.Cache.LoadApplied(id)
	switch {
	case err != nil:
		return &patchOutcome{err: &out.Error{Code: out.CodeCacheError, Message: "cannot read the record of the applied patch of " + id + ": " + err.Error(),
			Fix: "check the permissions of " + c.Cache.Dir() + ", or remove jobs/" + id + "/applied.json if you know the patch was not applied", Exit: out.ExitInfra, ID: id}}
	case ok:
		return appliedOutcome(a)
	}
	return nil
}

// patchOf fetches, checks and applies the patch of a gen job that succeeded.
// A patch that was applied before is reported, never applied again. The lock
// of the job is held from the second look at applied.json to the last write,
// so that two processes cannot both write the same patch.
func (c *Controller) patchOf(ctx context.Context, r *jobResult, allowSensitive bool) *patchOutcome {
	id := r.d.ID
	allowSensitive = allowSensitive || r.d.AllowSensitive
	if p := c.recordedPatch(id); p != nil {
		return p
	}
	if r.status.PatchSize == 0 {
		return &patchOutcome{sha: r.status.PatchSHA256, none: true}
	}
	expected := r.status.PatchSHA256
	if !sha256RE.MatchString(expected) {
		return &patchOutcome{err: &out.Error{Code: out.CodePatchMismatch,
			Message: fmt.Sprintf("the host reported no valid sha256 for the patch of job %s (%q)", id, expected),
			Fix:     "nothing was applied; update gatehaul, then run: gatehaul gen -- " + shellQuote(r.d.Cmd), Exit: out.ExitInfra, ID: id}}
	}

	lctx, cancel := context.WithTimeout(ctx, applyLockWait)
	release, err := lock.Acquire(lctx, filepath.Join(c.Cache.JobDir(id), "apply.lock"))
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return &patchOutcome{sha: expected, err: interrupted()}
		}
		e := &out.Error{Code: out.CodeLockBusy, Message: "another gatehaul is applying the patch of job " + id + ": " + err.Error(),
			Fix: "wait for it to finish, then run: gatehaul apply " + id, Exit: out.ExitInfra, ID: id}
		return &patchOutcome{sha: expected, err: e, next: "gatehaul apply " + id}
	}
	defer func() { _ = release() }()
	return c.patchLocked(ctx, r, expected, allowSensitive)
}

// patchLocked is patchOf with the lock of the job held.
func (c *Controller) patchLocked(ctx context.Context, r *jobResult, expected string, allowSensitive bool) *patchOutcome {
	id := r.d.ID
	stateDir := c.Cache.JobDir(id)

	// An apply that did not end left the marker: its files come back first.
	restored, err := patch.Recover(stateDir)
	if err != nil {
		msg := "cannot restore the working tree after an apply of job " + id + " that did not end: " + oneLine(err.Error())
		fix := "copy the files saved in " + filepath.Join(stateDir, "preimage", "files") + " back by hand (manifest.json beside them names each one), " +
			"then delete " + filepath.Join(stateDir, "applying") + " and run: gatehaul apply " + id
		e := &out.Error{Code: out.CodePatchError, Message: msg, Fix: fix, Exit: out.ExitInfra, ID: id}
		return &patchOutcome{sha: expected, err: e}
	}
	if restored != nil {
		fmt.Fprintf(c.Stderr, "warning: an earlier apply of job %s did not end; the working tree was restored first (%d file(s) put back: %s)\n",
			id, len(restored), listPaths(safePaths(restored), 5))
	}
	// Another process may have applied it while this one waited for the lock.
	if p := c.recordedPatch(id); p != nil {
		return p
	}

	data, e := c.patchBytes(ctx, r, expected)
	if e != nil {
		e.ID = id
		return &patchOutcome{sha: expected, err: e, next: "gatehaul apply " + id}
	}

	top, err := (detect.Git{Runner: c.Runner, Dir: c.Dir}).TopLevel(ctx)
	if err != nil {
		e := asError(err, out.CodeNotGitRepo, "run gatehaul from inside the repository, then: gatehaul apply "+id)
		e.ID = id
		return &patchOutcome{sha: expected, err: e}
	}
	if e := checkTree(r.d, top); e != nil {
		return &patchOutcome{sha: expected, err: e}
	}
	// The sanitiser's own limits apply (files, bytes per file, bytes in all).
	rep, err := patch.Check(ctx, c.Runner, top, r.d.SHA, data, expected, patch.Options{AllowSensitive: allowSensitive})
	if err != nil {
		e := asError(err, out.CodePatchError, "the base commit "+short(r.d.SHA)+" must exist locally; run git fsck, then: gatehaul apply "+id)
		if e.Code == out.CodePatchError {
			e.Message = "cannot check the patch of job " + id + ": " + oneLine(err.Error())
		}
		e.ID = id
		return &patchOutcome{sha: expected, err: e}
	}
	p := &patchOutcome{sha: rep.SHA256, files: rep.Files, sensitive: rep.Sensitive, problems: rep.Problems}
	if p.sha == "" {
		p.sha = expected
	}
	if !rep.OK() {
		fix := "the patch was not applied; review the listed paths, change the command so it does not write them, " +
			"or rerun with --allow-sensitive if a sensitive path is expected: gatehaul apply " + id + " --allow-sensitive"
		p.err = refusal(out.CodePatchRefused, id,
			fmt.Sprintf("the patch of job %s was refused (%d problem(s)) and nothing was applied", id, len(rep.Problems)), fix)
		return p
	}
	if e := c.checkBase(ctx, r.d, top, rep.Files); e != nil {
		p.err = e
		return p
	}
	return c.writePatch(ctx, r, p, top, data, stateDir)
}

// writePatch applies data to the work tree top, with a way back, and records
// it. p already holds the sanitiser's report.
func (c *Controller) writePatch(ctx context.Context, r *jobResult, p *patchOutcome, top string, data []byte, stateDir string) *patchOutcome {
	id := r.d.ID
	paths := make([]string, len(p.files))
	for i, f := range p.files {
		paths[i] = f.Path
	}
	err := patch.ApplySafely(ctx, c.Runner, top, data, paths, stateDir)
	var ae *patch.ApplyError
	switch {
	case err == nil:
		p.applied = true
	case errors.Is(err, patch.ErrAlreadyApplied):
		// The tree holds the patch although applied.json does not say so (it
		// was lost, or the process was killed before it was written).
		p.applied, p.already = true, true
	case errors.As(err, &ae):
		p.conflicts = ae.Paths
		msg := fmt.Sprintf("the patch of job %s does not apply to your working tree", id)
		if len(ae.Paths) > 0 {
			msg += ": " + listPaths(safePaths(ae.Paths), 5)
		}
		if d := oneLine(ae.Detail); d != "" {
			msg += " (" + truncate(d, 200) + ")"
		}
		p.err = refusal(out.CodePatchConflict, id, msg, conflictFix(r.d, ae))
		p.next = "gatehaul apply " + id
		return p
	default:
		e := asError(err, out.CodePatchError, "gatehaul apply "+id+" (if the message says the files could not be restored, the next run restores them first)")
		if e.Code == out.CodePatchError {
			e.Message = "cannot apply the patch of job " + id + ": " + oneLine(err.Error())
		}
		e.ID = id
		p.err, p.next = e, "gatehaul apply "+id
		return p
	}
	c.cacheWarn(c.Cache.SaveApplied(id, job.Applied{SHA256: p.sha, Files: paths, AppliedAt: c.Clock.Now().UTC().Format(time.RFC3339)}))
	return p
}

// conflictFix says what to do about a patch that does not fit. It blames a file
// in the way only when git says so.
func conflictFix(d job.Dispatch, ae *patch.ApplyError) string {
	if strings.Contains(ae.Detail, "already exists in working directory") {
		return "a file that the patch adds already exists: move or delete it, then gatehaul apply " + d.ID
	}
	return "the listed files differ from commit " + short(d.SHA) + " in your working tree (local changes, or another base): " +
		"undo, commit or stash those changes, then gatehaul apply " + d.ID
}

// sameDir reports whether a and b are the same directory.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

// checkTree refuses a patch that would be written to another work tree than
// the one the job was dispatched from.
func checkTree(d job.Dispatch, top string) *out.Error {
	if d.Top == "" || sameDir(d.Top, top) {
		return nil
	}
	return refusal(out.CodeWrongRepo, d.ID,
		fmt.Sprintf("job %s was dispatched from the work tree %s, not from %s, so its patch was not applied", d.ID, d.Top, top),
		"run the command from that work tree: gatehaul apply "+d.ID)
}

// checkBase refuses a patch whose base is not the work tree's: HEAD must be the
// commit the job ran on, or a descendant of it in which none of the files of
// the patch changed.
func (c *Controller) checkBase(ctx context.Context, d job.Dispatch, top string, files []patch.File) *out.Error {
	failed := func(err error) *out.Error {
		e := asError(err, out.CodePatchError, "run git status in "+top+" to see the error, then: gatehaul apply "+d.ID)
		if e.Code == out.CodePatchError {
			e.Message = "cannot compare HEAD with the commit job " + d.ID + " ran on: " + oneLine(err.Error())
		}
		e.ID = d.ID
		return e
	}
	stale := func(head, why string) *out.Error {
		return refusal(out.CodeStaleBase, d.ID,
			fmt.Sprintf("job %s ran on commit %s, but the work tree %s is at %s (%s), so its patch was not applied", d.ID, short(d.SHA), top, head, why),
			"check out the dispatched commit with git checkout "+d.SHA+" and run: gatehaul apply "+d.ID+"; or run again on this commit: gatehaul gen -- "+shellQuote(d.Cmd))
	}
	head, err := (detect.Git{Runner: c.Runner, Dir: top}).Head(ctx)
	if errors.Is(err, detect.ErrNoCommit) {
		return stale("no commit", "it has none")
	}
	if err != nil {
		return failed(err)
	}
	if head.Value == d.SHA {
		return nil
	}
	res, err := c.Runner.Run(ctx, []string{"git", "-C", top, "merge-base", "--is-ancestor", d.SHA, "HEAD"}, nil, gitTimeout)
	if err != nil {
		return failed(err)
	}
	switch res.ExitCode {
	case 0:
	case 1:
		return stale(short(head.Value), short(d.SHA)+" is not one of its ancestors")
	default:
		return failed(fmt.Errorf("git merge-base exit %d: %s", res.ExitCode, oneLine(string(res.Stderr))))
	}
	res, err = c.Runner.Run(ctx, []string{"git", "-C", top, "diff-tree", "-r", "-z", "--name-only", "--no-renames", d.SHA, "HEAD"}, nil, gitTimeout)
	if err != nil {
		return failed(err)
	}
	if !res.OK() {
		return failed(fmt.Errorf("git diff-tree exit %d: %s", res.ExitCode, oneLine(string(res.Stderr))))
	}
	patched := make(map[string]bool, len(files))
	for _, f := range files {
		patched[f.Path] = true
	}
	var touched []string
	for _, name := range strings.Split(string(res.Stdout), "\x00") {
		if patched[name] {
			touched = append(touched, name)
		}
	}
	switch {
	case res.Truncated:
		return stale(short(head.Value), "too many files changed since to compare them")
	case len(touched) > 0:
		return stale(short(head.Value), "the patch touches files that changed since: "+listPaths(safePaths(touched), 5))
	}
	return nil
}

// patchBytes returns the patch of r whose sha256 is expected: the copy in the
// cache when it is intact, else the one the host sends (and the cache keeps).
func (c *Controller) patchBytes(ctx context.Context, r *jobResult, expected string) ([]byte, *out.Error) {
	id := r.d.ID
	if data, ok, err := c.Cache.LoadPatch(id); err == nil && ok && sumHex(data) == expected {
		return data, nil
	}
	pd, e := c.fetchPatch(ctx, r.d)
	if e != nil {
		return nil, e
	}
	data, err := base64.StdEncoding.DecodeString(pd.DataB64)
	if err != nil {
		return nil, &out.Error{Code: out.CodeRunnerError, Message: "the patch of " + id + " is not valid base64: " + err.Error(),
			Fix: "update gatehaul, then retry: gatehaul apply " + id, Exit: out.ExitInfra}
	}
	if int64(len(data)) > runner.MaxPatchBytes {
		return nil, tooLarge(id)
	}
	if got := sumHex(data); got != expected || pd.SHA256 != expected || pd.Size != int64(len(data)) {
		return nil, &out.Error{Code: out.CodePatchMismatch,
			Message: fmt.Sprintf("the patch of job %s does not match its checksum: the host reported sha256 %s, the bytes received hash to %s", id, short(expected), short(got)),
			Fix:     "nothing was applied; retry with: gatehaul apply " + id + "; if it happens again, run gatehaul doctor", Exit: out.ExitInfra}
	}
	c.cacheWarn(c.Cache.SavePatch(id, data))
	return data, nil
}

// fetchPatch asks the host that ran d for its patch.
func (c *Controller) fetchPatch(ctx context.Context, d job.Dispatch) (runner.PatchData, *out.Error) {
	hosts, e := c.discover(ctx)
	if e != nil {
		return runner.PatchData{}, e
	}
	h, found := hostFor(hosts, d)
	switch found {
	case hostGone:
		return runner.PatchData{}, &out.Error{Code: out.CodeHostGone, Message: d.Host + " no longer exists, so the patch of " + d.ID + " is gone",
			Fix: "gatehaul up, then run the command again: gatehaul gen -- " + shellQuote(d.Cmd), Exit: out.ExitInfra}
	case hostRecreated:
		return runner.PatchData{}, &out.Error{Code: out.CodeHostReplaced, Message: d.Host + " was recreated, so the patch of " + d.ID + " is gone",
			Fix: "gatehaul gen -- " + shellQuote(d.Cmd), Exit: out.ExitInfra}
	}
	if e := unusable(h); e != nil {
		return runner.PatchData{}, e
	}
	pctx, cancel := context.WithTimeout(ctx, patchTimeout)
	defer cancel()
	pd, err := c.API(h).Patch(pctx, d.ID)
	if err != nil {
		if remoteCode(err) == runner.ErrCodePatchTooLarge {
			return runner.PatchData{}, tooLarge(d.ID)
		}
		if ctx.Err() != nil {
			return runner.PatchData{}, interrupted()
		}
		return runner.PatchData{}, runnerError(err, h)
	}
	return pd, nil
}

func tooLarge(id string) *out.Error {
	return &out.Error{Code: out.CodePatchTooLarge,
		Message: fmt.Sprintf("the patch of job %s is over %d MiB, so it was not brought back", id, runner.MaxPatchBytes>>20),
		Fix:     "the patch was not applied; make the command change fewer or smaller files, or run it on the host by hand and commit the result", Exit: out.ExitPatchRefused, ID: id}
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writePatchText prints the patch lines of a gen job. The paths and reasons
// come from the job's output, so control characters are replaced.
func writePatchText(w io.Writer, r *jobResult) {
	p := r.patch
	if p == nil {
		return
	}
	switch {
	case p.none:
		fmt.Fprintln(w, "patch: no changes")
	case p.already:
		fmt.Fprintf(w, "patch: already applied %d files (sha256 %s)\n", len(p.files), short(p.sha))
	case p.applied:
		fmt.Fprintf(w, "patch: applied %d files (sha256 %s)\n", len(p.files), short(p.sha))
	case p.err != nil:
		fmt.Fprintf(w, "patch: not applied (%s)\n", p.err.Code)
	}
	for _, pr := range p.problems {
		if pr.Path == "" {
			// A problem with the patch as a whole.
			line := safeText(pr.Reason)
			if d := safeText(pr.Detail); d != "" {
				line += " " + d
			}
			fmt.Fprintf(w, "refused (whole patch): %s\n", line)
			continue
		}
		fmt.Fprintf(w, "refused %s: %s\n", safeText(pr.Path), safeText(pr.Reason))
	}
	for _, path := range p.conflicts {
		fmt.Fprintf(w, "conflict %s\n", safeText(path))
	}
	for _, path := range p.sensitive {
		fmt.Fprintf(w, "sensitive %s\n", safeText(path))
	}
}

// writePatchErrors prints, on stderr, why the patches that failed did.
func (c *Controller) writePatchErrors(rs []*jobResult, asJSON bool) {
	for _, r := range rs {
		if r.patch != nil && r.patch.err != nil {
			out.WriteError(c.Stderr, asJSON, r.patch.err)
		}
	}
}

// safeText replaces control characters, so that text from a job cannot move
// the cursor or forge a line.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

func safePaths(paths []string) []string {
	res := make([]string, len(paths))
	for i, p := range paths {
		res[i] = safeText(p)
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
