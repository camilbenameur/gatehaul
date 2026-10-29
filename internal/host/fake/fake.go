// Package fake is a host.Backend for tests and CI: every host is a local
// directory, and its Transport runs a local "bash -s" there.
package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/camilbenameur/gatehaul/internal/host"
)

// stderrKeep is how much of the local bash's stderr goes into an error.
const stderrKeep = 2048

// Backend is an in-memory Lifecycle over local directories. Host n is
// "gatehaul-n"; its working directory is <dir>/<n> and its runner root is
// <dir>/<n>/.gatehaul.
type Backend struct {
	// Env is added to the environment of every remote bash ("KEY=value").
	Env []string
	// FailBeforeStart makes the next N Exec calls fail without running bash.
	FailBeforeStart int
	// CutAfter, when set, makes the next Exec stop copying stdout after the
	// first line that contains it, and fail: a connection lost after the
	// request was taken. It stays armed until a line matches, then clears.
	CutAfter string

	mu    sync.Mutex
	dir   string
	hosts map[int]host.State
	gens  map[int]int // how many times host n was created: part of its name
}

var _ host.Backend = (*Backend)(nil)

// New returns a Backend with hosts gatehaul-1 to gatehaul-hosts, all running.
func New(dir string, hosts int) *Backend {
	b := &Backend{dir: dir, hosts: map[int]host.State{}, gens: map[int]int{}}
	for n := 1; n <= hosts; n++ {
		b.hosts[n] = host.Running
		_ = os.MkdirAll(b.hostDir(n), 0o755)
	}
	return b
}

// SetEnv adds "KEY=value" entries to Env.
func (b *Backend) SetEnv(kv ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Env = append(b.Env, kv...)
}

// InjectFailBeforeStart sets FailBeforeStart.
func (b *Backend) InjectFailBeforeStart(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.FailBeforeStart = n
}

// InjectCutAfter sets CutAfter.
func (b *Backend) InjectCutAfter(substr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.CutAfter = substr
}

func (b *Backend) hostDir(n int) string { return filepath.Join(b.dir, strconv.Itoa(n)) }

func (b *Backend) describe(n int, st host.State) host.Host {
	raw := "Available"
	switch st {
	case host.Stopped:
		raw = "Shutdown"
	case host.Busy:
		raw = "Starting"
	case host.NotFound:
		raw = "NotFound"
	}
	return host.Host{
		Name:        fmt.Sprintf("fake-%d-%d", n, b.gens[n]),
		DisplayName: host.DisplayName(n),
		Num:         n,
		State:       st,
		RawState:    raw,
		Machine:     "fake",
		LastUsed:    time.Time{},
	}
}

// Discover implements host.Lifecycle. Deleted hosts are not listed.
func (b *Backend) Discover(ctx context.Context) ([]host.Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	nums := make([]int, 0, len(b.hosts))
	for n, st := range b.hosts {
		if st != host.NotFound {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	out := make([]host.Host, 0, len(nums))
	for _, n := range nums {
		out = append(out, b.describe(n, b.hosts[n]))
	}
	return out, nil
}

// Create implements host.Lifecycle: it adds host o.Num, or starts it again if
// it exists. A host that was deleted comes back with an empty directory and
// another name, like a recreated codespace.
func (b *Backend) Create(ctx context.Context, o host.CreateOptions) (host.Host, error) {
	if err := ctx.Err(); err != nil {
		return host.Host{}, err
	}
	if o.Num < 1 {
		return host.Host{}, fmt.Errorf("fake: invalid host number %d", o.Num)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.hosts[o.Num]; !ok || st == host.NotFound {
		b.gens[o.Num]++
		if err := os.RemoveAll(b.hostDir(o.Num)); err != nil {
			return host.Host{}, err
		}
	}
	if err := os.MkdirAll(b.hostDir(o.Num), 0o755); err != nil {
		return host.Host{}, err
	}
	b.hosts[o.Num] = host.Running
	return b.describe(o.Num, host.Running), nil
}

// Stop implements host.Lifecycle.
func (b *Backend) Stop(ctx context.Context, h host.Host) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.hosts[h.Num]; !ok || st == host.NotFound {
		return fmt.Errorf("fake: no host %s", h.DisplayName)
	}
	b.hosts[h.Num] = host.Stopped
	return nil
}

// Delete implements host.Lifecycle.
func (b *Backend) Delete(ctx context.Context, h host.Host) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.hosts[h.Num]; !ok || st == host.NotFound {
		return fmt.Errorf("fake: no host %s", h.DisplayName)
	}
	b.hosts[h.Num] = host.NotFound
	// Everything on the host goes with it.
	return os.RemoveAll(b.hostDir(h.Num))
}

// Root implements host.Backend.
func (b *Backend) Root(h host.Host) string {
	return filepath.Join(b.hostDir(h.Num), ".gatehaul")
}

// Transport implements host.Backend.
func (b *Backend) Transport(h host.Host) host.Transport {
	return &transport{b: b, num: h.Num}
}

type transport struct {
	b   *Backend
	num int
}

var errInjected = errors.New("injected failure before the remote script started")
var errCut = errors.New("injected connection loss")

// Exec implements host.Transport.
func (t *transport) Exec(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	b := t.b
	b.mu.Lock()
	state, known := b.hosts[t.num]
	fail := b.FailBeforeStart > 0
	if fail {
		b.FailBeforeStart--
	}
	// Like a real codespace, the first connection wakes a stopped host up, and
	// waits for one that is starting.
	if known && !fail && (state == host.Stopped || state == host.Busy) {
		b.hosts[t.num] = host.Running
		state = host.Running
	}
	// CutAfter stays armed until a line matches, so that a call that comes
	// first (a probe before a submit) does not use it up.
	cut := b.CutAfter
	if !known || state != host.Running || fail {
		cut = ""
	}
	env := append(os.Environ(), b.Env...)
	b.mu.Unlock()

	if !known || state == host.NotFound {
		return &host.ExecError{Op: "fake transport", Err: fmt.Errorf("host %s does not exist", host.DisplayName(t.num))}
	}
	if fail {
		return &host.ExecError{Op: "fake transport", Err: errInjected}
	}

	dir := b.hostDir(t.num)
	env = append(env, "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, ".config"))
	var errTail tail
	out := stdout
	var cw *cutWriter
	if cut != "" {
		cw = &cutWriter{w: stdout, substr: []byte(cut)}
		out = cw
	}
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = out
	cmd.Stderr = &errTail
	// A job that was detached closes its descriptors; this only bounds the
	// wait for a straggler that did not.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if cw != nil && cw.cut {
		b.mu.Lock()
		if b.CutAfter == cut {
			b.CutAfter = ""
		}
		b.mu.Unlock()
		return &host.ExecError{Op: "fake bash -s", StderrTail: errTail.String(), Err: errCut}
	}
	// Like the real transport, a cancelled call is an error, not a quiet exit.
	if cerr := ctx.Err(); err != nil && cerr != nil {
		return &host.ExecError{Op: "fake bash -s", StderrTail: errTail.String(), Err: cerr}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil // the runner reports its own outcome on stdout
	}
	if err != nil {
		return &host.ExecError{Op: "fake bash -s", StderrTail: errTail.String(), Err: err}
	}
	return nil
}

// cutWriter copies lines to w up to and including the first one that contains
// substr, then refuses everything.
type cutWriter struct {
	w      io.Writer
	substr []byte
	line   []byte
	cut    bool
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if c.cut {
		return 0, errCut
	}
	for i, ch := range p {
		c.line = append(c.line, ch)
		if ch != '\n' {
			continue
		}
		hit := bytes.Contains(c.line, c.substr)
		c.line = c.line[:0]
		if hit {
			if _, err := c.w.Write(p[:i+1]); err != nil {
				return 0, err
			}
			c.cut = true
			return len(p), errCut
		}
	}
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// tail keeps the last stderrKeep bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrKeep {
		t.buf = t.buf[len(t.buf)-stderrKeep:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
