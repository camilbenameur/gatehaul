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
	for _, cmd := range []string{"frobnicate", "gate", "status"} {
		code, _, stderr := call(cmd)
		if code != out.ExitUsage || !strings.Contains(stderr, "Fix: ") {
			t.Errorf("%s: code %d, stderr %q", cmd, code, stderr)
		}
	}
}

// These invocations are rejected before anything runs (no git, no gh).
func TestControllerCommandsRejectBadInvocations(t *testing.T) {
	cases := [][]string{
		{"run"},
		{"run", "--"},
		{"run", "main", "other", "--", "true"},
		{"gen"},
		{"gen", "--"},
		{"gen", "main", "other", "--", "true"},
		{"apply", "a", "b"},
		{"wait", "--any", "--all"},
		{"up", "-n", "0"},
		{"up", "stray"},
		{"down", "stray"},
		{"log", "a", "b"},
	}
	for _, args := range cases {
		code, _, stderr := call(args...)
		if code != out.ExitUsage || !strings.Contains(stderr, "Fix: ") {
			t.Errorf("%v: code %d, stderr %q", args, code, stderr)
		}
	}
	if code, _, _ := call("run", "--no-such-flag", "--", "true"); code != out.ExitUsage {
		t.Errorf("unknown flag: code %d", code)
	}
}

func TestHelpListsTheControllerCommands(t *testing.T) {
	_, stdout, _ := call("help")
	for _, want := range []string{"gatehaul run", "gatehaul gen", "gatehaul apply", "gatehaul wait", "gatehaul log", "gatehaul up", "gatehaul down"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestSplitDashAndInterspersedFlags(t *testing.T) {
	before, after, found := splitDash([]string{"main", "--host", "1", "--", "go", "test", "--", "x"})
	if !found || len(before) != 3 || len(after) != 4 || after[2] != "--" {
		t.Fatalf("splitDash: %v | %v | %v", before, after, found)
	}
	fs, c := newFlags("t", &bytes.Buffer{})
	n := fs.Int("host", 0, "")
	pos, err := parseInterspersed(fs, []string{"main", "--host", "2", "--json", "other"})
	if err != nil || *n != 2 || !*c.json || len(pos) != 2 || pos[0] != "main" || pos[1] != "other" {
		t.Fatalf("parseInterspersed: pos %v, host %d, err %v", pos, *n, err)
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
