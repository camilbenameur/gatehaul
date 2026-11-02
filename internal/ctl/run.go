package ctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/bundle"
	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// RunOptions configures Run. Host 0 means any host. Cmd is what follows "--".
type RunOptions struct {
	Ref     string
	Host    int
	Prio    int
	Timeout time.Duration
	Block   time.Duration
	JSON    bool
	Dir     string // --cd: run in this directory of the repository, relative to its root
	Cmd     []string
}

// Run sends a commit to a host, queues the command and waits at most
// o.Block. It returns the exit code of the command.
func (c *Controller) Run(ctx context.Context, o RunOptions) int {
	rs, code := c.dispatch(ctx, o, runner.KindRun, false)
	if rs == nil {
		return code
	}
	return c.report(rs, false, o.JSON)
}

// dispatch sends a commit to a host, queues a job of the given kind and waits
// at most o.Block. It returns the job as it stands then, or a nil list and the
// exit code when it failed before there was a job to report (the error is
// already printed). allowSensitive is kept in the job record, for wait and
// apply.
func (c *Controller) dispatch(ctx context.Context, o RunOptions, kind string, allowSensitive bool) ([]*jobResult, int) {
	verb := "gatehaul " + kind
	if len(o.Cmd) == 0 {
		return nil, c.fail(o.JSON, usageError("no command given", verb+" -- <cmd>"))
	}
	if o.Prio < 0 || o.Prio > 9 {
		return nil, c.fail(o.JSON, usageError("--prio must be between 0 and 9", verb+" --prio 5 -- <cmd>"))
	}
	if o.Timeout <= 0 || o.Block < 0 {
		return nil, c.fail(o.JSON, usageError("--timeout must be positive and --block must not be negative", verb+" --timeout 30m --block 5m -- <cmd>"))
	}

	sha, fullRef, e := c.commit(ctx, o.Ref)
	if e != nil {
		return nil, c.fail(o.JSON, e)
	}
	subdir, e := cleanSubdir(o.Dir, verb)
	if e != nil {
		return nil, c.fail(o.JSON, e)
	}
	if o.Ref == "" {
		if ch, err := (detect.Git{Runner: c.Runner, Dir: c.Dir}).Changes(ctx); err == nil {
			if len(ch.Modified) > 0 {
				w := out.Warn("worktree", out.CodeDirtyTree,
					fmt.Sprintf("%d modified file(s) are not committed, so NOT sent and NOT tested: %s", len(ch.Modified), listPaths(ch.Modified, 5)),
					"commit them before "+verb+", or stash them")
				fmt.Fprintf(c.Stderr, "warning: %s\nFix: %s\n", w.Message, w.Fix)
			}
			if len(ch.Untracked) > 0 {
				w := out.Warn("worktree", out.CodeUntracked,
					fmt.Sprintf("%d untracked file(s) are not in git, so not sent: %s", len(ch.Untracked), listPaths(ch.Untracked, 5)),
					"git add and commit them if the build needs them; otherwise ignore them (add them to .gitignore)")
				fmt.Fprintf(c.Stderr, "warning: %s\nFix: %s\n", w.Message, w.Fix)
			}
		}
	}

	hosts, e := c.discover(ctx)
	if e != nil {
		return nil, c.fail(o.JSON, e)
	}
	if len(hosts) == 0 {
		return nil, c.fail(o.JSON, &out.Error{Code: out.CodeNoHosts, Message: "no codespace of " + c.Repo + " is labelled " + host.DisplayPrefix + "<n>",
			Fix: "gatehaul up", Exit: out.ExitInfra})
	}
	h, ok := pickHost(hosts, o.Host)
	if !ok {
		return nil, c.fail(o.JSON, &out.Error{Code: out.CodeHostGone, Message: fmt.Sprintf("%s does not exist", host.DisplayName(o.Host)),
			Fix: fmt.Sprintf("gatehaul up -n %d", o.Host), Exit: out.ExitInfra})
	}
	if e := unusable(h); e != nil {
		return nil, c.fail(o.JSON, e)
	}
	// With --json, ref and sha are in the report, so stderr only says why the
	// command may take a while; in text, one line says what goes where.
	extra := ""
	switch h.State {
	case host.Stopped:
		extra = " (waking it, ~20 s)"
		if o.JSON {
			fmt.Fprintf(c.Stderr, "waking %s…\n", h.DisplayName)
		}
	case host.Busy:
		extra = fmt.Sprintf(" (it is %s; waiting for it)", h.RawState)
		if o.JSON {
			fmt.Fprintf(c.Stderr, "%s is %s; waiting for it…\n", h.DisplayName, h.RawState)
		}
	}
	if !o.JSON {
		fmt.Fprintln(c.Stderr, sendingLine(refName(fullRef, o.Ref), sha, h.DisplayName, subdir, extra))
	}
	p, e := c.probe(ctx, h)
	if e != nil {
		return nil, c.fail(o.JSON, e)
	}

	id, err := job.NewID(h.Num, c.Clock.Now(), c.Entropy)
	if err != nil {
		return nil, c.fail(o.JSON, asError(err, out.CodeCacheError, "retry; if it persists, check that the system can provide random numbers"))
	}
	cmdStr := CommandString(o.Cmd)
	if subdir != "" {
		cmdStr = guardedCommand(subdir, sha, cmdStr)
	}
	req := runner.SubmitRequest{
		ID: id, Key: job.BranchKey(fullRef), SHA: sha, Kind: kind,
		Prio: o.Prio, Timeout: o.Timeout, Cmd: cmdStr,
	}
	req.Fingerprint = runner.Fingerprint(req)

	// dispatch.json is written before any connection carrying the job opens.
	// It keeps the command as typed, without the --cd guard, so that the
	// commands it suggests (rerun) are the ones the user would type.
	d := job.Dispatch{
		ID: id, Repo: c.Repo, Host: h.DisplayName, HostNum: h.Num, HostID: p.HostID, HostName: h.Name, Ref: fullRef,
		Key: req.Key, SHA: sha, Kind: req.Kind, Cmd: CommandString(o.Cmd), Dir: subdir, Fingerprint: req.Fingerprint,
		SubmittedAt: c.Clock.Now().UTC().Format(time.RFC3339),
	}
	if kind == runner.KindGen {
		// The patch of a gen job is applied to this work tree and no other.
		if top, terr := (detect.Git{Runner: c.Runner, Dir: c.Dir}).TopLevel(ctx); terr == nil {
			d.Top = top
		}
		d.AllowSensitive = allowSensitive
	}
	err = c.Cache.SaveDispatch(d)
	if err != nil {
		return nil, c.fail(o.JSON, &out.Error{Code: out.CodeCacheError, Message: "cannot write the job record: " + err.Error(),
			Fix: "check the permissions of " + c.Cache.Dir() + " (set GATEHAUL_CACHE_DIR to move it)", Exit: out.ExitInfra})
	}

	tmp, err := os.MkdirTemp("", "gatehaul-bundle-")
	if err != nil {
		_ = c.Cache.Remove(id)
		return nil, c.fail(o.JSON, &out.Error{Code: out.CodeBundleFailed, Message: "cannot create a temporary directory: " + err.Error(),
			Fix: "free some disk space, or set TMPDIR to a writable directory", Exit: out.ExitInfra})
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	api := c.API(h)
	var bases []string
	if !bundle.HostHas(p.Refs, sha) {
		bases = bundle.Bases(p.Refs, sha, func(b string) bool { return bundle.IsAncestor(ctx, c.Runner, c.Dir, b, sha) })
		if e := c.attachBundle(ctx, &req, id, bases, filepath.Join(tmp, "send.bundle")); e != nil {
			_ = c.Cache.Remove(id)
			return nil, c.fail(o.JSON, e)
		}
	}

	sub, err := api.Submit(ctx, req)
	if remoteCode(err) == runner.ErrCodeNeedFull && req.Bundle != nil {
		// The host lacks a prerequisite: send everything, under the same id.
		if e := c.attachBundle(ctx, &req, id, nil, filepath.Join(tmp, "full.bundle")); e != nil {
			_ = c.Cache.Remove(id)
			return nil, c.fail(o.JSON, e)
		}
		sub, err = api.Submit(ctx, req)
	}
	if err != nil {
		return nil, c.submitFailure(o.JSON, err, h, id)
	}
	state, perr := job.Parse(sub.State)
	if perr != nil {
		return nil, c.fail(o.JSON, &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s answered %v for job %s", h.DisplayName, perr, id),
			Fix: "update gatehaul, then run: gatehaul wait " + id, Exit: out.ExitInfra})
	}
	c.cacheWarn(c.Cache.SaveStatus(runner.JobStatus{Schema: runner.Schema, ID: id, State: string(state), SHA: sha, HostID: p.HostID}))

	d, err = c.Cache.LoadDispatch(id)
	if err != nil {
		return nil, c.fail(o.JSON, c.lookupError(err, id))
	}
	rs, e := c.waitJobs(ctx, []job.Dispatch{d}, true, o.Block)
	if e != nil {
		// The job exists: whatever went wrong, it keeps running, and running
		// the command again would make a second job. The only fix is to wait.
		fmt.Fprintf(c.Stderr, "next: gatehaul wait %s\n", id)
		oe := *e
		oe.Message += fmt.Sprintf(" (job %s was accepted and keeps running)", id)
		oe.Fix = "gatehaul wait " + id
		oe.ID = id
		return nil, c.fail(o.JSON, &oe)
	}
	return rs, 0
}

// submitFailure maps a failed submit. A lost answer is never retried blindly;
// every other failure means the host did not take the job, so its record goes.
func (c *Controller) submitFailure(asJSON bool, err error, h host.Host, id string) int {
	if isOutcomeUnknown(err) {
		fmt.Fprintf(c.Stderr, "next: gatehaul wait %s\n", id)
		return c.fail(asJSON, &out.Error{Code: out.CodeOutcomeUnknown,
			Message: fmt.Sprintf("the answer of %s was lost, so job %s may exist: %v", h.DisplayName, id, err),
			Fix:     "gatehaul wait " + id, Exit: out.ExitInfra, ID: id})
	}
	_ = c.Cache.Remove(id)
	return c.fail(asJSON, runnerError(err, h))
}

// attachBundle writes a bundle of req.SHA without bases to path and points
// req.Bundle at it.
func (c *Controller) attachBundle(ctx context.Context, req *runner.SubmitRequest, id string, bases []string, path string) *out.Error {
	if err := bundle.Create(ctx, c.Runner, c.Dir, id, req.SHA, bases, path); err != nil {
		return asError(err, out.CodeBundleFailed, "run git bundle create by hand to see the error; the commit must exist locally")
	}
	st, err := os.Stat(path)
	if err != nil {
		return asError(err, out.CodeBundleFailed, "check that the temporary directory is writable")
	}
	size := st.Size()
	req.Bundle = func() (io.ReadCloser, int64, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		return f, size, nil
	}
	return nil
}

// pickHost chooses the host asked for (want > 0) or the first running one,
// else the first stopped one, else the first one that is starting.
func pickHost(hosts []host.Host, want int) (host.Host, bool) {
	if want > 0 {
		for _, h := range hosts {
			if h.Num == want && h.State != host.NotFound {
				return h, true
			}
		}
		return host.Host{}, false
	}
	for _, st := range []host.State{host.Running, host.Stopped, host.Busy} {
		for _, h := range hosts {
			if h.State == st {
				return h, true
			}
		}
	}
	return host.Host{}, false
}

func listPaths(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}
