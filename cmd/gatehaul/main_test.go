package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/out"
)

func call(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(context.Background(), args, &o, &e)
	return code, o.String(), e.String()
}

func TestVersion(t *testing.T) {
	code, stdout, _ := call("version")
	if code != out.ExitOK || !strings.HasPrefix(stdout, "gatehaul ") {
		t.Fatalf("version: code %d, stdout %q", code, stdout)
	}
	code, stdout, _ = call("version", "--json")
	if code != out.ExitOK || !strings.Contains(stdout, `"version"`) {
		t.Fatalf("version --json: code %d, stdout %q", code, stdout)
	}
}

func TestHelp(t *testing.T) {
	code, stdout, _ := call("help")
	if code != out.ExitOK || !strings.Contains(stdout, "gatehaul doctor") {
		t.Fatalf("help: code %d, stdout %q", code, stdout)
	}
}

func TestNoArgumentsIsUsage(t *testing.T) {
	if code, _, stderr := call(); code != out.ExitUsage || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("no args: code %d, stderr %q", code, stderr)
	}
}

func TestUnknownAndPlannedCommandsGiveAFix(t *testing.T) {
	for _, cmd := range []string{"frobnicate", "run", "wait"} {
		code, _, stderr := call(cmd)
		if code != out.ExitUsage || !strings.Contains(stderr, "Fix: ") {
			t.Errorf("%s: code %d, stderr %q", cmd, code, stderr)
		}
	}
}

func TestDoctorRejectsStrayArguments(t *testing.T) {
	if code, _, stderr := call("doctor", "extra"); code != out.ExitUsage || !strings.Contains(stderr, "Fix: ") {
		t.Fatalf("doctor extra: code %d, stderr %q", code, stderr)
	}
	if code, _, _ := call("doctor", "--no-such-flag"); code != out.ExitUsage {
		t.Fatalf("doctor --no-such-flag: code %d", code)
	}
}
