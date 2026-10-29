// Package detect infers everything gatehaul needs from the working directory,
// so that no configuration file is required (PLAN Â§2). Every inferred value
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

// ErrNoGitHubRemote means no remote points to the GitHub host.
var ErrNoGitHubRemote = errors.New("no remote points to GitHub")

// Repo returns "owner/repo" from the remotes: origin, then upstream, then the
// only remote on host.
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
	for _, name := range ordered {
		res, err := g.run(ctx, "remote", "get-url", name)
		if err != nil {
			return Value{}, err
		}
		if !res.OK() {
			continue
		}
		owner, repo, ok := ParseRemoteURL(strings.TrimSpace(string(res.Stdout)), host)
		if !ok {
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
	return Value{}, ErrNoGitHubRemote
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
	res, err := g.run(ctx, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, ErrNotRepo
	}
	var paths []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) > 3 {
			paths = append(paths, line[3:])
		}
	}
	return paths, nil
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
