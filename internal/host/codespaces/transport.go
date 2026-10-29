package codespaces

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/out"
)

const (
	stderrTail = 4 << 10         // bytes of stderr kept for ExecError
	waitDelay  = 3 * time.Second // grace for pipes a grandchild still holds open
)

// SSHArgv is the command line that opens "bash -s" on codespace name. The ssh
// options come after "--", then the remote command, which never changes: the
// request, the command to run and the payload all travel on stdin.
func SSHArgv(name string) []string {
	return []string{
		"gh", "codespace", "ssh", "-c", name, "--",
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "ServerAliveInterval=15",
		"bash", "-s",
	}
}

// transport streams one "gh codespace ssh" call. Only one call per codespace
// is in flight at a time, through sem.
type transport struct {
	name   string
	ghHost string
	sem    chan struct{}
}

// Exec implements host.Transport. A non-zero exit status of the remote script
// is not an error, because the runner reports its outcome inside stdout. It is
// one when nothing reached stdout (gh itself failed, such as an unreachable
// codespace) or when ssh exited with 255 (its own failure code), so that the
// message carries the end of stderr. A failure that waiting cannot cure (gh
// missing, an expired login, a deleted codespace) is marked Permanent.
func (t *transport) Exec(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return &host.ExecError{Op: "gh codespace ssh", Err: ctx.Err()}
	}

	argv := SSHArgv(t.name)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var written atomic.Int64
	cmd.Stdin = stdin
	cmd.Stdout = &countingWriter{w: stdout, n: &written}
	tail := &tailBuffer{max: stderrTail}
	cmd.Stderr = tail
	cmd.WaitDelay = waitDelay
	killTreeOnCancel(cmd)

	err := cmd.Run()
	if err == nil {
		return nil
	}
	execErr := func(e error) error {
		ee := &host.ExecError{Op: "gh codespace ssh", StderrTail: tail.String(), Err: e}
		if oe := t.permanent(err, tail.String()); oe != nil && written.Load() == 0 {
			ee.Err, ee.Permanent = oe, true
		}
		return ee
	}
	if ctx.Err() != nil {
		return &host.ExecError{Op: "gh codespace ssh", StderrTail: tail.String(), Err: ctx.Err()}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != 255 && written.Load() > 0 {
		return nil
	}
	return execErr(err)
}

// permanent classifies a failed gh call. It returns an error that says what to
// do when retrying cannot help, and nil when the failure may be transient (a
// codespace that is still waking, a dropped connection).
func (t *transport) permanent(err error, stderr string) *out.Error {
	ghHost := t.ghHost
	if ghHost == "" {
		ghHost = "github.com"
	}
	if ghx.IsNotFound(err) {
		return &out.Error{Code: out.CodeGHMissing, Message: "gh is not installed",
			Fix: "install gh from https://cli.github.com, then run: gh auth login -h " + ghHost, Exit: out.ExitInfra}
	}
	code, fix := ghx.Classify(ghHost, stderr)
	switch code {
	case out.CodeGHTooOld, out.CodeScopeMissing, out.CodeNotLoggedIn:
		return &out.Error{Code: code, Message: "gh codespace ssh: " + firstLine(stderr), Fix: fix, Exit: out.ExitInfra}
	}
	s := strings.ToLower(stderr)
	if strings.Contains(s, "codespace not found") || strings.Contains(s, "no codespace") || strings.Contains(s, "http 404") {
		return &out.Error{Code: out.CodeHostGone, Message: "codespace " + t.name + " no longer exists",
			Fix: "gh codespace list, then gatehaul up to create the missing gatehaul-<n> host", Exit: out.ExitInfra}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// countingWriter counts what reaches the underlying writer.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.buf)) }
