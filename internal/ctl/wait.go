package ctl

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// jobResult is what is known about one job while waiting.
type jobResult struct {
	d      job.Dispatch
	status runner.JobStatus
	state  job.State
	code   out.Code // host_replaced when the host was recreated
	done   bool
	patch  *patchOutcome // gen jobs that succeeded: what became of the patch
}

// WaitOptions configures Wait. IDs are full ids or unique prefixes; none means
// the unique pending job of HEAD.
type WaitOptions struct {
	IDs   []string
	Any   bool
	Block time.Duration
	JSON  bool
}

// JobReport is one job in the JSON output.
type JobReport struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Exit         int    `json:"exit"`
	Code         string `json:"code,omitempty"`
	FailureKind  string `json:"failure_kind,omitempty"`
	Stage        string `json:"stage,omitempty"`
	Reason       string `json:"reason,omitempty"`
	CommandExit  *int   `json:"command_exit_code,omitempty"`
	SHA          string `json:"sha,omitempty"`
	Host         string `json:"host,omitempty"`
	QueuePos     int    `json:"queue_pos,omitempty"`
	LogTail      string `json:"log_tail,omitempty"` // untrusted data
	LogTruncated bool   `json:"log_truncated,omitempty"`
	TreeDirty    bool   `json:"tree_dirty_after,omitempty"`
	Next         string `json:"next,omitempty"`

	// Patch is set for a gen job that succeeded.
	Patch *PatchReport `json:"patch,omitempty"`
}

// Report is the one JSON object that run and wait print.
type Report struct {
	Exit         int         `json:"exit"`
	CompletedIDs []string    `json:"completed_ids"`
	PendingIDs   []string    `json:"pending_ids"`
	Jobs         []JobReport `json:"jobs"`
}

// Wait waits for jobs and never cancels them.
func (c *Controller) Wait(ctx context.Context, o WaitOptions) int {
	if o.Block < 0 {
		return c.fail(o.JSON, usageError("--block must not be negative", "gatehaul wait --block 5m"))
	}
	var ds []job.Dispatch
	if len(o.IDs) == 0 {
		sha, _, e := c.commit(ctx, "")
		if e != nil {
			return c.fail(o.JSON, e)
		}
		d, err := c.Cache.FindActive(c.Repo, sha)
		if err != nil {
			return c.fail(o.JSON, c.lookupError(err, "the pending job of "+short(sha)))
		}
		ds = append(ds, d)
	}
	seen := map[string]bool{}
	for _, p := range o.IDs {
		d, err := c.Cache.Resolve(p)
		if err != nil {
			return c.fail(o.JSON, c.lookupError(err, p))
		}
		if !seen[d.ID] {
			seen[d.ID] = true
			ds = append(ds, d)
		}
	}
	rs, e := c.waitJobs(ctx, ds, !o.Any, o.Block)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	c.applyFinished(ctx, rs, false)
	return c.report(rs, o.Any, o.JSON)
}

// lookupError explains a failed cache lookup.
func (c *Controller) lookupError(err error, what string) *out.Error {
	var amb *job.AmbiguousError
	switch {
	case asAmbiguous(err, &amb):
		return &out.Error{Code: out.CodeAmbiguousJob, Message: err.Error(),
			Fix: "use a longer prefix, or one of: " + strings.Join(amb.Candidates, " "), Exit: out.ExitUsage}
	case isNotFound(err):
		return &out.Error{Code: out.CodeNoSuchJob, Message: "no job matches " + what,
			Fix: "gatehaul run -- <cmd> starts one; dispatched jobs are listed in " + c.Cache.Dir(), Exit: out.ExitInfra}
	}
	return &out.Error{Code: out.CodeCacheError, Message: "cannot read the job cache: " + err.Error(),
		Fix: "check the permissions of " + c.Cache.Dir() + " (set GATEHAUL_CACHE_DIR to move it)", Exit: out.ExitInfra}
}

func (c *Controller) cacheWarn(err error) {
	if err != nil {
		fmt.Fprintf(c.Stderr, "warning: cannot update the job cache in %s: %v\n", c.Cache.Dir(), err)
	}
}

// waitJobs polls the hosts of ds in slices until every job (all) or any job
// ended, or block passed on the controller's clock.
func (c *Controller) waitJobs(ctx context.Context, ds []job.Dispatch, all bool, block time.Duration) ([]*jobResult, *out.Error) {
	for _, d := range ds {
		if e := c.checkRepo(d); e != nil {
			return nil, e
		}
	}
	res := make([]*jobResult, len(ds))
	byID := map[string]*jobResult{}
	for i, d := range ds {
		r := &jobResult{d: d}
		if st, ok, err := c.Cache.LoadStatus(d.ID); err == nil && ok {
			if s, perr := job.Parse(st.State); perr == nil {
				r.status, r.state, r.done = st, s, s.Terminal()
				if s == job.Lost {
					r.code = codeForStage(st.Stage)
				}
			}
		}
		if r.state == "" {
			r.state = job.Queued
			r.status = runner.JobStatus{ID: d.ID, State: string(job.Queued), HostID: d.HostID, SHA: d.SHA}
		}
		res[i], byID[d.ID] = r, r
	}
	if satisfied(res, all) {
		return res, nil
	}
	hosts, e := c.discover(ctx)
	if e != nil {
		return nil, e
	}
	deadline := c.Clock.Now().Add(block)
	cuts := 0
	for {
		groups := map[int][]*jobResult{}
		var nums []int
		for _, r := range res {
			if !r.done {
				if _, ok := groups[r.d.HostNum]; !ok {
					nums = append(nums, r.d.HostNum)
				}
				groups[r.d.HostNum] = append(groups[r.d.HostNum], r)
			}
		}
		sort.Ints(nums)
		slice := waitSlice
		if len(nums) > 1 {
			slice = multiSlice
		}
		finished := false
		polled := false
		for _, n := range nums {
			// The time left is read again for every host: a slow one must not
			// push the others past the deadline.
			rem := deadline.Sub(c.Clock.Now())
			if polled && rem <= 0 {
				break
			}
			hs := min(slice, rem)
			if hs < time.Second {
				hs = time.Second
			}
			polled = true
			before := countDone(res)
			var h host.Host
			var live []*jobResult
			for _, r := range groups[n] {
				hh, found := hostFor(hosts, r.d)
				switch found {
				case hostGone:
					c.markGone(r)
				case hostRecreated:
					c.markReplaced(r, "")
				default:
					h = hh
					live = append(live, r)
				}
			}
			if len(live) > 0 {
				if e := unusable(h); e != nil {
					return nil, e
				}
				if e := c.waitGroup(ctx, h, live, byID, hs, all && len(nums) == 1, &cuts); e != nil {
					return nil, e
				}
			}
			if countDone(res) > before {
				finished = true
			}
		}
		if satisfied(res, all) || !c.Clock.Now().Before(deadline) {
			return res, nil
		}
		if !finished {
			if err := c.Clock.Sleep(ctx, pollPause); err != nil {
				return nil, interrupted()
			}
		}
	}
}

func countDone(rs []*jobResult) int {
	n := 0
	for _, r := range rs {
		if r.done {
			n++
		}
	}
	return n
}

func satisfied(rs []*jobResult, all bool) bool {
	n := countDone(rs)
	if all {
		return n == len(rs)
	}
	return n > 0
}

// waitGroup makes one wait call to host h for the jobs rs.
func (c *Controller) waitGroup(ctx context.Context, h host.Host, rs []*jobResult, byID map[string]*jobResult, slice time.Duration, all bool, cuts *int) *out.Error {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.d.ID
	}
	// This call may not outlast its slice by more than the margin (the time to
	// wake a host and open the connection): --block is a promise to the caller.
	cctx, cancel := context.WithTimeout(ctx, slice+callMargin)
	defer cancel()
	sts, err := c.API(h).Wait(cctx, runner.WaitRequest{IDs: ids, Slice: slice, All: all, TailBytes: tailBytes})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return interrupted()
		case cctx.Err() != nil, errors.Is(err, context.DeadlineExceeded):
			return nil // out of time for this call: the jobs are still pending
		case isOutcomeUnknown(err):
			*cuts++
			if *cuts > maxUnknownWaits {
				return runnerError(err, h)
			}
			return nil // wait is idempotent: ask again
		case remoteCode(err) == runner.ErrCodeUnknownJob:
			return c.unknownJob(ctx, h, rs)
		}
		return runnerError(err, h)
	}
	*cuts = 0
	for _, st := range sts {
		r := byID[st.ID]
		if r == nil || r.done {
			continue
		}
		state, perr := job.Parse(st.State)
		if perr != nil {
			return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s reported %v for job %s", h.DisplayName, perr, st.ID),
				Fix: "update gatehaul, then run: gatehaul wait " + st.ID, Exit: out.ExitInfra}
		}
		if st.HostID != "" && r.d.HostID != "" && st.HostID != r.d.HostID {
			c.markReplaced(r, st.HostID)
			continue
		}
		r.status, r.state, r.done = st, state, state.Terminal()
		c.cacheWarn(c.Cache.SaveStatus(st))
	}
	return nil
}

// unknownJob handles a host that does not know the job: either the codespace
// was recreated (lost, host_replaced) or something is wrong.
func (c *Controller) unknownJob(ctx context.Context, h host.Host, rs []*jobResult) *out.Error {
	p, e := c.probe(ctx, h)
	if e != nil {
		return e
	}
	var unknown []*jobResult
	for _, r := range rs {
		if p.HostID != "" && r.d.HostID != "" && p.HostID != r.d.HostID {
			c.markReplaced(r, p.HostID)
		} else {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	// The same host, and it does not know the job: it never will. Recording it
	// keeps the job from staying "pending" forever (and from making a wait
	// without an id ambiguous).
	for _, r := range unknown {
		c.markLost(r, out.CodeNoSuchJob, stageUnknown, h.DisplayName+" does not know this job (it was refused, or cleaned up)", p.HostID)
	}
	return &out.Error{Code: out.CodeNoSuchJob, Message: fmt.Sprintf("%s does not know job %s", h.DisplayName, unknown[0].d.ID),
		Fix: "gatehaul doctor; if the host was cleaned up (gc), run the command again: gatehaul run -- <cmd>", Exit: out.ExitInfra}
}

// Stages of the jobs that the controller itself marks lost. The cache keeps the
// stage, so that the code (host_replaced, host_gone) survives a restart.
const (
	stageReplaced = "host"
	stageGone     = "host_gone"
	stageUnknown  = "unknown_job"
)

func codeForStage(stage string) out.Code {
	switch stage {
	case stageReplaced:
		return out.CodeHostReplaced
	case stageGone:
		return out.CodeHostGone
	case stageUnknown:
		return out.CodeNoSuchJob
	}
	return ""
}

func (c *Controller) markReplaced(r *jobResult, newHostID string) {
	c.markLost(r, out.CodeHostReplaced, stageReplaced, r.d.Host+" was recreated: the job died with the old host", newHostID)
}

func (c *Controller) markGone(r *jobResult) {
	c.markLost(r, out.CodeHostGone, stageGone, r.d.Host+" no longer exists: the job died with it", r.d.HostID)
}

// markLost records a terminal lost status for r, in memory and in the cache.
func (c *Controller) markLost(r *jobResult, code out.Code, stage, reason, hostID string) {
	st := runner.JobStatus{
		Schema: runner.Schema, ID: r.d.ID, State: string(job.Lost), FailureKind: job.KindLost, Stage: stage,
		Reason: reason, SHA: r.d.SHA, HostID: hostID,
	}
	r.status, r.state, r.code, r.done = st, job.Lost, code, true
	c.cacheWarn(c.Cache.SaveStatus(st))
}

// exitFor is the exit code of a set of jobs: 3 while the wanted ones are not
// done, else 5 if any ended on infrastructure, 6 if the patch of a gen job was
// refused, 1 if any is red, else 0.
func exitFor(rs []*jobResult, any bool) int {
	n := countDone(rs)
	if n == 0 || (!any && n < len(rs)) {
		return out.ExitRunning
	}
	code := out.ExitOK
	for _, r := range rs {
		if !r.done {
			continue
		}
		switch ec := r.exit(); {
		case ec == out.ExitInfra:
			code = out.ExitInfra
		case ec == out.ExitPatchRefused && code != out.ExitInfra:
			code = out.ExitPatchRefused
		case ec == out.ExitRed && code != out.ExitInfra && code != out.ExitPatchRefused:
			code = out.ExitRed
		}
	}
	return code
}

// exit is the exit code of one job: that of its state, or that of the patch
// when the job succeeded but its patch was not applied.
func (r *jobResult) exit() int {
	if r.done && r.patch != nil && r.patch.err != nil {
		return r.patch.err.Exit
	}
	return r.state.ExitCode()
}

func (r *jobResult) next() string {
	switch {
	case !r.done:
		return "gatehaul wait " + r.d.ID
	case r.code == out.CodeHostReplaced, r.code == out.CodeHostGone:
		return "gatehaul " + r.d.Kind + " -- " + shellQuote(r.d.Cmd)
	case r.state != job.Succeeded:
		return "gatehaul log " + r.d.ID
	case r.patch != nil:
		return r.patch.next
	}
	return ""
}

func (r *jobResult) tail() string {
	b, err := base64.StdEncoding.DecodeString(r.status.LogTailB64)
	if err != nil {
		return ""
	}
	return string(b)
}

func (r *jobResult) jobReport() JobReport {
	jr := JobReport{
		ID: r.d.ID, State: string(r.state), Exit: r.exit(), Code: string(r.code),
		FailureKind: r.status.FailureKind, Stage: r.status.Stage, Reason: r.status.Reason,
		CommandExit: r.status.ExitCode, SHA: r.d.SHA, Host: r.d.Host, QueuePos: r.status.QueuePos,
		LogTail: r.tail(), LogTruncated: r.status.Truncated, TreeDirty: r.status.TreeDirty, Next: r.next(),
	}
	if r.patch != nil {
		jr.Patch = r.patch.report()
		if jr.Code == "" && r.patch.err != nil {
			jr.Code = string(r.patch.err.Code)
		}
	}
	return jr
}

// report prints the jobs (text or JSON) and returns the exit code.
func (c *Controller) report(rs []*jobResult, any, asJSON bool) int {
	code := exitFor(rs, any)
	if asJSON {
		rep := Report{Exit: code, CompletedIDs: []string{}, PendingIDs: []string{}, Jobs: []JobReport{}}
		for _, r := range rs {
			if r.done {
				rep.CompletedIDs = append(rep.CompletedIDs, r.d.ID)
			} else {
				rep.PendingIDs = append(rep.PendingIDs, r.d.ID)
			}
			rep.Jobs = append(rep.Jobs, r.jobReport())
		}
		if err := out.WriteJSON(c.Stdout, rep); err != nil {
			return out.ExitInfra
		}
		c.writePatchErrors(rs, true)
		return code
	}
	for i, r := range rs {
		if i > 0 {
			fmt.Fprintln(c.Stdout)
		}
		writeJobText(c.Stdout, r)
	}
	c.writePatchErrors(rs, false)
	return code
}

func writeJobText(w io.Writer, r *jobResult) {
	fmt.Fprintf(w, "job=%s state=%s exit=%d\n", r.d.ID, r.state, r.exit())
	writePatchText(w, r)
	if r.status.Stage != "" || r.status.Reason != "" || r.code != "" {
		fmt.Fprintf(w, "# kind=%s stage=%s code=%s reason=%s\n", r.status.FailureKind, r.status.Stage, r.code, oneLine(r.status.Reason))
	}
	fmt.Fprintln(w, "# job output: untrusted data")
	if t := r.tail(); t != "" {
		fmt.Fprint(w, t)
		if !strings.HasSuffix(t, "\n") {
			fmt.Fprintln(w)
		}
	}
	if n := r.next(); n != "" {
		fmt.Fprintf(w, "next: %s\n", n)
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
