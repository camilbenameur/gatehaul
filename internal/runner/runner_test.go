package runner_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

func TestVersion(t *testing.T) {
	sum := sha256.Sum256(runner.Script())
	want := hex.EncodeToString(sum[:])[:12]
	if got := runner.Version(); got != want {
		t.Errorf("Version() = %q, want the first 12 hex digits of the script's sha256, %q", got, want)
	}
	if a, b := runner.Version(), runner.Version(); a != b {
		t.Errorf("Version() is not stable: %q then %q", a, b)
	}
}

func TestScriptIsACopy(t *testing.T) {
	s := runner.Script()
	s[0] = 'X'
	if runner.Script()[0] != '#' {
		t.Errorf("changing the result of Script() changed the embedded script")
	}
}

func TestScriptShape(t *testing.T) {
	s := runner.Script()
	if bytes.ContainsRune(s, '\r') {
		t.Errorf("runner.sh contains a carriage return")
	}
	for i, c := range s {
		if c >= 0x80 || (c < 0x20 && c != '\n') {
			t.Fatalf("runner.sh byte %d is not printable ASCII: %#x", i, c)
		}
	}
	text := string(s)
	if !strings.HasPrefix(text, "#!/usr/bin/env bash\n") {
		t.Errorf("runner.sh does not start with the bash shebang")
	}
	if !strings.Contains(text, "\nset -euo pipefail\n") {
		t.Errorf("runner.sh does not set -euo pipefail")
	}
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("runner.sh does not end with a newline")
	}
	if strings.Contains(text, "eval ") {
		t.Errorf("runner.sh must never eval")
	}
}

func TestScriptHandlesEverySubcommand(t *testing.T) {
	text := string(runner.Script())
	for _, c := range []string{
		runner.CmdProbe, runner.CmdSubmit, runner.CmdWait, runner.CmdLog, runner.CmdCancel, runner.CmdGC, "__job",
	} {
		want := "\n    " + c + ")"
		if c == "__job" {
			want = `"__job"`
		}
		if !strings.Contains(text, want) {
			t.Errorf("runner.sh has no case for subcommand %q", c)
		}
	}
}

func TestScriptKnowsEveryStateAndErrorCode(t *testing.T) {
	text := string(runner.Script())
	for _, s := range []job.State{
		job.Queued, job.Running, job.Succeeded, job.Failed, job.TimedOut, job.Cancelled, job.Lost, job.InfraFailed,
	} {
		if !strings.Contains(text, string(s)) {
			t.Errorf("runner.sh never mentions the state %q", s)
		}
	}
	for _, k := range []string{job.KindUser, job.KindInfra, job.KindLost} {
		if !strings.Contains(text, k) {
			t.Errorf("runner.sh never mentions the failure kind %q", k)
		}
	}
	// install_failed and noexec belong to the preamble.
	for _, c := range []string{
		runner.ErrCodeBadRequest, runner.ErrCodeUnknownJob, runner.ErrCodeConflict, runner.ErrCodeNeedFull,
		runner.ErrCodeBundleInvalid, runner.ErrCodeBundleTooLarge, runner.ErrCodeSHAMismatch,
		runner.ErrCodeMissingTool, runner.ErrCodeDiskFull,
	} {
		if !strings.Contains(text, c) {
			t.Errorf("runner.sh never answers with the error code %q", c)
		}
	}
}

func TestScriptSharesTheContractsLimitsAndPatterns(t *testing.T) {
	text := string(runner.Script())
	for _, want := range []string{
		"MAX_LOG_CHUNK=" + strconv.Itoa(runner.MaxLogChunk),
		"MAX_LOG_FILE=" + strconv.Itoa(runner.MaxLogFile),
		"MAX_BUNDLE_BYTES=" + strconv.FormatInt(int64(runner.MaxBundleBytes), 10),
		fmt.Sprintf("MAX_WAIT_S=%d", int(runner.MaxWaitSlice/time.Second)),
		"'" + runner.IDPattern.String() + "'",
		"'" + runner.KeyPattern.String() + "'",
		"'" + runner.SHAPattern.String() + "'",
		"SCHEMA=" + strconv.Itoa(runner.Schema),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runner.sh does not contain %q", want)
		}
	}
}

func TestPreamble(t *testing.T) {
	root := "/workspaces/.gatehaul"
	p := string(runner.Preamble(root))
	sum := sha256.Sum256(runner.Script())

	if !strings.HasPrefix(p, "gh1_preamble() {\n") {
		t.Errorf("the preamble does not start with the function definition:\n%s", p)
	}
	if !strings.HasSuffix(p, "\n}\ngh1_preamble\n") {
		t.Errorf("the preamble must end with the function and the line that calls it")
	}
	for _, want := range []string{
		"local root='" + root + "'",
		"ver='" + runner.Version() + "'",
		"want='" + hex.EncodeToString(sum[:]) + "'",
		"len=" + strconv.Itoa(len(runner.Script())),
		"GATEHAUL1 HELLO %s",
		"read -r -d '' -N",
		"sha256sum",
		"GATEHAUL1 BEGIN",
		"GATEHAUL1 END 1",
		runner.ErrCodeInstallFailed,
		runner.ErrCodeMissingTool,
		"exec env GATEHAUL_ROOT=",
		`"$root/runner-$ver.sh"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the preamble does not contain %q", want)
		}
	}
	// HELLO is the first thing the function prints.
	hello := strings.Index(p, "GATEHAUL1 HELLO")
	for _, later := range []string{"mkdir", "sha256sum", "read -r", "GATEHAUL1 BEGIN"} {
		if i := strings.Index(p, later); i < hello {
			t.Errorf("%q comes before HELLO in the preamble", later)
		}
	}
	if strings.ContainsRune(p, '\r') {
		t.Errorf("the preamble contains a carriage return")
	}
}

func TestPreambleChecksExecAndTheInstalledRunner(t *testing.T) {
	p := string(runner.Preamble("/workspaces/.gatehaul"))
	// A noexec mount has its own code; an installed runner is checked again and
	// replaced when wrong; the bytes are counted, not the characters; a failed
	// install leaves no .tmp file.
	for _, want := range []string{
		runner.ErrCodeNoExec,
		`got=$(sha256sum <"$runner")`,
		`IFS= LC_ALL=C read -r -d '' -N`,
		`trap 'rm -f -- "$gh1_tmp"' EXIT`,
		`"$probe" >/dev/null 2>&1`,
		`rm -f -- "$runner"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the preamble does not contain %q", want)
		}
	}
}

func TestScriptGuards(t *testing.T) {
	text := string(runner.Script())
	// git's messages are matched in English; no background gc keeps stdout open;
	// a probe that holds the job lock for a moment is waited for; a queued job
	// whose process is gone is launched again once; cancel waits for the job
	// shell before it writes exit.json; an unremovable worktree is moved aside.
	for _, want := range []string{
		"\nexport LC_ALL=C\n",
		"gc.auto=0",
		"flock -w 10 8",
		"relaunch_job",
		"wait_job_gone",
		".trash-",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runner.sh does not contain %q", want)
		}
	}
	if strings.Contains(text, "cap_log") {
		t.Errorf("the log is no longer cut after the fact: it would invalidate cursors")
	}
}

func TestPreambleQuoting(t *testing.T) {
	if p := string(runner.Preamble("/work space/a-b_c")); !strings.Contains(p, "local root='/work space/a-b_c'") {
		t.Errorf("a root with a space must be kept as is, single-quoted:\n%s", p)
	}
	for _, root := range []string{
		"/tmp/x'; touch pwned; '",
		"/tmp/x\ntouch pwned",
		"/tmp/x\rtouch pwned",
		"/tmp/x\x00y",
		"relative/path",
		"",
		"/",
		"/a/../b",
		"/a/..",
	} {
		p := string(runner.Preamble(root))
		if !strings.Contains(p, "local root='' ") {
			t.Errorf("root %q is not replaced by an empty root", root)
		}
		if strings.Contains(p, "pwned") || strings.Contains(p, "relative") {
			t.Errorf("root %q leaks into the preamble", root)
		}
	}
}
