package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// seedHost stores a dispatched job of repo o/r on gatehaul-<num>, sent to the
// codespace called hostName.
func seedHost(t *testing.T, e *env, id string, num int, hostName string) {
	t.Helper()
	err := e.cache.SaveDispatch(job.Dispatch{
		ID: id, Repo: "o/r", Host: host.DisplayName(num), HostNum: num, HostID: "hid-1", HostName: hostName,
		Ref: "refs/heads/main", Key: "main-abcdef", SHA: headSHA, Kind: runner.KindRun, Cmd: "make test",
		Fingerprint: "fp", SubmittedAt: "2026-10-29T12:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// jsonAfterNoise decodes the JSON object in b, which may follow other text.
func jsonAfterNoise(t *testing.T, b *bytes.Buffer, v any) {
	t.Helper()
	s := b.String()
	i := strings.Index(s, "{")
	if i < 0 {
		t.Fatalf("no JSON in %q", s)
	}
	if err := json.Unmarshal([]byte(s[i:]), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
}

func jobByID(t *testing.T, rep Report, id string) JobReport {
	t.Helper()
	for _, j := range rep.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no job %s in %+v", id, rep)
	return JobReport{}
}

func TestWaitRefusesAJobOfAnotherRepository(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	err := e.cache.SaveDispatch(job.Dispatch{
		ID: id, Repo: "other/repo", Host: "gatehaul-1", HostNum: 1, HostID: "hid-1", Ref: "refs/heads/main",
		Key: "main-abcdef", SHA: headSHA, Kind: runner.KindRun, Cmd: "make", Fingerprint: "fp",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		t.Error("the gatehaul-1 of another repository was asked about the job")
		return nil, nil
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	if code != out.ExitUsage {
		t.Fatalf("exit %d, want 2", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "wrong_repo" || !strings.Contains(v.Fix, "--repo other/repo") {
		t.Fatalf("error %+v", v)
	}
	if e.cache.IsTerminal(id) {
		t.Error("the job was recorded as finished by a host that is not its own")
	}
}

func TestUpWithoutAnAnswerIsNotASuccess(t *testing.T) {
	e := newEnv(t)
	e.c.IsTerminal = func() bool { return true }
	e.c.Stdin = strings.NewReader("") // end of input: /dev/null, or a closed pipe
	if code := e.c.Up(context.Background(), UpOptions{N: 1, Max: 2, JSON: true}); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v struct{ Code, Fix string }
	jsonAfterNoise(t, e.stderr, &v)
	if v.Code != "confirmation_required" || v.Fix != "gatehaul up -n 1 --yes" {
		t.Fatalf("error %+v", v)
	}
	if len(e.be.created) != 0 {
		t.Fatal("a codespace was created without confirmation")
	}
}

func TestRefusedSubmitForgetsTheJobSoWaitIsNeverAmbiguous(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.submitErrs = []error{&runner.RemoteError{Code: runner.ErrCodeDiskFull, Message: "cannot store the bundle"}}
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	refused := e.api.submits[0].ID
	if _, err := e.cache.LoadDispatch(refused); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("the refused job is still in the cache: %v", err)
	}

	// A later run goes through; waiting without an id finds exactly that one.
	e.api.waitFn = e.advanceWait("running")
	o.Block = time.Minute
	if code := e.c.Run(context.Background(), o); code != out.ExitRunning {
		t.Fatalf("second run: exit %d, want 3", code)
	}
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Wait(context.Background(), WaitOptions{Block: time.Minute, JSON: true}); code != out.ExitOK {
		t.Fatalf("wait without an id: exit %d; stderr %q", code, e.stderr.String())
	}
}

func TestAcceptedJobWhoseWaitFailsKeepsItsID(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		return nil, fmt.Errorf("wait: %w", runner.ErrOutcomeUnknown)
	}
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	id := e.api.submits[0].ID
	first, rest, _ := strings.Cut(e.stderr.String(), "\n")
	if first != "next: gatehaul wait "+id {
		t.Fatalf("first stderr line %q", first)
	}
	var v struct{ Code, Fix, ID string }
	if err := json.Unmarshal([]byte(rest), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rest)
	}
	if v.Fix != "gatehaul wait "+id || v.ID != id {
		t.Fatalf("error %+v: the fix must be to wait on the job, never to run it again", v)
	}
}

func TestWaitCallThatRunsOutOfTimeLeavesTheJobPending(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		e.clk.Advance(r.Slice)
		return nil, fmt.Errorf("runner wait: %w", context.DeadlineExceeded)
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: 30 * time.Second, JSON: true})
	if code != out.ExitRunning {
		t.Fatalf("exit %d, want 3 (still pending); stderr %q", code, e.stderr.String())
	}
}

func TestWaitJobOfARecreatedCodespaceIsLostAndStaysLost(t *testing.T) {
	e := newEnv(t, running(1)) // its codespace is called cs-1
	id := "1-01JB0000000000000000000001"
	seedHost(t, e, id, 1, "cs-old")
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		t.Error("a host that is not the job's own was asked about it")
		return nil, nil
	}
	for i := 0; i < 2; i++ {
		e.stdout.Reset()
		code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
		var rep Report
		decode(t, e.stdout, &rep)
		if code != out.ExitInfra || rep.Jobs[0].State != "lost" || rep.Jobs[0].Code != "host_replaced" {
			t.Fatalf("wait %d: exit %d, report %+v", i+1, code, rep)
		}
	}
}

func TestWaitJobOfADeletedHostDoesNotFailTheOtherJobs(t *testing.T) {
	e := newEnv(t, running(2)) // gatehaul-1 no longer exists
	a, b := "1-01JB0000000000000000000001", "2-01JB0000000000000000000002"
	seedHost(t, e, a, 1, "cs-1")
	seedHost(t, e, b, 2, "cs-2")
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		if len(r.IDs) != 1 || r.IDs[0] != b {
			t.Errorf("waited on %v, want only %s", r.IDs, b)
		}
		return []runner.JobStatus{{Schema: 1, ID: b, State: "succeeded", HostID: "hid-1"}}, nil
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{a, b}, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, e.stdout, &rep)
	if code != out.ExitInfra {
		t.Fatalf("exit %d, want 5; stderr %q", code, e.stderr.String())
	}
	if ja := jobByID(t, rep, a); ja.State != "lost" || ja.Code != "host_gone" {
		t.Errorf("job of the deleted host: %+v", ja)
	}
	if jb := jobByID(t, rep, b); jb.State != "succeeded" {
		t.Errorf("job of the live host: %+v", jb)
	}
}

func TestUnknownJobOnTheSameHostIsRecordedAsFinished(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		return nil, &runner.RemoteError{Code: runner.ErrCodeUnknownJob, Message: "no such job"}
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var v struct{ Code string }
	decode(t, e.stderr, &v)
	if code != out.ExitInfra || v.Code != "no_such_job" {
		t.Fatalf("exit %d, error %+v", code, v)
	}
	if !e.cache.IsTerminal(id) {
		t.Error("the job stays pending forever, which makes a wait without an id ambiguous")
	}
}

func TestRunRecordsTheCodespaceName(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	d, err := e.cache.LoadDispatch(e.api.submits[0].ID)
	if err != nil || d.HostName != "cs-1" {
		t.Fatalf("dispatch %+v, %v", d, err)
	}
}

func TestRunKeepsTheTransportsOwnPermanentError(t *testing.T) {
	e := newEnv(t, running(1))
	cause := &out.Error{Code: out.CodeNotLoggedIn, Message: "gh codespace ssh: not logged in", Fix: "gh auth login -h github.com", Exit: out.ExitInfra}
	e.api.probeErr = fmt.Errorf("runner probe: %w", &host.ExecError{Op: "gh codespace ssh", Err: cause, Permanent: true})
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "not_logged_in" || v.Fix != "gh auth login -h github.com" {
		t.Fatalf("error %+v: the real cause and its fix must come out, not host_unreachable", v)
	}
}

func TestUpDoesNotRecreateAHostThatIsStillListed(t *testing.T) {
	deleting := running(2)
	deleting.State, deleting.RawState = host.NotFound, "Deleted"
	e := newEnv(t, running(1), deleting)
	code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, Yes: true, JSON: true})
	if code != out.ExitInfra || len(e.be.created) != 0 {
		t.Fatalf("exit %d, created %v", code, e.be.created)
	}
	var v struct{ Code string }
	decode(t, e.stderr, &v)
	if v.Code != "host_state" {
		t.Fatalf("error %+v", v)
	}
}

func TestUpReportsAFailedHostInsteadOfCountingIt(t *testing.T) {
	failed := running(1)
	failed.State, failed.RawState = host.Failed, "Failed"
	e := newEnv(t, failed)
	code := e.c.Up(context.Background(), UpOptions{N: 1, Max: 2, Yes: true, JSON: true})
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if code != out.ExitInfra || v.Code != "host_state" || !strings.Contains(v.Fix, "gh codespace delete -c cs-1") {
		t.Fatalf("exit %d, error %+v", code, v)
	}
}

func TestDownDeleteWakesAStoppedHostAndSkipsItWhenJobsAreQueued(t *testing.T) {
	stopped := running(2)
	stopped.State, stopped.RawState = host.Stopped, "Shutdown"
	e := newEnv(t, stopped)
	e.api.probe.QueueDepth = 1
	if code := e.c.Down(context.Background(), DownOptions{Delete: true}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if len(e.be.deleted) != 0 {
		t.Fatalf("deleted %v: jobs wait in its queue", e.be.deleted)
	}
	if !strings.Contains(e.stdout.String(), "gatehaul-2: skipped: 1 job(s) are queued") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestDownStopsAHostThatLacksATool(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.probe.Tools["flock"] = false
	if code := e.c.Down(context.Background(), DownOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d; stdout %q", code, e.stdout.String())
	}
	if len(e.be.stopped) != 1 || e.be.stopped[0] != "gatehaul-1" {
		t.Fatalf("stopped %v: a host with a missing tool must still be stoppable", e.be.stopped)
	}
}

func TestDownFailedHostIsOnlyDeletedOnRequest(t *testing.T) {
	failed := running(1)
	failed.State, failed.RawState = host.Failed, "Failed"
	e := newEnv(t, failed, running(2))
	if code := e.c.Down(context.Background(), DownOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if len(e.be.deleted) != 0 || len(e.be.stopped) != 1 || e.be.stopped[0] != "gatehaul-2" {
		t.Fatalf("deleted %v stopped %v", e.be.deleted, e.be.stopped)
	}
	e.be.stopped = nil
	if code := e.c.Down(context.Background(), DownOptions{Delete: true}); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if len(e.be.deleted) != 2 {
		t.Fatalf("deleted %v, want both hosts", e.be.deleted)
	}
}
