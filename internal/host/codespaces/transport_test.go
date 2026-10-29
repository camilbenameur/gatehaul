package codespaces

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/out"
)

func TestSSHArgv(t *testing.T) {
	want := []string{
		"gh", "codespace", "ssh", "-c", "cs-1", "--",
		"-T", "-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=15", "bash", "-s",
	}
	if got := SSHArgv("cs-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHArgv:\n got %v\nwant %v", got, want)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := &tailBuffer{max: 10}
	for _, s := range []string{"0123456789", "abc", "defghijklmnop"} {
		if n, err := tb.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := tb.String(); got != "ghijklmnop" {
		t.Fatalf("tail = %q, want the last 10 bytes", got)
	}
}

func TestPermanentFailuresAreNotRetriedAndKeepTheirFix(t *testing.T) {
	tr := &transport{name: "cs-1", ghHost: "github.com", sem: make(chan struct{}, 1)}
	cases := []struct {
		name   string
		err    error
		stderr string
		want   out.Code // "" means transient
	}{
		{"gh missing", exec.ErrNotFound, "", out.CodeGHMissing},
		{"not logged in", errors.New("exit status 1"), "To get started with GitHub CLI, please run:  gh auth login", out.CodeNotLoggedIn},
		{"scope", errors.New("exit status 1"), "HTTP 403: Resource not accessible; needs the codespace scope", out.CodeScopeMissing},
		{"deleted codespace", errors.New("exit status 1"), "error getting codespace: codespace not found", out.CodeHostGone},
		{"waking", errors.New("exit status 255"), "ssh: connect to host: Connection refused", ""},
		{"nothing to say", errors.New("exit status 1"), "", ""},
	}
	for _, c := range cases {
		oe := tr.permanent(c.err, c.stderr)
		switch {
		case c.want == "" && oe != nil:
			t.Errorf("%s: classified as permanent (%s), want a retry", c.name, oe.Code)
		case c.want != "" && (oe == nil || oe.Code != c.want || oe.Fix == ""):
			t.Errorf("%s: got %+v, want code %s with a fix", c.name, oe, c.want)
		}
	}
}

func TestExecWithACancelledContextIsAnExecError(t *testing.T) {
	tr := &transport{name: "cs-1", sem: make(chan struct{}, 1)}
	tr.sem <- struct{}{} // another call is in flight
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tr.Exec(ctx, strings.NewReader(""), &strings.Builder{})
	var ee *host.ExecError
	if !errors.As(err, &ee) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a *host.ExecError wrapping context.Canceled", err)
	}
}
