package ctl

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

func failedWith(code int, reason string) *jobResult {
	return &jobResult{
		d: job.Dispatch{ID: "1-X", Kind: runner.KindRun}, state: job.Failed, done: true,
		status: runner.JobStatus{FailureKind: job.KindUser, Stage: stageCommand, ExitCode: &code, Reason: reason},
	}
}

func TestSignalStatusesAreExplained(t *testing.T) {
	cases := []struct {
		code int
		want []string
	}{
		{141, []string{"status 141 (SIGPIPE:", `"| head" closed the pipe early`, "jobs run with pipefail", "--tail", "--grep"}},
		{137, []string{"status 137 (SIGKILL:", "out of memory", "timeout"}},
		{143, []string{"status 143 (SIGTERM:", "timeout"}},
		{130, []string{"status 130 (SIGINT"}},
		{139, []string{"status 139 (SIGSEGV"}},
		{158, []string{"status 158 (usually killed by signal 30)"}},
	}
	for _, c := range cases {
		r := failedWith(c.code, "the command exited with status "+strconv.Itoa(c.code))
		var b strings.Builder
		writeJobText(&b, r, "gatehaul")
		line := ""
		for l := range strings.SplitSeq(b.String(), "\n") {
			if strings.HasPrefix(l, "# kind=") {
				line = l
			}
		}
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("exit %d: reason line %q lacks %q", c.code, line, w)
			}
		}
		if got := r.jobReport("gatehaul").Reason; got != r.reason() || !strings.Contains(got, c.want[0]) {
			t.Errorf("exit %d: JSON reason %q", c.code, got)
		}
	}
}

func TestOrdinaryStatusesAreLeftAlone(t *testing.T) {
	for _, code := range []int{1, 2, 127, 128, 255} {
		r := failedWith(code, "the command exited with status "+strconv.Itoa(code))
		if got := r.reason(); got != r.status.Reason {
			t.Errorf("exit %d: reason %q, want it unchanged", code, got)
		}
	}
	// A timeout is reported by its own state, and an infrastructure failure is
	// not the command's status.
	to := failedWith(137, "the command ran over its timeout of 60 seconds")
	to.state = job.TimedOut
	infra := failedWith(141, "fetch failed")
	infra.status.FailureKind, infra.status.Stage = job.KindInfra, "fetch"
	for _, r := range []*jobResult{to, infra} {
		if got := r.reason(); got != r.status.Reason {
			t.Errorf("reason %q, want it unchanged", got)
		}
	}
}

func TestSignalNoteKeepsTheCleanupSuffix(t *testing.T) {
	r := failedWith(141, "the command exited with status 141; cleanup_unresolved")
	got := r.reason()
	if !strings.HasPrefix(got, "the command exited with status 141 (SIGPIPE:") || !strings.HasSuffix(got, "; cleanup_unresolved") {
		t.Errorf("reason = %q", got)
	}
}

// End to end through run: a pipeline cut by head fails with 141 and says why.
func TestRunExplainsSIGPIPE(t *testing.T) {
	e := newEnv(t, running(1))
	code := 141
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "failed", FailureKind: "user", Stage: "command",
			ExitCode: &code, Reason: "the command exited with status 141", HostID: "hid-1"}}, nil
	}
	if got := e.c.Run(context.Background(), runOpts("make lint 2>&1 | head -40")); got != out.ExitRed {
		t.Fatalf("exit %d, want 1", got)
	}
	if !strings.Contains(e.stdout.String(), "reason=the command exited with status 141 (SIGPIPE:") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}
