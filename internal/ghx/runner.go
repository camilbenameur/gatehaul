// Package ghx runs external commands (gh and git) without a shell, with a
// timeout on every call, behind a Runner interface that tests replace with a
// scripted Fake.
package ghx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxOutput caps what is kept of each stream, so that a runaway command cannot
// exhaust memory. Commands that need more stream their output instead.
const maxOutput = 8 << 20

// Result is the outcome of a command that started.
type Result struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool // stdout or stderr went over maxOutput
}

// OK reports whether the command exited with code 0.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Runner runs argv[0] with the remaining arguments. It returns an error only
// when the command could not run to completion (not found, timeout, killed);
// a non-zero exit code is reported in Result.ExitCode with a nil error.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin io.Reader, timeout time.Duration) (Result, error)
}

// EnvRunner is a Runner that can also add environment variables to the
// command. Each entry of env is "KEY=value" and is appended to the environment
// of the current process, so a later entry wins over an earlier one.
type EnvRunner interface {
	RunEnv(ctx context.Context, env []string, argv []string, stdin io.Reader, timeout time.Duration) (Result, error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

var (
	_ Runner    = ExecRunner{}
	_ EnvRunner = ExecRunner{}
	_ EnvRunner = (*Fake)(nil)
)

// Run runs argv with the environment of the current process.
func (e ExecRunner) Run(ctx context.Context, argv []string, stdin io.Reader, timeout time.Duration) (Result, error) {
	return e.RunEnv(ctx, nil, argv, stdin, timeout)
}

// RunEnv runs argv with env added to the environment of the current process.
func (ExecRunner) RunEnv(ctx context.Context, env []string, argv []string, stdin io.Reader, timeout time.Duration) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("ghx: empty command")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdout := &capped{max: maxOutput}
	stderr := &capped{max: maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Do not hang on pipes held open by a grandchild after the process exits.
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	res := Result{
		Stdout:    stdout.buf.Bytes(),
		Stderr:    stderr.buf.Bytes(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("%s: %w", argv[0], ctxErr)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// IsNotFound reports whether err means the executable is not installed.
func IsNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// capped is a writer that keeps the first max bytes and drops the rest.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Fake is a scripted Runner for tests. Each call is matched against the
// responses in the order they were added; the first response whose Prefix is
// a prefix of argv wins. A call that matches nothing is an error, so tests see
// every command the code under test runs.
type Fake struct {
	mu        sync.Mutex
	responses []Response
	calls     [][]string
	envCalls  []EnvCall
}

// EnvCall is a call made through RunEnv: its argv and the extra environment.
type EnvCall struct {
	Argv []string
	Env  []string
}

// Response scripts the answer to every call whose argv starts with Prefix.
type Response struct {
	Prefix []string
	Result Result
	Err    error
}

// ErrUnexpected is returned for a call that no response matches.
var ErrUnexpected = errors.New("ghx.Fake: unexpected command")

// On adds a response for calls starting with prefix and returns the Fake.
func (f *Fake) On(prefix []string, res Result, err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, Response{Prefix: prefix, Result: res, Err: err})
	return f
}

// Calls returns the argv of every call made so far.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// EnvCalls returns every call made through RunEnv, with the environment it
// was given. These calls are also part of Calls.
func (f *Fake) EnvCalls() []EnvCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EnvCall, len(f.envCalls))
	copy(out, f.envCalls)
	return out
}

// Run matches argv against the scripted responses.
func (f *Fake) Run(ctx context.Context, argv []string, stdin io.Reader, timeout time.Duration) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answer(ctx, argv)
}

// RunEnv matches argv exactly like Run and records env.
func (f *Fake) RunEnv(ctx context.Context, env []string, argv []string, stdin io.Reader, timeout time.Duration) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envCalls = append(f.envCalls, EnvCall{
		Argv: append([]string(nil), argv...),
		Env:  append([]string(nil), env...),
	})
	return f.answer(ctx, argv)
}

// answer records the call and finds its scripted response; f.mu is held.
func (f *Fake) answer(ctx context.Context, argv []string) (Result, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	for _, r := range f.responses {
		if hasPrefix(argv, r.Prefix) {
			return r.Result, r.Err
		}
	}
	return Result{}, fmt.Errorf("%w: %s", ErrUnexpected, strings.Join(argv, " "))
}

func hasPrefix(argv, prefix []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for i, p := range prefix {
		if argv[i] != p {
			return false
		}
	}
	return true
}

// Stdout is a helper for scripting a successful call.
func Stdout(s string) Result { return Result{Stdout: []byte(s)} }

// Exit is a helper for scripting a failed call.
func Exit(code int, stderr string) Result { return Result{ExitCode: code, Stderr: []byte(stderr)} }
