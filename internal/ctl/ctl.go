// Package ctl is what the commands do on the laptop: choose a host, send a
// commit, wait for the job, read its log, create and stop hosts. Everything it
// touches is injected, so that it runs in tests without a terminal, a network
// or a clock.
package ctl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// Defaults in one place; they may change once M0's numbers are final.
const (
	DefaultIdleTimeout = 30 * time.Minute
	DefaultRetention   = 72 * time.Hour
	DefaultMaxHosts    = 2
	DefaultBlock       = 5 * time.Minute
	DefaultTimeout     = 30 * time.Minute
	DefaultPrio        = 5

	// CostNote is printed before gatehaul creates anything.
	CostNote = "each 2-core codespace uses 2 core-hours of your quota per hour while running"
)

// Timings. waitSlice stays under runner.MaxWaitSlice; multiSlice is used when
// several hosts are polled in turn; pollPause follows a pass that finished
// nothing; probeBudget covers waking a stopped codespace; tailBytes is the log
// tail asked with every finished job; maxUnknownWaits is how many cut wait
// streams in a row are tolerated; callMargin is what one wait call may take
// beyond its slice (waking a host, opening the connection).
const (
	waitSlice       = 50 * time.Second
	multiSlice      = 10 * time.Second
	pollPause       = time.Second
	probeBudget     = 3 * time.Minute
	tailBytes       = 8 << 10
	gitTimeout      = 30 * time.Second
	maxUnknownWaits = 3
	callMargin      = 30 * time.Second
)

// Config holds everything the controller needs from outside. Runner runs git
// and gh. API opens the runner of one host (default: runner.NewClient over the
// backend's transport). Entropy feeds job ids (default crypto/rand). Stdin
// answers the y/N prompt of "up" when IsTerminal says a person is there
// (default: nobody). Dir is a directory inside the repository (default ".")
// and Repo is owner/repo.
type Config struct {
	Runner     ghx.Runner
	Clock      clock.Clock
	Backend    host.Backend
	API        func(host.Host) runner.API
	Cache      *job.Cache
	Entropy    io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Stdin      io.Reader
	IsTerminal func() bool
	Dir        string
	Repo       string
}

// Controller runs the commands.
type Controller struct {
	Config
}

// New fills the defaults of cfg.
func New(cfg Config) *Controller {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Runner == nil {
		cfg.Runner = ghx.ExecRunner{}
	}
	if cfg.Entropy == nil {
		cfg.Entropy = rand.Reader
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.Stdin == nil {
		cfg.Stdin = strings.NewReader("")
	}
	if cfg.IsTerminal == nil {
		cfg.IsTerminal = func() bool { return false }
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.API == nil {
		backend, clk := cfg.Backend, cfg.Clock
		cfg.API = func(h host.Host) runner.API {
			return runner.NewClient(backend.Transport(h), backend.Root(h), clk)
		}
	}
	return &Controller{Config: cfg}
}

// DetectRepo returns owner/repo: the flag when given, else the git remote.
func DetectRepo(ctx context.Context, r ghx.Runner, dir, flagValue, ghHost string) (string, *out.Error) {
	if flagValue != "" {
		if !repoRE.MatchString(flagValue) {
			return "", &out.Error{Code: out.CodeUsage, Message: fmt.Sprintf("--repo %q is not of the form owner/repo", flagValue), Fix: "pass --repo owner/repo", Exit: out.ExitUsage}
		}
		return flagValue, nil
	}
	if dir == "" {
		dir = "."
	}
	g := detect.Git{Runner: r, Dir: dir}
	v, err := g.Repo(ctx, ghHost)
	switch {
	case err == nil:
		return v.Value, nil
	case errors.Is(err, detect.ErrNoGitHubRemote):
		return "", &out.Error{Code: out.CodeNoGitHubRemote, Message: "no git remote points to " + ghHost,
			Fix: "git remote add origin https://" + ghHost + "/OWNER/REPO.git, or pass --repo owner/repo", Exit: out.ExitInfra}
	case ghx.IsNotFound(err):
		return "", &out.Error{Code: out.CodeGitMissing, Message: "git is not installed", Fix: "install git " + detect.MinGitVersion.String() + " or later: https://git-scm.com", Exit: out.ExitInfra}
	case errors.Is(err, context.Canceled):
		return "", interrupted()
	}
	return "", &out.Error{Code: out.CodeNotGitRepo, Message: "cannot read the git remotes of " + dir + ": " + err.Error(),
		Fix: "cd into your project, or pass --repo owner/repo", Exit: out.ExitInfra}
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// CommandString turns the arguments after "--" into the command the host runs:
// one argument is a shell string, several are quoted and joined.
func CommandString(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fail prints e and returns its exit code (5 unless e says otherwise).
func (c *Controller) fail(asJSON bool, e *out.Error) int {
	if e.Exit == 0 {
		e.Exit = out.ExitInfra
	}
	out.WriteError(c.Stderr, asJSON, e)
	return e.Exit
}

func usageError(msg, fix string) *out.Error {
	return &out.Error{Code: out.CodeUsage, Message: msg, Fix: fix, Exit: out.ExitUsage}
}

func interrupted() *out.Error {
	return &out.Error{Code: out.CodeInterrupted, Message: "interrupted",
		Fix: "run the command again; a job that was already submitted keeps running: gatehaul wait", Exit: out.ExitInfra}
}

// asError turns err into an *out.Error, keeping one that already is.
func asError(err error, code out.Code, fix string) *out.Error {
	var oe *out.Error
	switch {
	case errors.As(err, &oe):
		return oe
	case errors.Is(err, context.Canceled):
		return interrupted()
	}
	return &out.Error{Code: code, Message: err.Error(), Fix: fix, Exit: out.ExitInfra}
}

// runnerError maps a failure of a runner call on host h.
func runnerError(err error, h host.Host) *out.Error {
	var re *runner.RemoteError
	var oe *out.Error
	switch {
	case errors.Is(err, context.Canceled):
		return interrupted()
	case host.IsPermanent(err) && errors.As(err, &oe):
		// The transport knows what is wrong (gh missing, expired login...).
		return oe
	case errors.Is(err, runner.ErrNotStarted), errors.Is(err, runner.ErrOutcomeUnknown), errors.Is(err, context.DeadlineExceeded):
		return &out.Error{Code: out.CodeHostUnreachable, Message: fmt.Sprintf("cannot reach %s: %v", h.DisplayName, err),
			Fix: "gatehaul doctor, then retry (a stopped codespace can take up to 3 minutes to wake)", Exit: out.ExitInfra}
	case errors.As(err, &re):
		switch re.Code {
		case runner.ErrCodeMissingTool:
			return &out.Error{Code: out.CodeMissingTool, Message: fmt.Sprintf("%s: %s", h.DisplayName, re.Message),
				Fix: installFix(h), Exit: out.ExitInfra}
		case runner.ErrCodeBundleTooLarge, runner.ErrCodeBundleInvalid, runner.ErrCodeSHAMismatch:
			return &out.Error{Code: out.CodeBundleFailed, Message: fmt.Sprintf("%s refused the bundle: %s", h.DisplayName, re.Message),
				Fix: "check the commit with git fsck and git bundle verify; if the history is huge, push a smaller base first", Exit: out.ExitInfra}
		case runner.ErrCodeConflict:
			return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s: %s", h.DisplayName, re.Message),
				Fix: "run gatehaul run again: a new job id is chosen", Exit: out.ExitInfra}
		case runner.ErrCodeNoExec:
			return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s: %s", h.DisplayName, re.Message),
				Fix: "gh codespace ssh -c " + h.Name + " -- sudo mount -o remount,exec /workspaces; or recreate the host: gatehaul down --delete, then gatehaul up", Exit: out.ExitInfra}
		case runner.ErrCodeDiskFull:
			return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s: %s", h.DisplayName, re.Message),
				Fix: "free disk space on the host: gh codespace ssh -c " + h.Name + " -- df -h /workspaces", Exit: out.ExitInfra}
		}
		return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s: %s: %s", h.DisplayName, re.Code, re.Message),
			Fix: "gatehaul doctor; if it persists, update gatehaul and recreate the host: gatehaul down --delete, then gatehaul up", Exit: out.ExitInfra}
	}
	return &out.Error{Code: out.CodeRunnerError, Message: fmt.Sprintf("%s: %v", h.DisplayName, err),
		Fix: "gatehaul doctor; if it persists, update gatehaul", Exit: out.ExitInfra}
}

func installFix(h host.Host) string {
	return "gh codespace ssh -c " + h.Name + " -- sudo apt-get install -y coreutils util-linux"
}

// remoteCode returns the code of a runner error answer, or "".
func remoteCode(err error) string {
	var re *runner.RemoteError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

func (c *Controller) discover(ctx context.Context) ([]host.Host, *out.Error) {
	hosts, err := c.Backend.Discover(ctx)
	if err != nil {
		return nil, asError(err, out.CodeGHError, "gh codespace list --repo "+c.Repo)
	}
	return hosts, nil
}

// hostLookup is what hostFor found for a dispatched job.
type hostLookup int

const (
	hostFound hostLookup = iota
	hostGone
	hostRecreated
)

// hostFor finds the host a job was dispatched to. The display name is not
// enough: a deleted and recreated gatehaul-1 has another codespace name, and a
// job of the old one died with it.
func hostFor(hosts []host.Host, d job.Dispatch) (host.Host, hostLookup) {
	for _, h := range hosts {
		if h.DisplayName != d.Host || h.State == host.NotFound {
			continue
		}
		if d.HostName != "" && h.Name != d.HostName {
			return h, hostRecreated
		}
		return h, hostFound
	}
	return host.Host{}, hostGone
}

// unusable explains why h cannot take a connection, or returns nil. Only the
// hosts in a state gatehaul cannot use are refused: a problem with one
// codespace never stops the commands that work on the others.
func unusable(h host.Host) *out.Error {
	switch h.State {
	case host.Failed:
		return &out.Error{Code: out.CodeHostState,
			Message: fmt.Sprintf("codespace %s (%s) is in state %q and cannot be used", h.DisplayName, h.Name, h.RawState),
			Fix:     "gh codespace delete -c " + h.Name + " (or gatehaul down --delete), then gatehaul up", Exit: out.ExitInfra}
	case host.Unknown:
		return &out.Error{Code: out.CodeHostState,
			Message: fmt.Sprintf("codespace %s (%s) is in state %q, which gatehaul does not know", h.DisplayName, h.Name, h.RawState),
			Fix:     "wait for it to settle (gh codespace list), or delete it: gh codespace delete -c " + h.Name, Exit: out.ExitInfra}
	}
	return nil
}

// checkRepo refuses a job that was dispatched from another repository: its
// display name may belong to a different gatehaul-<n> here.
func (c *Controller) checkRepo(d job.Dispatch) *out.Error {
	if d.Repo == c.Repo {
		return nil
	}
	return &out.Error{Code: out.CodeWrongRepo,
		Message: fmt.Sprintf("job %s was dispatched from %s, not from %s", d.ID, d.Repo, c.Repo),
		Fix:     "run the command again with --repo " + d.Repo + ", or from a clone of it", Exit: out.ExitUsage}
}

var requiredTools = []string{"flock", "setsid", "timeout", "base64", "sha256sum"}

// probe asks host h what it has. The budget covers waking a stopped codespace.
func (c *Controller) probe(ctx context.Context, h host.Host) (runner.Probe, *out.Error) {
	pctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	p, err := c.API(h).Probe(pctx)
	if err != nil {
		if ctx.Err() != nil {
			return p, interrupted()
		}
		return p, runnerError(err, h)
	}
	var missing []string
	for _, tool := range requiredTools {
		if !p.Tools[tool] {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return p, &out.Error{Code: out.CodeMissingTool,
			Message: fmt.Sprintf("%s lacks: %s", h.DisplayName, strings.Join(missing, ", ")),
			Fix:     installFix(h), Exit: out.ExitInfra}
	}
	return p, nil
}

// git runs git in the controller's directory.
func (c *Controller) git(ctx context.Context, args ...string) (ghx.Result, error) {
	argv := append([]string{"git", "-C", c.Dir}, args...)
	return c.Runner.Run(ctx, argv, nil, gitTimeout)
}

// commit returns the full commit id and the full ref name to send: HEAD or the
// given ref. A detached HEAD, a bare commit id and a ref that is not a branch
// all use the ref "HEAD" for the branch key.
func (c *Controller) commit(ctx context.Context, ref string) (sha, fullRef string, e *out.Error) {
	fullRef = "HEAD"
	if ref == "" {
		head, err := detect.Git{Runner: c.Runner, Dir: c.Dir}.Head(ctx)
		switch {
		case errors.Is(err, detect.ErrNoCommit):
			return "", "", &out.Error{Code: out.CodeNoCommit, Message: "the repository has no commit yet",
				Fix: "commit your work: gatehaul sends commits, never the working tree", Exit: out.ExitInfra}
		case err != nil:
			return "", "", asError(err, out.CodeNotGitRepo, "run git rev-parse HEAD by hand to see the error")
		}
		sha = head.Value
		if res, err := c.git(ctx, "symbolic-ref", "-q", "HEAD"); err == nil && res.OK() {
			if s := strings.TrimSpace(string(res.Stdout)); s != "" {
				fullRef = s
			}
		}
	} else {
		res, err := c.git(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return "", "", asError(err, out.CodeNotGitRepo, "run git rev-parse by hand to see the error")
		}
		if !res.OK() {
			return "", "", &out.Error{Code: out.CodeUsage, Message: fmt.Sprintf("%q does not name a commit", ref),
				Fix: "git branch -a, then gatehaul run <ref> -- <cmd>", Exit: out.ExitUsage}
		}
		sha = strings.TrimSpace(string(res.Stdout))
		if res, err := c.git(ctx, "rev-parse", "--symbolic-full-name", "--end-of-options", ref); err == nil && res.OK() {
			if s, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n"); strings.TrimSpace(s) != "" {
				fullRef = strings.TrimSpace(s)
			}
		}
	}
	if !runner.SHAPattern.MatchString(sha) {
		return "", "", &out.Error{Code: out.CodeNotGitRepo, Message: fmt.Sprintf("git returned %q, which is not a full commit id", sha),
			Fix: "gatehaul needs a SHA-1 repository: run git rev-parse HEAD by hand", Exit: out.ExitInfra}
	}
	return sha, fullRef, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
