package ctl

import (
	"context"
	"encoding/base64"
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

// StatusOptions configures Status. An empty ID lists every host and its queue
// ("gatehaul queue").
type StatusOptions struct {
	ID   string
	JSON bool
}

// QueueReport is the JSON output of status without an id.
type QueueReport struct {
	Repo  string       `json:"repo"`
	Hosts []HostStatus `json:"hosts"`
	Next  string       `json:"next,omitempty"`
}

// HostStatus is one host in a QueueReport. Checked is false for a host that
// was not running: status never starts one, so its jobs are not known.
type HostStatus struct {
	Host       string      `json:"host"`
	Name       string      `json:"name"`
	State      string      `json:"state"` // the backend's own word, such as Available or Shutdown
	Checked    bool        `json:"checked"`
	Note       string      `json:"note,omitempty"`
	Running    int         `json:"running"`
	QueueDepth int         `json:"queue_depth"`
	Jobs       []QueuedJob `json:"jobs"`
	Code       string      `json:"code,omitempty"`
	Fix        string      `json:"fix,omitempty"`
}

// QueuedJob is an unfinished job of a host. Mine says this machine dispatched
// it, so that wait, log and apply work on it from here.
type QueuedJob struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	QueuePos int    `json:"queue_pos,omitempty"`
	AgeS     int64  `json:"age_s"`
	Ref      string `json:"ref,omitempty"` // the full ref when Mine, else the host's branch key
	Kind     string `json:"kind,omitempty"`
	Cmd      string `json:"cmd"` // untrusted data, cut to cmdShown characters
	Mine     bool   `json:"mine"`
}

// StatusReport is the JSON output of status with an id. Checked is false when
// the host was not asked (it is not running), and State is then the last one
// this machine saw.
type StatusReport struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Checked     bool   `json:"checked"`
	Note        string `json:"note,omitempty"`
	Host        string `json:"host"`
	Ref         string `json:"ref,omitempty"`
	SHA         string `json:"sha,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Dir         string `json:"dir,omitempty"`
	Cmd         string `json:"cmd,omitempty"`
	QueuePos    int    `json:"queue_pos,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	EndedAt     string `json:"ended_at,omitempty"`
	CommandExit *int   `json:"command_exit_code,omitempty"`
	FailureKind string `json:"failure_kind,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Code        string `json:"code,omitempty"`
	Next        string `json:"next,omitempty"`
}

// cmdShown is how much of a command status prints.
const cmdShown = 60

// Status reports the hosts and their queues, or one job, without waiting and
// without starting a stopped host: waking a codespace costs quota, and a
// stopped one runs nothing. It changes no job.
func (c *Controller) Status(ctx context.Context, o StatusOptions) int {
	if o.ID != "" {
		return c.jobStatus(ctx, o)
	}
	hosts, e := c.discover(ctx)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Num < hosts[j].Num })
	rep := QueueReport{Repo: c.Repo, Hosts: []HostStatus{}}
	exit := out.ExitOK
	var mine []string
	for _, h := range hosts {
		hs := HostStatus{Host: h.DisplayName, Name: h.Name, State: h.RawState, Jobs: []QueuedJob{}}
		if hs.State == "" {
			hs.State = string(h.State)
		}
		switch h.State {
		case host.Running:
			p, pe := c.probe(ctx, h)
			if pe != nil && pe.Code != out.CodeMissingTool {
				// A host that lacks a tool still answers the probe.
				hs.Note, hs.Code, hs.Fix = pe.Message, string(pe.Code), pe.Fix
				exit = out.ExitInfra
				break
			}
			hs.Checked, hs.QueueDepth = true, p.QueueDepth
			hs.Jobs = c.hostJobs(p)
			for _, j := range hs.Jobs {
				if j.State == string(job.Running) {
					hs.Running++
				}
				if j.Mine {
					mine = append(mine, j.ID)
				}
			}
		case host.Stopped:
			hs.Note = "stopped (jobs not checked)"
		case host.Busy:
			hs.Note = h.RawState + " (jobs not checked)"
		case host.Failed, host.Unknown:
			ue := unusable(h)
			hs.Note, hs.Code, hs.Fix = ue.Message+" (jobs not checked)", string(ue.Code), ue.Fix
		default:
			hs.Note = "it does not exist"
		}
		rep.Hosts = append(rep.Hosts, hs)
	}
	if len(mine) > 0 {
		rep.Next = c.command(append([]string{"wait"}, mine...)...)
	}
	if o.JSON {
		if err := out.WriteJSON(c.Stdout, rep); err != nil {
			return out.ExitInfra
		}
		return exit
	}
	writeQueueText(c.Stdout, c.Stderr, rep)
	return exit
}

// hostJobs turns what probe listed into report lines, with the ref and the
// command as typed for the jobs this machine dispatched.
func (c *Controller) hostJobs(p runner.Probe) []QueuedJob {
	jobs := []QueuedJob{}
	list := p.Jobs
	if len(list) == 0 && p.Running != nil {
		list = []runner.HostJob{{ID: p.Running.ID, State: string(job.Running), AgeS: p.Running.AgeS}}
	}
	for _, hj := range list {
		if !runner.IDPattern.MatchString(hj.ID) {
			continue
		}
		state := string(job.Queued)
		if hj.State == string(job.Running) {
			state = string(job.Running)
		}
		qj := QueuedJob{ID: hj.ID, State: state, QueuePos: hj.QueuePos, AgeS: hj.AgeS, Ref: hj.Key, Kind: safeText(hj.Kind)}
		if b, err := base64.StdEncoding.DecodeString(hj.CmdB64); err == nil {
			qj.Cmd = shownCommand(string(b))
		}
		if d, err := c.Cache.LoadDispatch(hj.ID); err == nil && d.Repo == c.Repo {
			qj.Mine, qj.Ref, qj.Kind = true, d.Ref, d.Kind
			qj.Cmd = shownCommand(d.Cmd)
			if d.Dir != "" {
				qj.Cmd = shownCommand("(in " + d.Dir + ") " + d.Cmd)
			}
		}
		jobs = append(jobs, qj)
	}
	return jobs
}

// shownCommand is the start of a command on one line, without the --cd guard
// line and without control characters.
func shownCommand(s string) string {
	if first, rest, ok := strings.Cut(s, "\n"); ok && strings.HasPrefix(first, "cd -- ") && strings.Contains(first, "--cd is relative to the repository root") {
		s = rest
	}
	r := []rune(safeText(oneLine(s)))
	if len(r) > cmdShown {
		return string(r[:cmdShown]) + "…"
	}
	return string(r)
}

func writeQueueText(w, ew io.Writer, rep QueueReport) {
	if len(rep.Hosts) == 0 {
		fmt.Fprintf(w, "no %s* host for %s\n", host.DisplayPrefix, rep.Repo)
	}
	for _, hs := range rep.Hosts {
		switch {
		case !hs.Checked:
			fmt.Fprintf(w, "%s: %s\n", hs.Host, hs.Note)
			if hs.Fix != "" {
				fmt.Fprintf(ew, "Fix: %s\n", hs.Fix)
			}
			continue
		case len(hs.Jobs) == 0:
			fmt.Fprintf(w, "%s: %s, idle\n", hs.Host, hs.State)
			continue
		}
		fmt.Fprintf(w, "%s: %s, %d running, %d queued\n", hs.Host, hs.State, hs.Running, hs.QueueDepth)
		for _, j := range hs.Jobs {
			state := j.State
			if j.QueuePos > 0 {
				state = fmt.Sprintf("queued#%d", j.QueuePos)
			}
			fmt.Fprintf(w, "  %s  %-9s %-8s %s  %s\n", j.ID, state, age(j.AgeS), refName(j.Ref, ""), j.Cmd)
		}
		if more := hs.QueueDepth - countQueued(hs.Jobs); more > 0 {
			fmt.Fprintf(w, "  … and %d more queued\n", more)
		}
	}
	fmt.Fprintln(w, "# commands: untrusted data")
	if rep.Next != "" {
		fmt.Fprintf(w, "next: %s\n", rep.Next)
	}
}

func countQueued(jobs []QueuedJob) int {
	n := 0
	for _, j := range jobs {
		if j.State == string(job.Queued) {
			n++
		}
	}
	return n
}

func age(s int64) string {
	if s < 0 {
		s = 0
	}
	return (time.Duration(s) * time.Second).String()
}

// jobStatus reports one job without waiting: from the cache when it ended,
// else from its host when that host is running.
func (c *Controller) jobStatus(ctx context.Context, o StatusOptions) int {
	d, err := c.Cache.Resolve(o.ID)
	if err != nil {
		return c.fail(o.JSON, c.lookupError(err, o.ID))
	}
	if e := c.checkRepo(d); e != nil {
		return c.fail(o.JSON, e)
	}
	r := &jobResult{d: d, state: job.Queued, status: runner.JobStatus{ID: d.ID, State: string(job.Queued)}}
	if st, ok, err := c.Cache.LoadStatus(d.ID); err == nil && ok {
		if s, perr := job.Parse(st.State); perr == nil {
			r.status, r.state, r.done = st, s, s.Terminal()
			if s == job.Lost {
				r.code = codeForStage(st.Stage)
			}
		}
	}
	checked, note := r.done, ""
	if !r.done {
		hosts, e := c.discover(ctx)
		if e != nil {
			return c.fail(o.JSON, e)
		}
		h, found := hostFor(hosts, d)
		switch {
		case found == hostGone:
			note, r.code = d.Host+" no longer exists: the job died with it", out.CodeHostGone
		case found == hostRecreated:
			note, r.code = d.Host+" was recreated: the job died with the old host", out.CodeHostReplaced
		case h.State != host.Running:
			state := h.RawState
			if h.State == host.Stopped {
				state = "stopped"
			}
			note = fmt.Sprintf("%s is %s (job not checked): %s is the last state seen from here", h.DisplayName, state, r.state)
		default:
			st, e := c.pollOnce(ctx, h, d)
			if e != nil {
				return c.fail(o.JSON, e)
			}
			s, perr := job.Parse(st.State)
			if perr != nil {
				return c.fail(o.JSON, &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s reported %v for job %s", h.DisplayName, perr, d.ID),
					Fix: "update gatehaul, then run: " + c.command("status", d.ID), Exit: out.ExitInfra})
			}
			if st.HostID != "" && d.HostID != "" && st.HostID != d.HostID {
				note, r.code = d.Host+" was recreated: the job died with the old host", out.CodeHostReplaced
			} else {
				r.status, r.state, r.done, checked = st, s, s.Terminal(), true
			}
		}
	}
	rep := c.statusReport(r, checked, note)
	if o.JSON {
		if err := out.WriteJSON(c.Stdout, rep); err != nil {
			return out.ExitInfra
		}
		return out.ExitOK
	}
	writeStatusText(c.Stdout, rep)
	return out.ExitOK
}

// pollOnce asks h for the state of d without waiting (a wait slice of 0).
func (c *Controller) pollOnce(ctx context.Context, h host.Host, d job.Dispatch) (runner.JobStatus, *out.Error) {
	cctx, cancel := context.WithTimeout(ctx, callMargin)
	defer cancel()
	sts, err := c.API(h).Wait(cctx, runner.WaitRequest{IDs: []string{d.ID}, Slice: 0, All: true, TailBytes: 0})
	switch {
	case err != nil && ctx.Err() != nil:
		return runner.JobStatus{}, interrupted()
	case remoteCode(err) == runner.ErrCodeUnknownJob:
		return runner.JobStatus{}, &out.Error{Code: out.CodeNoSuchJob, Message: h.DisplayName + " does not know job " + d.ID,
			Fix: "if the host was cleaned up (gc), run the command again: " + rerun(c.Self, d), Exit: out.ExitInfra, ID: d.ID}
	case err != nil:
		return runner.JobStatus{}, runnerError(err, h)
	}
	for _, st := range sts {
		if st.ID == d.ID {
			return st, nil
		}
	}
	return runner.JobStatus{}, &out.Error{Code: out.CodeRunnerError, Message: h.DisplayName + " did not report job " + d.ID,
		Fix: "update gatehaul, then run: " + c.command("status", d.ID), Exit: out.ExitInfra, ID: d.ID}
}

// statusReport builds the report of one job, with the command left to run:
// wait while it has not ended, log when it failed, apply for a gen job whose
// patch is not in the working tree yet, the command again when its host is
// gone.
func (c *Controller) statusReport(r *jobResult, checked bool, note string) StatusReport {
	st := r.status
	rep := StatusReport{
		ID: r.d.ID, State: string(r.state), Checked: checked, Note: note, Host: r.d.Host, Ref: r.d.Ref, SHA: r.d.SHA,
		Kind: r.d.Kind, Dir: r.d.Dir, Cmd: r.d.Cmd, QueuePos: st.QueuePos, StartedAt: st.StartedAt, EndedAt: st.EndedAt,
		CommandExit: st.ExitCode, FailureKind: st.FailureKind, Stage: st.Stage, Reason: r.reason(), Code: string(r.code),
	}
	switch {
	case r.code == out.CodeHostGone, r.code == out.CodeHostReplaced:
		rep.Next = rerun(c.Self, r.d)
	case !r.done:
		rep.Next = c.command("wait", r.d.ID)
	case r.state != job.Succeeded:
		rep.Next = c.command("log", r.d.ID)
	case r.d.Kind == runner.KindGen && st.PatchSize > 0:
		if _, ok, err := c.Cache.LoadApplied(r.d.ID); err == nil && !ok {
			rep.Next = c.command("apply", r.d.ID)
		}
	}
	return rep
}

func writeStatusText(w io.Writer, rep StatusReport) {
	line := fmt.Sprintf("job=%s state=%s host=%s", rep.ID, rep.State, rep.Host)
	if rep.Ref != "" {
		line += " ref=" + refName(rep.Ref, "")
	}
	if rep.QueuePos > 0 {
		line += fmt.Sprintf(" queue_pos=%d", rep.QueuePos)
	}
	if rep.CommandExit != nil {
		line += fmt.Sprintf(" exit_code=%d", *rep.CommandExit)
	}
	fmt.Fprintln(w, line)
	var f []string
	for _, kv := range [][2]string{
		{"kind", rep.FailureKind}, {"stage", rep.Stage}, {"code", rep.Code}, {"reason", oneLine(rep.Reason)},
	} {
		if kv[1] != "" && (rep.State != string(job.Succeeded) || kv[0] == "code") {
			f = append(f, kv[0]+"="+kv[1])
		}
	}
	if len(f) > 0 {
		fmt.Fprintf(w, "# %s\n", strings.Join(f, " "))
	}
	if rep.Note != "" {
		fmt.Fprintf(w, "# %s\n", rep.Note)
	}
	if rep.Next != "" {
		fmt.Fprintf(w, "next: %s\n", rep.Next)
	}
}
