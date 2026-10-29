package ctl

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// serveGen makes the fake host finish every job as succeeded with this patch.
// statusSHA is the sha256 the host reports in the job's status; an honest host
// reports the sha256 of the bytes.
func serveGen(api *fakeAPI, data []byte, statusSHA string) {
	sum := sumHex(data)
	api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		var sts []runner.JobStatus
		for _, id := range r.IDs {
			sts = append(sts, runner.JobStatus{
				Schema: 1, ID: id, State: "succeeded", HostID: "hid-1",
				PatchSHA256: statusSHA, PatchSize: int64(len(data)),
			})
		}
		return sts, nil
	}
	api.patchFn = func(id string) (runner.PatchData, error) {
		return runner.PatchData{Schema: 1, ID: id, SHA256: sum, Size: int64(len(data)), DataB64: base64.StdEncoding.EncodeToString(data)}, nil
	}
}

func genOpts(cmd ...string) GenOptions {
	return GenOptions{RunOptions: runOpts(cmd...)}
}

// seedGen stores a dispatched gen job on gatehaul-1.
func seedGen(t *testing.T, e *env, id string) {
	t.Helper()
	seed(t, e, id, headSHA)
	d, err := e.cache.LoadDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	d.Kind = runner.KindGen
	if err := e.cache.SaveDispatch(d); err != nil {
		t.Fatal(err)
	}
}

type errJSON struct{ Code, Fix, ID string }

func TestGenSubmitsAGenJobAndReportsNoChanges(t *testing.T) {
	e := newEnv(t, running(1))
	serveGen(e.api, nil, sumHex(nil))
	if code := e.c.Gen(context.Background(), genOpts("gofmt", "-w", ".")); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if len(e.api.submits) != 1 || e.api.submits[0].Kind != runner.KindGen || e.api.submits[0].Cmd != "gofmt -w ." {
		t.Fatalf("submits %+v", e.api.submits)
	}
	d, err := e.cache.LoadDispatch(e.api.submits[0].ID)
	if err != nil || d.Kind != runner.KindGen {
		t.Fatalf("dispatch %+v, %v", d, err)
	}
	lines := strings.Split(e.stdout.String(), "\n")
	if !strings.HasPrefix(lines[0], "job=1-") || !strings.HasSuffix(lines[0], " state=succeeded exit=0") || lines[1] != "patch: no changes" {
		t.Fatalf("stdout %q", e.stdout.String())
	}
	if e.api.patchCalls != 0 {
		t.Fatalf("%d patch fetches for an empty patch", e.api.patchCalls)
	}
}

func TestGenJSONReportsAnEmptyPatch(t *testing.T) {
	e := newEnv(t, running(1))
	serveGen(e.api, nil, sumHex(nil))
	o := genOpts("true")
	o.JSON = true
	if code := e.c.Gen(context.Background(), o); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	var rep Report
	decode(t, e.stdout, &rep)
	p := rep.Jobs[0].Patch
	if p == nil || p.Applied || len(p.Files) != 0 || len(p.Problems) != 0 || len(p.SensitivePaths) != 0 {
		t.Fatalf("report %+v", rep.Jobs[0])
	}
	for _, want := range []string{`"files": []`, `"sensitive_paths": []`, `"problems": []`, `"applied": false`} {
		if !strings.Contains(e.stdout.String(), want) {
			t.Errorf("stdout lacks %s:\n%s", want, e.stdout.String())
		}
	}
}

func TestGenFailedJobHasNoPatch(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "failed", FailureKind: "user", Stage: "command", HostID: "hid-1"}}, nil
	}
	if code := e.c.Gen(context.Background(), genOpts("false")); code != out.ExitRed {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(e.stdout.String(), "patch:") || e.api.patchCalls != 0 {
		t.Fatalf("a failed job has no patch: stdout %q, %d fetches", e.stdout.String(), e.api.patchCalls)
	}
	if !strings.Contains(e.stdout.String(), "next: gatehaul log 1-") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestGenStillRunningHasNoPatchYet(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("running")
	o := genOpts("sleep 1000")
	o.Block = 2 * time.Minute
	if code := e.c.Gen(context.Background(), o); code != out.ExitRunning {
		t.Fatalf("exit %d, want 3", code)
	}
	if !strings.Contains(e.stdout.String(), "next: gatehaul wait 1-") || strings.Contains(e.stdout.String(), "patch:") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

// wantMismatch runs gen on e and expects exit 5 with patch_mismatch and
// nothing kept.
func wantMismatch(t *testing.T, e *env) {
	t.Helper()
	o := genOpts("true")
	o.JSON = true
	if code := e.c.Gen(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5; stderr %q", code, e.stderr.String())
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodePatchMismatch) || v.Fix == "" || v.ID == "" {
		t.Fatalf("error %+v", v)
	}
	if _, ok, _ := e.cache.LoadPatch(v.ID); ok {
		t.Error("a patch that failed its checksum was kept")
	}
	if _, ok, _ := e.cache.LoadApplied(v.ID); ok {
		t.Error("a patch that failed its checksum was recorded as applied")
	}
	var rep Report
	decode(t, e.stdout, &rep)
	if rep.Exit != out.ExitInfra || rep.Jobs[0].Patch == nil || rep.Jobs[0].Patch.Applied || rep.Jobs[0].Code != string(out.CodePatchMismatch) {
		t.Fatalf("report %+v", rep)
	}
}

func TestGenPatchWhoseChecksumIsNotTheReportedOneIsAMismatch(t *testing.T) {
	data := []byte("diff --git a/f.txt b/f.txt\n")
	t.Run("status", func(t *testing.T) {
		e := newEnv(t, running(1))
		serveGen(e.api, data, sumHex([]byte("something else")))
		wantMismatch(t, e)
	})
	t.Run("bytes", func(t *testing.T) {
		e := newEnv(t, running(1))
		serveGen(e.api, data, sumHex(data))
		honest := e.api.patchFn
		e.api.patchFn = func(id string) (runner.PatchData, error) {
			pd, err := honest(id)
			pd.DataB64 = base64.StdEncoding.EncodeToString(append([]byte("x"), data...)) // changed in transit
			return pd, err
		}
		wantMismatch(t, e)
	})
}

func TestGenPatchTooLargeForTheHost(t *testing.T) {
	e := newEnv(t, running(1))
	serveGen(e.api, []byte("diff"), sumHex([]byte("diff")))
	e.api.patchFn = func(string) (runner.PatchData, error) {
		return runner.PatchData{}, &runner.RemoteError{Code: runner.ErrCodePatchTooLarge, Message: "patch is 9 MiB"}
	}
	o := genOpts("true")
	o.JSON = true
	if code := e.c.Gen(context.Background(), o); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stderr %q", code, e.stderr.String())
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodePatchTooLarge) || v.Fix == "" {
		t.Fatalf("error %+v", v)
	}
}

func TestGenPatchFetchFailureKeepsTheJobAndSaysHowToRetry(t *testing.T) {
	e := newEnv(t, running(1))
	serveGen(e.api, []byte("diff"), sumHex([]byte("diff")))
	e.api.patchFn = func(string) (runner.PatchData, error) {
		return runner.PatchData{}, &runner.RemoteError{Code: runner.ErrCodeUnknownJob, Message: "no such job"}
	}
	if code := e.c.Gen(context.Background(), genOpts("true")); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	if !strings.Contains(e.stdout.String(), "next: gatehaul apply 1-") || !strings.Contains(e.stderr.String(), "Fix: ") {
		t.Fatalf("stdout %q, stderr %q", e.stdout.String(), e.stderr.String())
	}
}

func TestApplyRefusesJobsThatHaveNoPatch(t *testing.T) {
	e := newEnv(t, running(1))
	runID, failedID, pendingID := "1-01JB0000000000000000000001", "1-01JB0000000000000000000002", "1-01JB0000000000000000000003"
	seed(t, e, runID, headSHA) // a run job
	seedGen(t, e, failedID)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: failedID, State: "failed"}); err != nil {
		t.Fatal(err)
	}
	seedGen(t, e, pendingID)
	e.api.waitFn = e.advanceWait("running")

	cases := []struct {
		id       string
		wantExit int
		wantCode out.Code
	}{
		{runID, out.ExitUsage, out.CodeNoPatch},
		{failedID, out.ExitRed, out.CodeNoPatch},
		{pendingID, out.ExitRunning, out.CodeJobNotDone},
	}
	for _, c := range cases {
		e.stderr.Reset()
		if code := e.c.Apply(context.Background(), ApplyOptions{ID: c.id, JSON: true}); code != c.wantExit {
			t.Errorf("apply %s: exit %d, want %d; stderr %q", c.id, code, c.wantExit, e.stderr.String())
			continue
		}
		var v errJSON
		decode(t, e.stderr, &v)
		if v.Code != string(c.wantCode) || v.Fix == "" {
			t.Errorf("apply %s: error %+v, want code %s", c.id, v, c.wantCode)
		}
	}
	if e.api.patchCalls != 0 {
		t.Errorf("%d patch fetches for jobs without a patch", e.api.patchCalls)
	}
}

func TestApplyWithoutIDNeedsAGenJobOfHead(t *testing.T) {
	e := newEnv(t, running(1))
	seed(t, e, "1-01JB0000000000000000000001", headSHA) // a run job is never chosen
	if code := e.c.Apply(context.Background(), ApplyOptions{JSON: true}); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodeNoSuchJob) || !strings.Contains(v.Fix, "gatehaul gen") {
		t.Fatalf("error %+v", v)
	}
}

func TestAppliedPatchIsNeverAppliedAgain(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seedGen(t, e, id)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: id, State: "succeeded", PatchSHA256: strings.Repeat("c", 64), PatchSize: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.cache.SaveApplied(id, job.Applied{SHA256: strings.Repeat("c", 64), Files: []string{"a.go", "b.go"}, AppliedAt: "2026-10-29T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() int{
		func() int { return e.c.Apply(context.Background(), ApplyOptions{ID: id}) },
		func() int { return e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}) },
	} {
		e.stdout.Reset()
		if code := call(); code != out.ExitOK {
			t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
		}
		if !strings.Contains(e.stdout.String(), "patch: already applied 2 files (sha256 cccccccccccc)\n") {
			t.Fatalf("stdout %q", e.stdout.String())
		}
	}
	if e.api.patchCalls != 0 {
		t.Fatalf("%d patch fetches for a patch that was applied", e.api.patchCalls)
	}
}
