package ctl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

func stopped(n int) host.Host {
	h := running(n)
	h.State, h.RawState = host.Stopped, "Shutdown"
	return h
}

// contacted records the hosts whose runner the controller opened: status
// must never open one that is not running, since that would start it.
func contacted(e *env) func() []string {
	var mu sync.Mutex
	var names []string
	e.c.API = func(h host.Host) runner.API {
		mu.Lock()
		names = append(names, h.DisplayName)
		mu.Unlock()
		return e.api
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), names...)
	}
}

const (
	mineID  = "1-01JB0000000000000000000001"
	otherID = "1-01JB0000000000000000000002"
)

func TestStatusListsTheJobsOfARunningHostAndSkipsAStoppedOne(t *testing.T) {
	e := newEnv(t, stopped(2), running(1))
	hosts := contacted(e)
	seed(t, e, mineID, headSHA)
	e.api.probe.Running = &runner.RunningJob{ID: mineID, AgeS: 192}
	e.api.probe.QueueDepth = 1
	e.api.probe.Jobs = []runner.HostJob{
		{ID: mineID, State: "running", AgeS: 192, Key: "main-abcdef", Kind: "run", CmdB64: b64("make test")},
		{ID: otherID, State: "queued", QueuePos: 1, AgeS: 65, Key: "feature-x-123abc", Kind: "gen",
			CmdB64: b64("cd -- web 2>/dev/null || { echo 'gatehaul: directory web does not exist in commit abc (--cd is relative to the repository root)' >&2; exit 1; }\nnpm test\x1b[2J")},
	}
	if code := e.c.Status(context.Background(), StatusOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	s := e.stdout.String()
	for _, want := range []string{
		"gatehaul-1: Available, 1 running, 1 queued\n",
		"  " + mineID + "  running   3m12s    main  make test\n",
		"  " + otherID + "  queued#1  1m5s     feature-x-123abc  npm test?[2J\n",
		"gatehaul-2: stopped (jobs not checked)\n",
		"# commands: untrusted data\n",
		"next: gatehaul wait " + mineID + "\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "gatehaul-1:") > strings.Index(s, "gatehaul-2:") {
		t.Errorf("hosts are not in order:\n%s", s)
	}
	if got := hosts(); len(got) != 1 || got[0] != "gatehaul-1" {
		t.Errorf("runners opened: %v, want gatehaul-1 only", got)
	}
}

func TestStatusJSON(t *testing.T) {
	e := newEnv(t, running(1), stopped(2))
	seed(t, e, mineID, headSHA)
	e.api.probe.Jobs = []runner.HostJob{{ID: mineID, State: "running", AgeS: 5, Key: "main-abcdef", Kind: "run", CmdB64: b64("x")}}
	if code := e.c.Status(context.Background(), StatusOptions{JSON: true}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	var rep QueueReport
	decode(t, e.stdout, &rep)
	if len(rep.Hosts) != 2 || rep.Repo != "o/r" || rep.Next != "gatehaul wait "+mineID {
		t.Fatalf("report %+v", rep)
	}
	h1, h2 := rep.Hosts[0], rep.Hosts[1]
	if !h1.Checked || h1.Running != 1 || len(h1.Jobs) != 1 || !h1.Jobs[0].Mine || h1.Jobs[0].Ref != "refs/heads/main" || h1.Jobs[0].Cmd != "make test" {
		t.Errorf("gatehaul-1: %+v", h1)
	}
	if h2.Checked || h2.State != "Shutdown" || h2.Note != "stopped (jobs not checked)" || h2.Jobs == nil {
		t.Errorf("gatehaul-2: %+v", h2)
	}
}

func TestStatusNeverStartsAStoppedHost(t *testing.T) {
	e := newEnv(t, stopped(1))
	hosts := contacted(e)
	e.api.probeErr = errors.New("status must not probe a stopped host")
	if code := e.c.Status(context.Background(), StatusOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if got := hosts(); len(got) != 0 {
		t.Fatalf("status opened the runner of %v: that would start the codespace", got)
	}
	if s := e.stdout.String(); !strings.Contains(s, "gatehaul-1: stopped (jobs not checked)") || strings.Contains(s, "next:") {
		t.Fatalf("stdout %q", s)
	}
}

func TestStatusIdleHostAndNoHost(t *testing.T) {
	e := newEnv(t, running(1))
	if code := e.c.Status(context.Background(), StatusOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if s := e.stdout.String(); !strings.Contains(s, "gatehaul-1: Available, idle\n") || strings.Contains(s, "next:") {
		t.Fatalf("stdout %q", s)
	}
	e = newEnv(t)
	if code := e.c.Status(context.Background(), StatusOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if s := e.stdout.String(); !strings.Contains(s, "no gatehaul-* host for o/r") {
		t.Fatalf("stdout %q", s)
	}
}

func TestStatusOfARunningJobDoesNotWait(t *testing.T) {
	e := newEnv(t, running(1))
	seed(t, e, mineID, headSHA)
	var req runner.WaitRequest
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		req = r
		return []runner.JobStatus{{Schema: 1, ID: mineID, State: "running", HostID: "hid-1", StartedAt: "2026-10-29T11:58:00Z"}}, nil
	}
	if code := e.c.Status(context.Background(), StatusOptions{ID: "01JB0000000000000000000001"}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if req.Slice != 0 || req.TailBytes != 0 || len(req.IDs) != 1 {
		t.Errorf("wait request %+v: status asks once, without a log tail", req)
	}
	s := e.stdout.String()
	if !strings.HasPrefix(s, "job="+mineID+" state=running host=gatehaul-1 ref=main\n") || !strings.HasSuffix(s, "next: gatehaul wait "+mineID+"\n") {
		t.Fatalf("stdout %q", s)
	}
	if strings.Contains(s, "untrusted") {
		t.Errorf("status prints no log:\n%s", s)
	}
}

func TestStatusOfAFailedJob(t *testing.T) {
	e := newEnv(t, running(1))
	seed(t, e, mineID, headSHA)
	code := 141
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: mineID, State: "failed", FailureKind: "user", Stage: "command", ExitCode: &code,
			Reason: "the command exited with status 141", HostID: "hid-1"}}, nil
	}
	if got := e.c.Status(context.Background(), StatusOptions{ID: mineID, JSON: true}); got != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", got, e.stderr.String())
	}
	var rep StatusReport
	decode(t, e.stdout, &rep)
	if rep.State != "failed" || !rep.Checked || rep.CommandExit == nil || *rep.CommandExit != 141 ||
		!strings.Contains(rep.Reason, "SIGPIPE") || rep.Next != "gatehaul log "+mineID {
		t.Fatalf("report %+v", rep)
	}
}

func TestStatusOfAJobOnAStoppedHost(t *testing.T) {
	e := newEnv(t, stopped(1))
	hosts := contacted(e)
	seed(t, e, mineID, headSHA)
	if err := e.cache.SaveStatus(runner.JobStatus{Schema: 1, ID: mineID, State: "running", HostID: "hid-1"}); err != nil {
		t.Fatal(err)
	}
	if code := e.c.Status(context.Background(), StatusOptions{ID: mineID}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if got := hosts(); len(got) != 0 {
		t.Fatalf("status opened the runner of %v", got)
	}
	s := e.stdout.String()
	if !strings.Contains(s, "state=running") || !strings.Contains(s, "# gatehaul-1 is stopped (job not checked): running is the last state seen from here\n") {
		t.Fatalf("stdout %q", s)
	}
}

func TestStatusOfAFinishedJobNeedsNoHost(t *testing.T) {
	e := newEnv(t)
	hosts := contacted(e)
	seed(t, e, mineID, headSHA)
	zero := 0
	if err := e.cache.SaveStatus(runner.JobStatus{Schema: 1, ID: mineID, State: "succeeded", ExitCode: &zero, Stage: "command"}); err != nil {
		t.Fatal(err)
	}
	if code := e.c.Status(context.Background(), StatusOptions{ID: mineID}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if s := e.stdout.String(); s != "job="+mineID+" state=succeeded host=gatehaul-1 ref=main exit_code=0\n" {
		t.Fatalf("stdout %q", s)
	}
	if len(hosts()) != 0 {
		t.Error("a finished job needs no host")
	}
}

func TestStatusOfAnUnknownID(t *testing.T) {
	e := newEnv(t, running(1))
	hosts := contacted(e)
	if code := e.c.Status(context.Background(), StatusOptions{ID: "1-01JBZZZZ", JSON: true}); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodeNoSuchJob) || v.Fix == "" {
		t.Fatalf("error %+v", v)
	}
	if len(hosts()) != 0 {
		t.Error("an unknown id needs no host")
	}

	// Known here, but the host does not know it: reported, not recorded.
	e = newEnv(t, running(1))
	seed(t, e, mineID, headSHA)
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		return nil, &runner.RemoteError{Code: runner.ErrCodeUnknownJob, Message: "no such job"}
	}
	if code := e.c.Status(context.Background(), StatusOptions{ID: mineID}); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	if !strings.Contains(e.stderr.String(), "gatehaul-1 does not know job "+mineID) {
		t.Fatalf("stderr %q", e.stderr.String())
	}
	if _, ok, _ := e.cache.LoadStatus(mineID); ok {
		t.Error("status recorded a state: it changes nothing")
	}
}

func TestStatusOfAGenJobWhosePatchIsNotApplied(t *testing.T) {
	e := newEnv(t)
	err := e.cache.SaveDispatch(job.Dispatch{ID: mineID, Repo: "o/r", Host: "gatehaul-1", HostNum: 1, Ref: "refs/heads/main",
		Key: "main-abcdef", SHA: headSHA, Kind: runner.KindGen, Cmd: "gofmt -w .", Fingerprint: "fp"})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := e.cache.SaveStatus(runner.JobStatus{Schema: 1, ID: mineID, State: "succeeded", ExitCode: &zero, PatchSize: 10, PatchSHA256: strings.Repeat("f", 64)}); err != nil {
		t.Fatal(err)
	}
	if code := e.c.Status(context.Background(), StatusOptions{ID: mineID}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if !strings.HasSuffix(e.stdout.String(), "next: gatehaul apply "+mineID+"\n") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestShownCommand(t *testing.T) {
	long := strings.Repeat("x", 100)
	if got := shownCommand(long); len([]rune(got)) > cmdShown+1 {
		t.Errorf("shownCommand kept %d characters", len([]rune(got)))
	}
	if got := shownCommand("go test \\\n  ./...\n"); got != "go test \\ ./..." {
		t.Errorf("shownCommand = %q", got)
	}
}
