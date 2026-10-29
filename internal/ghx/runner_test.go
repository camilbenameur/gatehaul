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
