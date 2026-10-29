package ghx

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExecRunnerRunsWithoutShell(t *testing.T) {
	// The go tool is always present where these tests run.
	res, err := ExecRunner{}.Run(context.Background(), []string{"go", "version"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || !strings.HasPrefix(string(res.Stdout), "go version") {
		t.Fatalf("unexpected result: exit %d, stdout %q", res.ExitCode, res.Stdout)
	}
}

func TestExecRunnerReportsExitCode(t *testing.T) {
	res, err := ExecRunner{}.Run(context.Background(), []string{"go", "no-such-go-subcommand"}, nil, 0)
	if err != nil {
		t.Fatalf("a non-zero exit must not be an error, got %v", err)
	}
	if res.OK() || len(res.Stderr) == 0 {
		t.Fatalf("want a failed exit with stderr, got exit %d", res.ExitCode)
	}
}

func TestExecRunnerNotFound(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), []string{"gatehaul-test-no-such-binary"}, nil, 0)
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false, want true", err)
	}
}

func TestCappedKeepsTheStart(t *testing.T) {
	c := &capped{max: 4}
	n, err := c.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("Write = %d, %v; want 6, nil", n, err)
	}
	if c.buf.String() != "abcd" || !c.truncated {
		t.Fatalf("kept %q, truncated %v", c.buf.String(), c.truncated)
	}
}

func TestFakeMatchesFirstPrefixAndRejectsTheRest(t *testing.T) {
	f := (&Fake{}).
		On([]string{"git", "remote", "get-url"}, Stdout("https://github.com/a/b\n"), nil).
		On([]string{"git", "remote"}, Stdout("origin\n"), nil)
	ctx := context.Background()

	res, err := f.Run(ctx, []string{"git", "remote", "get-url", "origin"}, nil, 0)
	if err != nil || string(res.Stdout) != "https://github.com/a/b\n" {
		t.Fatalf("get-url: %q, %v", res.Stdout, err)
	}
	res, err = f.Run(ctx, []string{"git", "remote"}, nil, 0)
	if err != nil || string(res.Stdout) != "origin\n" {
		t.Fatalf("remote: %q, %v", res.Stdout, err)
	}
	if _, err := f.Run(ctx, []string{"gh", "--version"}, nil, 0); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("unscripted call: err = %v, want ErrUnexpected", err)
	}
	if got := len(f.Calls()); got != 3 {
		t.Fatalf("recorded %d calls, want 3", got)
	}
}

func TestExecRunnerRunEnvAddsToTheEnvironment(t *testing.T) {
	env := []string{"GOPROXY=https://proxy.example.invalid"}
	res, err := ExecRunner{}.RunEnv(context.Background(), env, []string{"go", "env", "GOPROXY"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); !res.OK() || got != "https://proxy.example.invalid" {
		t.Fatalf("go env GOPROXY = %q (exit %d), want the value passed in env", got, res.ExitCode)
	}
}

func TestFakeRunEnvMatchesLikeRunAndRecordsTheEnv(t *testing.T) {
	f := (&Fake{}).On([]string{"git", "status"}, Stdout("clean\n"), nil)
	ctx := context.Background()

	res, err := f.RunEnv(ctx, []string{"GIT_INDEX_FILE=/tmp/x"}, []string{"git", "status", "-s"}, nil, 0)
	if err != nil || string(res.Stdout) != "clean\n" {
		t.Fatalf("RunEnv: %q, %v", res.Stdout, err)
	}
	if _, err := f.RunEnv(ctx, nil, []string{"gh", "--version"}, nil, 0); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("unscripted call: err = %v, want ErrUnexpected", err)
	}
	calls := f.EnvCalls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d env calls, want 2", len(calls))
	}
	if len(calls[0].Env) != 1 || calls[0].Env[0] != "GIT_INDEX_FILE=/tmp/x" || calls[0].Argv[0] != "git" {
		t.Fatalf("first env call = %+v", calls[0])
	}
	if got := len(f.Calls()); got != 2 {
		t.Fatalf("Calls() has %d entries, want 2 (RunEnv calls are included)", got)
	}
}
