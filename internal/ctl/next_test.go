package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// next: lines name gatehaul the way it can be run again: Config.Self, which
// main sets from detect.DetectSelf. The tests that expect "next: gatehaul …"
// cover the default, a gatehaul found on PATH.

func TestNextLinesUseTheFullPathWhenNotOnPath(t *testing.T) {
	const self = "/home/me/.local/bin/gatehaul"
	e := newEnv(t, running(1))
	e.c.Self = self
	e.api.waitFn = e.advanceWait("running")
	o := runOpts("sleep 1000")
	o.Block = 2 * time.Minute
	if code := e.c.Run(context.Background(), o); code != out.ExitRunning {
		t.Fatalf("exit %d, want 3", code)
	}
	id := e.api.submits[0].ID
	if !strings.Contains(e.stdout.String(), "\nnext: "+self+" wait "+id+"\n") {
		t.Fatalf("stdout %q", e.stdout.String())
	}

	e.stdout.Reset()
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "failed", FailureKind: "user", Stage: "command", HostID: "hid-1"}}, nil
	}
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitRed {
		t.Fatalf("wait exit %d, want 1", code)
	}
	if !strings.Contains(e.stdout.String(), "\nnext: "+self+" log "+id+"\n") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestNextLinesUseTheWrapperOverride(t *testing.T) {
	const self = "bash scripts/gatehaul.sh"
	e := newEnv(t, running(1))
	e.c.Self = self
	e.api.submitErrs = []error{fmt.Errorf("submit: %w", runner.ErrOutcomeUnknown)}
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	id := e.api.submits[0].ID
	first, rest, _ := strings.Cut(e.stderr.String(), "\n")
	if first != "next: "+self+" wait "+id {
		t.Fatalf("first stderr line %q", first)
	}
	var v struct{ Code, Fix string }
	if err := json.Unmarshal([]byte(rest), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rest)
	}
	if v.Code != "outcome_unknown" || v.Fix != self+" wait "+id {
		t.Fatalf("error %+v: the Fix line repeats the next: command", v)
	}

	// log --json carries the same prefix in its "next" field.
	e.stdout.Reset()
	e.api.logFn = func(r runner.LogRequest) (runner.LogChunk, error) {
		return runner.LogChunk{Schema: 1, ID: id, State: "running", DataB64: b64("x\n"), Cursor: 7, Size: 90, Truncated: true}, nil
	}
	if code := e.c.Log(context.Background(), LogOptions{ID: id, JSON: true}); code != out.ExitOK {
		t.Fatalf("log exit %d; stderr %q", code, e.stderr.String())
	}
	var lr LogReport
	decode(t, e.stdout, &lr)
	if lr.Next != self+" log "+id+" --since 7" {
		t.Fatalf("next = %q", lr.Next)
	}
}

func TestRerunUsesThePrefix(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	d, err := e.cache.LoadDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := rerun("C:/tools/gatehaul.exe", d); got != "C:/tools/gatehaul.exe run -- 'make test'" {
		t.Errorf("rerun = %q", got)
	}
}
