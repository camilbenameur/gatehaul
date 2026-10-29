// Package host defines the two interfaces a sandbox backend implements
// (docs/SPEC.md §6): Transport, to run the runner on a host that exists,
// and Lifecycle, to find, create, stop and delete hosts.
//
// v0.1 has two backends: codespaces (gh codespace ssh) and fake (a local
// "bash -s" in a temporary directory, for tests and CI).
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Transport runs "bash -s" on one host. The remote command line is always the
// same; everything else (the runner, the request, the payload) goes through
// stdin, so that no user data is ever parsed by a remote shell's command line.
type Transport interface {
	// Exec feeds stdin to "bash -s" on the host and copies the remote stdout
	// to stdout. It returns when the remote process exits, the context ends or
	// the connection fails. A non-zero remote exit status is not an error:
	// the runner reports its own outcome inside stdout (internal/runner).
	//
	// Whether the remote script started is decided by the caller from the
	// runner's markers, not by the transport: some transports print noise
	// before the script runs.
	Exec(ctx context.Context, stdin io.Reader, stdout io.Writer) error
}

// ExecError is a transport failure, with the end of the remote or local
// stderr for the error message.
type ExecError struct {
	Op         string // what failed, such as "gh codespace ssh"
	StderrTail string
	Err        error
	// Permanent marks a failure that retrying cannot fix (gh missing, an
	// expired login, a deleted host). Err then says what to do, and callers stop
	// at once instead of retrying while the remote script "never started".
	Permanent bool
}

func (e *ExecError) Error() string {
	msg := e.Op + ": " + e.Err.Error()
	if t := strings.TrimSpace(e.StderrTail); t != "" {
		msg += ": " + t
	}
	return msg
}

func (e *ExecError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is an ExecError marked Permanent.
func IsPermanent(err error) bool {
	var ee *ExecError
	return errors.As(err, &ee) && ee.Permanent
}

// State is a backend-neutral host state. An unknown backend state is an
// error, never Running (docs/SPEC.md §4).
type State string

const (
	Running  State = "running"   // reachable now
	Busy     State = "busy"      // starting, stopping or provisioning: reachable soon
	Stopped  State = "stopped"   // the first Exec wakes it up
	NotFound State = "not_found" // deleted, or never created
	Failed   State = "failed"    // the backend gave up on it: delete it
	Unknown  State = "unknown"   // a state word the backend adapter does not know: never usable
)

// Host is one sandbox, labelled by its display name "gatehaul-<Num>".
type Host struct {
	Name        string // backend identifier, such as the codespace's random name
	DisplayName string // "gatehaul-<Num>": the label, and the source of Num
	Num         int    // first part of every job id run on this host
	State       State
	RawState    string // the backend's own word for the state, for messages
	Machine     string
	LastUsed    time.Time
}

// DisplayPrefix labels the hosts that gatehaul may use.
const DisplayPrefix = "gatehaul-"

// ParseDisplayName returns n for "gatehaul-<n>", with n ≥ 1.
func ParseDisplayName(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, DisplayPrefix)
	if !ok || rest == "" || rest[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// DisplayName returns "gatehaul-<n>".
func DisplayName(n int) string { return fmt.Sprintf("%s%d", DisplayPrefix, n) }

// CreateOptions are explicit on purpose: creating a host uses the user's
// quota, so nothing is left to the backend's defaults.
type CreateOptions struct {
	Num         int
	Repo        string // owner/repo
	Branch      string // the repository's default branch
	Machine     string // empty: the smallest machine the backend allows
	IdleTimeout time.Duration
	Retention   time.Duration
}

// Lifecycle finds and manages hosts. Discover never relies on a hard-coded
// name: it lists the hosts whose display name starts with DisplayPrefix.
type Lifecycle interface {
	Discover(ctx context.Context) ([]Host, error)
	Create(ctx context.Context, o CreateOptions) (Host, error)
	Stop(ctx context.Context, h Host) error
	Delete(ctx context.Context, h Host) error
}

// Backend is a Lifecycle that can also open a Transport to each of its hosts.
type Backend interface {
	Lifecycle
	Transport(h Host) Transport
	// Root is the runner's state directory on host h, such as
	// "/workspaces/.gatehaul" for every codespace.
	Root(h Host) string
}

// ErrNoHost means Discover found no host labelled gatehaul-*.
var ErrNoHost = errors.New("no gatehaul-* host")
