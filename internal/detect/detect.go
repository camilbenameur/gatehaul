// Package detect infers everything gatehaul needs from the working directory,
// so that no configuration file is required (docs/SPEC.md §2). Every inferred value
// records where it came from, for "gatehaul doctor --json".
package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

// MinGitVersion is the oldest git that supports everything the runner and the
// controller use (worktrees, bundles, --end-of-options).
var MinGitVersion = ghx.Version{Major: 2, Minor: 31, Patch: 0}

const gitTimeout = 5 * time.Second

// Value is an inferred value and where it came from.
type Value struct {
	Value    string `json:"value"`
	Source   string `json:"source"`
	Fallback string `json:"fallback,omitempty"` // how to set it by hand
}

// GHHost returns the GitHub host gh talks to: $GH_HOST, or github.com.
func GHHost(getenv func(string) string) Value {
	if h := strings.TrimSpace(getenv("GH_HOST")); h != "" {
		return Value{Value: h, Source: "GH_HOST"}
	}
	return Value{Value: "github.com", Source: "default", Fallback: "set GH_HOST"}
}

// Git runs git in a directory.
type Git struct {
	Runner ghx.Runner
	Dir    string
}

func (g Git) run(ctx context.Context, args ...string) (ghx.Result, error) {
	argv := append([]string{"git", "-C", g.Dir}, args...)
	return g.Runner.Run(ctx, argv, nil, gitTimeout)
}

// Version returns the installed git version.
func (g Git) Version(ctx context.Context) (ghx.Version, error) {
	res, err := g.Runner.Run(ctx, []string{"git", "--version"}, nil, gitTimeout)
	if err != nil {
		return ghx.Version{}, err
	}
	v, ok := ghx.ParseVersion(string(res.Stdout))
	if !res.OK() || !ok {
		return ghx.Version{}, fmt.Errorf("cannot read the git version from %q", strings.TrimSpace(string(res.Stdout)))
	}
	return v, nil
}

// ErrNotRepo means the directory is not inside a git work tree.
var ErrNotRepo = errors.New("not inside a git repository")

// TopLevel returns the root of the work tree containing Dir.
func (g Git) TopLevel(ctx context.Context) (string, error) {
	res, err := g.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", ErrNotRepo
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// ErrNoGitHubRemote means no remote points to the GitHub host. Repo returns it
// wrapped in a *NoGitHubRemoteError, which says what it found instead.
var ErrNoGitHubRemote = errors.New("no remote points to GitHub")

// Remote is a git remote that Repo looked at: its name, and the host of its
// URL ("" for a local path or a URL gatehaul cannot read). The URL itself is
// never kept: it may carry credentials.
type Remote struct {
	Name string
	Host string
}

// NoGitHubRemoteError is ErrNoGitHubRemote with what the checkout has instead:
// its remotes that do not point to GHHost, and the GitHub repositories of the
// remotes that do when there are several and none is origin or upstream.
type NoGitHubRemoteError struct {
	GHHost    string
	Others    []Remote
	Ambiguous []Value
}

func (e *NoGitHubRemoteError) Error() string {
	msg, _ := e.Explain()
	return msg
}

// Is makes errors.Is(err, ErrNoGitHubRemote) true.
func (e *NoGitHubRemoteError) Is(target error) bool { return target == ErrNoGitHubRemote }

// Explain returns the message and the Fix line of the error. The first fixes
// are read-only: the usual cause is gatehaul started from the wrong directory,
// and adding a GitHub remote to an unrelated repository is the wrong fix
// (docs/SPEC.md §2). "git remote add" is only mentioned for a checkout with no
// remote at all, as a step for the user.
func (e *NoGitHubRemoteError) Explain() (message, fix string) {
	const readOnly = "run gatehaul from the checkout of the GitHub repository (cd into it, or -C <dir>), or pass --repo owner/repo"
	if len(e.Ambiguous) > 1 {
		var parts, repos []string
		for _, v := range e.Ambiguous {
			parts = append(parts, strings.TrimPrefix(v.Source, "git remote ")+" is "+v.Value)
			repos = append(repos, v.Value)
		}
		return fmt.Sprintf("several git remotes point to %s and none is origin or upstream (%s)", e.GHHost, strings.Join(parts, ", ")),
			"pass --repo with the repository you mean: --repo " + strings.Join(repos, " or --repo ")
	}
	if len(e.Others) == 0 {
		return "this checkout has no git remote, so its GitHub repository is unknown",
			readOnly + "; if this checkout really is that repository and only lacks a remote, adding one is for the user to do (git remote add origin https://" + e.GHHost + "/OWNER/REPO.git)"
	}
	var parts []string
	for i, r := range e.Others {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Others)-3))
			break
		}
		where := "a local path"
		if r.Host != "" {
			where = r.Host
		}
		parts = append(parts, r.Name+" is "+where)
	}
	return fmt.Sprintf("no git remote points to %s (%s)", e.GHHost, strings.Join(parts, ", ")), readOnly
}

// Repo returns "owner/repo" from the remotes: origin, then upstream, then the
// only remote on host. When there is none, the error is a
// *NoGitHubRemoteError.
func (g Git) Repo(ctx context.Context, host string) (Value, error) {
	res, err := g.run(ctx, "remote")
	if err != nil {
		return Value{}, err
	}
	if !res.OK() {
		return Value{}, ErrNotRepo
	}
	remotes := strings.Fields(string(res.Stdout))
	ordered := orderRemotes(remotes)

	var found []Value
	var others []Remote
	for _, name := range ordered {
		res, err := g.run(ctx, "remote", "get-url", name)
		if err != nil {
			return Value{}, err
		}
		if !res.OK() {
			others = append(others, Remote{Name: name})
			continue
		}
		raw := strings.TrimSpace(string(res.Stdout))
		owner, repo, ok := ParseRemoteURL(raw, host)
		if !ok {
			others = append(others, Remote{Name: name, Host: RemoteHost(raw)})
			continue
		}
		v := Value{Value: owner + "/" + repo, Source: "git remote " + name, Fallback: "--repo owner/repo"}
		if name == "origin" || name == "upstream" {
			return v, nil
		}
		found = append(found, v)
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return Value{}, &NoGitHubRemoteError{GHHost: host, Others: others, Ambiguous: found}
}

// RemoteHost returns the host of a git remote URL, without user or port, or ""
// for a local path (including a Windows drive letter) or a URL it cannot read.
func RemoteHost(raw string) string {
	raw = strings.TrimSpace(raw)
	var h string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		h = u.Hostname()
	case strings.Contains(raw, ":"):
		h = raw[:strings.Index(raw, ":")]
		if at := strings.LastIndex(h, "@"); at >= 0 {
			h = h[at+1:]
		}
		if len(h) < 2 || strings.ContainsAny(h, `/\`) {
			return "" // C:\src\x, or ./a:b, are paths
		}
	default:
		return ""
	}
	for _, r := range h {
		if r <= ' ' || r == 0x7f {
			return ""
		}
	}
	return h
}

// orderRemotes puts origin first and upstream second, keeping the others.
func orderRemotes(remotes []string) []string {
	var out, rest []string
	for _, preferred := range []string{"origin", "upstream"} {
		for _, r := range remotes {
			if r == preferred {
				out = append(out, r)
			}
		}
	}
	for _, r := range remotes {
		if r != "origin" && r != "upstream" {
			rest = append(rest, r)
		}
	}
	return append(out, rest...)
}

// ParseRemoteURL extracts owner and repo from a git remote URL on host, in
// any of the forms git accepts: https://host/o/r(.git), ssh://git@host/o/r,
// git@host:o/r.git.
func ParseRemoteURL(raw, host string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	var h, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false
		}
		h, path = u.Hostname(), u.Path
	case strings.Contains(raw, ":"):
		// scp-like syntax: [user@]host:path
		i := strings.Index(raw, ":")
		h, path = raw[:i], raw[i+1:]
		if at := strings.LastIndex(h, "@"); at >= 0 {
			h = h[at+1:]
		}
	default:
		return "", "", false
	}
	if !strings.EqualFold(h, host) {
		return "", "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ErrNoCommit means the repository has no commit yet.
var ErrNoCommit = errors.New("the repository has no commit")

// Head returns the commit that gatehaul would send.
func (g Git) Head(ctx context.Context) (Value, error) {
	res, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return Value{}, err
	}
	if !res.OK() {
		return Value{}, ErrNoCommit
	}
	return Value{Value: strings.TrimSpace(string(res.Stdout)), Source: "HEAD", Fallback: "pass a ref: gatehaul run <ref> -- <cmd>"}, nil
}

// Dirty returns the paths with uncommitted changes. They are never sent.
func (g Git) Dirty(ctx context.Context) ([]string, error) {
	ch, err := g.Changes(ctx)
	if err != nil {
		return nil, err
	}
	return ch.all, nil
}

// Changes splits the uncommitted paths of the work tree. Modified holds the
// tracked files that are modified, staged, deleted or renamed; Untracked the
// files git does not know. Neither is ever sent.
type Changes struct {
	Modified  []string
	Untracked []string
	all       []string // every path, in git status order
}

// Changes reads git status and splits its paths into modified and untracked.
func (g Git) Changes(ctx context.Context) (Changes, error) {
	res, err := g.run(ctx, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return Changes{}, err
	}
	if !res.OK() {
		return Changes{}, ErrNotRepo
	}
	var ch Changes
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) <= 3 {
			continue
		}
		p := line[3:]
		ch.all = append(ch.all, p)
		if strings.HasPrefix(line, "??") {
			ch.Untracked = append(ch.Untracked, p)
		} else {
			ch.Modified = append(ch.Modified, p)
		}
	}
	return ch, nil
}

// Gate is the test command inferred from the project files.
type Gate struct {
	Command string `json:"command"`
	Parser  string `json:"parser"`
	Source  string `json:"source"`
}

// DetectGate infers the gate from the files at the root of fsys, in a fixed
// order. ok is false when nothing is recognised.
func DetectGate(fsys fs.FS) (gate Gate, ok bool) {
	if exists(fsys, "gatehaul.toml") {
		return Gate{Command: "(from gatehaul.toml)", Parser: "(from gatehaul.toml)", Source: "gatehaul.toml"}, true
	}
	if exists(fsys, "go.mod") {
		return Gate{Command: "go test -json ./...", Parser: "go-test-json", Source: "go.mod"}, true
	}
	if hasNpmTest(fsys) {
		return Gate{Command: "npm test", Parser: "regex", Source: "package.json"}, true
	}
	if exists(fsys, "Cargo.toml") {
		return Gate{Command: "cargo test", Parser: "regex", Source: "Cargo.toml"}, true
	}
	if exists(fsys, "pyproject.toml") {
		return Gate{Command: "pytest --junitxml=.gatehaul/junit.xml", Parser: "junit", Source: "pyproject.toml"}, true
	}
	return Gate{}, false
}

// skipDirs are never searched for a gate: dependencies, build output, caches.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true,
	"venv": true, "__pycache__": true,
}

// FindGate is DetectGate for monorepos. It looks at the root first, then in
// the subdirectories, breadth first (so the shallowest wins) down to maxDepth
// levels, in the order fs.ReadDir returns them (lexical). Directories whose
// name starts with a dot, and dependency or build directories, are skipped.
// dir is the slash-separated path of the directory the gate was found in, ""
// for the root.
func FindGate(fsys fs.FS, maxDepth int) (gate Gate, dir string, ok bool) {
	if g, found := DetectGate(fsys); found {
		return g, "", true
	}
	level := []string{"."}
	for depth := 1; depth <= maxDepth; depth++ {
		var next []string
		for _, parent := range level {
			entries, err := fs.ReadDir(fsys, parent)
			if err != nil {
				continue
			}
			for _, e := range entries {
				name := e.Name()
				if !e.IsDir() || strings.HasPrefix(name, ".") || skipDirs[name] {
					continue
				}
				p := name
				if parent != "." {
					p = parent + "/" + name
				}
				sub, err := fs.Sub(fsys, p)
				if err != nil {
					continue
				}
				if g, found := DetectGate(sub); found {
					return g, p, true
				}
				next = append(next, p)
			}
		}
		level = next
	}
	return Gate{}, "", false
}

func exists(fsys fs.FS, name string) bool {
	st, err := fs.Stat(fsys, name)
	return err == nil && !st.IsDir()
}

func hasNpmTest(fsys fs.FS) bool {
	b, err := fs.ReadFile(fsys, "package.json")
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts["test"]) != ""
}

// DirFS is os.DirFS, kept here so callers do not import os just for it.
func DirFS(dir string) fs.FS { return os.DirFS(dir) }
