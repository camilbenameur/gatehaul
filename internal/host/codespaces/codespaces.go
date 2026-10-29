// Package codespaces is the host.Backend for GitHub Codespaces. The lifecycle
// goes through gh behind ghx.Runner; the transport streams "bash -s" through
// "gh codespace ssh". gh keeps the credentials: gatehaul never sees a token.
package codespaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// RootDir is the runner's state directory on every codespace. /workspaces is
// the only path GitHub documents as persistent across stop and start.
const RootDir = "/workspaces/.gatehaul"

const (
	listTTL       = 60 * time.Second // Discover reuses its answer for this long
	queryTimeout  = 30 * time.Second
	changeTimeout = 2 * time.Minute
	createTimeout = 5 * time.Minute
)

// Backend implements host.Backend for the codespaces of one repository.
type Backend struct {
	// Clock times the list cache; it defaults to the wall clock.
	Clock clock.Clock
	// GHHost is the GitHub host gh talks to, for the fixes in error messages.
	GHHost string

	r    ghx.Runner
	repo string

	mu       sync.Mutex
	cache    []host.Host
	cachedAt time.Time
	cached   bool
	sems     map[string]chan struct{} // one ssh call in flight per codespace
}

var _ host.Backend = (*Backend)(nil)

// New returns a backend for the codespaces of repo ("owner/repo").
func New(r ghx.Runner, repo string) *Backend {
	return &Backend{
		Clock:  clock.Real{},
		GHHost: detect.GHHost(os.Getenv).Value,
		r:      r,
		repo:   repo,
		sems:   map[string]chan struct{}{},
	}
}

// Root implements host.Backend.
func (b *Backend) Root(host.Host) string { return RootDir }

// Transport implements host.Backend.
func (b *Backend) Transport(h host.Host) host.Transport {
	return &transport{name: h.Name, ghHost: b.GHHost, sem: b.sem(h.Name)}
}

func (b *Backend) sem(name string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sems[name]
	if !ok {
		s = make(chan struct{}, 1)
		b.sems[name] = s
	}
	return s
}

// Invalidate drops the cached list, so that the next Discover asks gh again.
func (b *Backend) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache, b.cached = nil, false
}

// MapState maps a state word of gh codespace list to a host.State. A word
// that is not in the table is an error, never Running (docs/SPEC.md §4);
// Discover turns that error into host.Unknown for the one host concerned.
func MapState(raw string) (host.State, error) {
	switch raw {
	case "Available":
		return host.Running, nil
	case "Starting", "ShuttingDown", "Provisioning", "Rebuilding", "Queued", "Awaiting",
		"Exporting", "Updating", "Unavailable", "Created":
		return host.Busy, nil
	case "Shutdown", "Archived":
		return host.Stopped, nil
	case "Deleted", "Moved":
		return host.NotFound, nil
	case "Failed":
		return host.Failed, nil
	}
	return "", fmt.Errorf("unknown codespace state %q", raw)
}

// Discover implements host.Lifecycle: the codespaces of the repository whose
// display name is gatehaul-<n>, ordered by n. The answer is cached for 60 s.
func (b *Backend) Discover(ctx context.Context) ([]host.Host, error) {
	b.mu.Lock()
	if b.cached && b.Clock.Now().Sub(b.cachedAt) < listTTL {
		hosts := append([]host.Host(nil), b.cache...)
		b.mu.Unlock()
		return hosts, nil
	}
	b.mu.Unlock()

	list, err := ghx.ListHosts(ctx, b.r, b.repo)
	if err != nil {
		return nil, b.failure(ctx, "codespace list", err)
	}
	hosts := make([]host.Host, 0, len(list))
	byNum := map[int]string{}
	for _, c := range list {
		n, ok := host.ParseDisplayName(c.DisplayName)
		if !ok {
			continue // gatehaul-<something else>: not ours
		}
		if first, dup := byNum[n]; dup {
			// Two codespaces with one number: gatehaul cannot tell which one
			// holds its jobs, and would count them once against the cap.
			return nil, &out.Error{
				Code:    out.CodeHostState,
				Message: fmt.Sprintf("two codespaces are labelled %s: %s and %s", c.DisplayName, first, c.Name),
				Fix:     "delete the one you do not need: gh codespace delete -c " + c.Name + " (or -c " + first + ")",
				Exit:    out.ExitInfra,
			}
		}
		byNum[n] = c.Name
		// An unknown state word is an error, never Running (docs/SPEC.md §4),
		// but only for that host: the others stay usable.
		st, err := MapState(c.State)
		if err != nil {
			st = host.Unknown
		}
		used, _ := time.Parse(time.RFC3339, c.LastUsedAt)
		hosts = append(hosts, host.Host{
			Name: c.Name, DisplayName: c.DisplayName, Num: n,
			State: st, RawState: c.State, Machine: c.MachineName, LastUsed: used,
		})
	}
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Num < hosts[j].Num })

	b.mu.Lock()
	b.cache, b.cachedAt, b.cached = append([]host.Host(nil), hosts...), b.Clock.Now(), true
	b.mu.Unlock()
	return hosts, nil
}

// Create implements host.Lifecycle. Every option is passed to gh explicitly;
// an empty Branch becomes the repository's default branch and an empty Machine
// the smallest machine the repository allows.
func (b *Backend) Create(ctx context.Context, o host.CreateOptions) (host.Host, error) {
	repo := o.Repo
	if repo == "" {
		repo = b.repo
	}
	branch, machine := o.Branch, o.Machine
	var err error
	if branch == "" {
		if branch, err = b.defaultBranch(ctx, repo); err != nil {
			return host.Host{}, err
		}
	}
	if machine == "" {
		if machine, err = b.smallestMachine(ctx, repo); err != nil {
			return host.Host{}, err
		}
	}
	display := host.DisplayName(o.Num)
	res, err := b.gh(ctx, "codespace create", []string{
		"gh", "codespace", "create", "-R", repo, "-b", branch, "-m", machine, "-d", display,
		"--idle-timeout", FormatDuration(o.IdleTimeout),
		"--retention-period", FormatDuration(o.Retention),
		"--default-permissions",
	}, createTimeout)
	b.Invalidate()
	if err != nil {
		var oe *out.Error
		if errors.As(err, &oe) && oe.Code == out.CodeNetwork {
			// The request may have reached GitHub before the connection or the
			// clock gave out: the codespace may exist, and creating it again
			// would make a duplicate.
			return host.Host{}, &out.Error{
				Code:    out.CodeCreateUnknown,
				Message: "gh codespace create did not finish (" + oe.Message + "): " + display + " may have been created",
				Fix:     "check what exists: gh codespace list --repo " + repo + "; gatehaul up never creates a name that is listed, so run it again afterwards",
				Exit:    out.ExitInfra,
			}
		}
		return host.Host{}, err
	}
	name := lastLine(res.Stdout)
	if name == "" {
		return host.Host{}, &out.Error{
			Code:    out.CodeCreateFailed,
			Message: "gh codespace create printed no codespace name",
			Fix:     "check what was created: gh codespace list --repo " + repo,
			Exit:    out.ExitInfra,
		}
	}
	return host.Host{Name: name, DisplayName: display, Num: o.Num, State: host.Busy, RawState: "Provisioning", Machine: machine}, nil
}

// Stop implements host.Lifecycle.
func (b *Backend) Stop(ctx context.Context, h host.Host) error {
	_, err := b.gh(ctx, "codespace stop", []string{"gh", "codespace", "stop", "-c", h.Name}, changeTimeout)
	b.Invalidate()
	return err
}

// Delete implements host.Lifecycle.
func (b *Backend) Delete(ctx context.Context, h host.Host) error {
	_, err := b.gh(ctx, "codespace delete", []string{"gh", "codespace", "delete", "-c", h.Name, "--force"}, changeTimeout)
	b.Invalidate()
	return err
}

func (b *Backend) defaultBranch(ctx context.Context, repo string) (string, error) {
	res, err := b.gh(ctx, "repo view", []string{"gh", "repo", "view", repo, "--json", "defaultBranchRef"}, queryTimeout)
	if err != nil {
		return "", err
	}
	var v struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal(res.Stdout, &v); err != nil || v.DefaultBranchRef.Name == "" {
		return "", &out.Error{
			Code:    out.CodeCreateFailed,
			Message: "cannot read the default branch of " + repo,
			Fix:     "check that the repository has a commit: gh repo view " + repo,
			Exit:    out.ExitInfra,
		}
	}
	return v.DefaultBranchRef.Name, nil
}

func (b *Backend) smallestMachine(ctx context.Context, repo string) (string, error) {
	res, err := b.gh(ctx, "api machines", []string{"gh", "api", "repos/" + repo + "/codespaces/machines"}, queryTimeout)
	if err != nil {
		return "", err
	}
	var v struct {
		Machines []struct {
			Name   string `json:"name"`
			CPUs   int    `json:"cpus"`
			Memory int64  `json:"memory_in_bytes"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(res.Stdout, &v); err != nil {
		return "", &out.Error{
			Code:    out.CodeCreateFailed,
			Message: "cannot read the machine list of " + repo + ": " + err.Error(),
			Fix:     "pass the machine by hand: gatehaul up --machine NAME (names: gh api repos/" + repo + "/codespaces/machines)",
			Exit:    out.ExitInfra,
		}
	}
	best := -1
	for i, m := range v.Machines {
		if m.Name == "" {
			continue
		}
		if best < 0 || m.CPUs < v.Machines[best].CPUs ||
			(m.CPUs == v.Machines[best].CPUs && m.Memory < v.Machines[best].Memory) {
			best = i
		}
	}
	if best < 0 {
		return "", &out.Error{
			Code:    out.CodeCreateFailed,
			Message: "no codespace machine is available for " + repo,
			Fix:     "check the repository's codespace settings, or pass --machine NAME",
			Exit:    out.ExitInfra,
		}
	}
	return v.Machines[best].Name, nil
}

// gh runs one gh command and turns every failure into an *out.Error with a
// Fix, except the caller's own cancellation.
func (b *Backend) gh(ctx context.Context, op string, argv []string, timeout time.Duration) (ghx.Result, error) {
	res, err := b.r.Run(ctx, argv, nil, timeout)
	if err != nil {
		return res, b.failure(ctx, op, err)
	}
	if !res.OK() {
		return res, b.failure(ctx, op, &ghx.GHError{Args: op, Result: res})
	}
	return res, nil
}

// failure classifies err. The caller's cancellation passes through unchanged.
func (b *Backend) failure(ctx context.Context, op string, err error) error {
	var ge *ghx.GHError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &ge):
		code, fix := ghx.Classify(b.GHHost, string(ge.Result.Stderr))
		return &out.Error{Code: code, Message: ge.Error(), Fix: fix, Exit: out.ExitInfra}
	case ghx.IsNotFound(err):
		return &out.Error{Code: out.CodeGHMissing, Message: "gh is not installed", Fix: "install gh from https://cli.github.com, then run: gh auth login -h " + b.GHHost, Exit: out.ExitInfra}
	case errors.Is(err, context.DeadlineExceeded):
		return &out.Error{Code: out.CodeNetwork, Message: "gh " + op + " timed out", Fix: "check the network connection to " + b.GHHost + ", then retry", Exit: out.ExitInfra}
	}
	return &out.Error{Code: out.CodeGHError, Message: fmt.Sprintf("gh %s: %v", op, err), Fix: "run the gh command by hand to see the full error: gh " + op, Exit: out.ExitInfra}
}

// FormatDuration prints d the way gh reads it: "30m", "72h".
func FormatDuration(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
