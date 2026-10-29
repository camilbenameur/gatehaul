package job

import (
	"testing"

	"github.com/camilbenameur/gatehaul/internal/out"
)

// TestStateTable pins docs/SPEC.md §3: terminal states and exit codes.
func TestStateTable(t *testing.T) {
	cases := []struct {
		name     string
		state    State
		terminal bool
		exit     int
	}{
		{"queued", Queued, false, 3},
		{"running", Running, false, 3},
		{"succeeded", Succeeded, true, 0},
		{"failed", Failed, true, 1},
		{"timed_out", TimedOut, true, 1},
		{"cancelled", Cancelled, true, 1},
		{"lost", Lost, true, 5},
		{"infra_failed", InfraFailed, true, 5},
	}
	if len(cases) != len(info) {
		t.Fatalf("the table covers %d states, state.go defines %d", len(cases), len(info))
	}
	for _, c := range cases {
		got, err := Parse(c.name)
		if err != nil || got != c.state {
			t.Errorf("Parse(%q) = %q, %v; want %q", c.name, got, err, c.state)
			continue
		}
		if got.Terminal() != c.terminal {
			t.Errorf("%s: Terminal() = %v, want %v", c.name, got.Terminal(), c.terminal)
		}
		if got.ExitCode() != c.exit {
			t.Errorf("%s: ExitCode() = %d, want %d", c.name, got.ExitCode(), c.exit)
		}
	}
}

func TestExitCodesUseTheSharedConstants(t *testing.T) {
	if Succeeded.ExitCode() != out.ExitOK || Failed.ExitCode() != out.ExitRed ||
		Running.ExitCode() != out.ExitRunning || Lost.ExitCode() != out.ExitInfra {
		t.Fatal("state exit codes drifted from internal/out")
	}
}

func TestUnknownStateIsAnError(t *testing.T) {
	for _, s := range []string{"", "done", "Succeeded", "SUCCEEDED", "queued ", "exited"} {
		if st, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %q, want an error", s, st)
		}
	}
	var unknown State = "bogus"
	if unknown.Terminal() {
		t.Error("an unknown state must not be terminal")
	}
	if unknown.ExitCode() != out.ExitInfra {
		t.Errorf("an unknown state exits %d, want %d (infrastructure)", unknown.ExitCode(), out.ExitInfra)
	}
}
