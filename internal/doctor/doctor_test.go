package doctor

import (
	"bytes"
	"context"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/out"
)

const (
	dir     = "/w"
	sha     = "0123456789abcdef0123456789abcdef01234567"
	authOK  = `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octo","tokenSource":"keyring","scopes":"gist, read:org, repo, workflow, codespace"}]}}`
	hostsJS = `[{"name":"fluffy-x1","displayName":"gatehaul-1","state":"Available","repository":"acme/widget"},
	            {"name":"other-y2","displayName":"dev box","state":"Shutdown","repository":"acme/widget"}]`
)

type response struct {
	prefix []string
	res    ghx.Result
	err    error
}

func git(args ...string) []string { return append([]string{"git", "-C", dir}, args...) }

// scenario returns a Fake for a healthy machine, with overrides matched first.
func scenario(overrides ...response) *ghx.Fake {
	f := &ghx.Fake{}
	for _, o := range overrides {
		f.On(o.prefix, o.res, o.err)
	}
	f.On([]string{"gh", "--version"}, ghx.Stdout("gh version 2.102.0 (2026-09-30)\n"), nil)
	f.On([]string{"gh", "auth", "status"}, ghx.Stdout(authOK), nil)
	f.On([]string{"gh", "codespace", "list"}, ghx.Stdout(hostsJS), nil)
	f.On([]string{"git", "--version"}, ghx.Stdout("git version 2.47.1\n"), nil)
	f.On(git("rev-parse", "--show-toplevel"), ghx.Stdout(dir+"\n"), nil)
	f.On(git("rev-parse", "--verify"), ghx.Stdout(sha+"\n"), nil)
	f.On(git("remote", "get-url", "origin"), ghx.Stdout("git@github.com:acme/widget.git\n"), nil)
	f.On(git("remote"), ghx.Stdout("origin\n"), nil)
	f.On(git("status"), ghx.Stdout(""), nil)
	return f
}

func goProject(string) fs.FS { return fstest.MapFS{"go.mod": {Data: []byte("module x\n")}} }

func run(t *testing.T, f *ghx.Fake, repo string) Report {
	t.Helper()
	r := Run(context.Background(), Options{
		Runner: f,
		Dir:    dir,
		Repo:   repo,
		Getenv: func(string) string { return "" },
		DirFS:  goProject,
	})
	for _, c := range r.Checks {
		if err := c.Validate(); err != nil {
			t.Errorf("invalid check: %v", err)
		}
	}
	return r
}

func find(t *testing.T, r Report, name string) out.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, r.Checks)
	return out.Check{}
}

func expect(t *testing.T, r Report, name string, level out.Level, code out.Code) out.Check {
	t.Helper()
	c := find(t, r, name)
	if c.Level != level || c.Code != code {
		t.Errorf("check %s = %s/%s (%s), want %s/%s", name, c.Level, c.Code, c.Message, level, code)
	}
	return c
}

func TestHealthyMachine(t *testing.T) {
	r := run(t, scenario(), "")
	if !r.OK {
		t.Fatalf("report not OK: %+v", r.Checks)
	}
	for _, name := range []string{"gh", "auth", "scope", "git", "repo", "commit", "worktree", "gate", "hosts"} {
		expect(t, r, name, out.LevelOK, "")
	}
	if got := r.Resolved["repo"]; got.Value != "acme/widget" || got.Source != "git remote origin" {
		t.Errorf("resolved repo = %+v", got)
	}
	if got := r.Resolved["gate"]; got.Value != "go test -json ./..." || got.Source != "go.mod" {
		t.Errorf("resolved gate = %+v", got)
	}
	if got := r.Resolved["commit"]; got.Value != sha {
		t.Errorf("resolved commit = %+v", got)
	}
	if len(r.Hosts) != 1 || r.Hosts[0].DisplayName != "gatehaul-1" {
		t.Errorf("hosts = %+v", r.Hosts)
	}
}

func TestMissingCodespaceScope(t *testing.T) {
	noScope := `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octo","tokenSource":"keyring","scopes":"gist, repo"}]}}`
	r := run(t, scenario(response{prefix: []string{"gh", "auth", "status"}, res: ghx.Stdout(noScope)}), "")
	if r.OK {
		t.Fatal("a token without the codespace scope must not be OK")
	}
	c := expect(t, r, "scope", out.LevelError, out.CodeScopeMissing)
	if c.Fix != "gh auth refresh -h github.com -s codespace" {
		t.Errorf("fix = %q", c.Fix)
	}
	expect(t, r, "hosts", out.LevelSkipped, out.CodeSkipped)
	// Local checks still run.
	expect(t, r, "gate", out.LevelOK, "")
}

func TestUnknownScopesOnlyWarn(t *testing.T) {
	envToken := `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octo","tokenSource":"GH_TOKEN","scopes":""}]}}`
	r := run(t, scenario(response{prefix: []string{"gh", "auth", "status"}, res: ghx.Stdout(envToken)}), "")
	if !r.OK {
		t.Fatalf("unknown scopes are a warning, not an error: %+v", r.Checks)
	}
	expect(t, r, "scope", out.LevelWarning, out.CodeScopeMissing)
	expect(t, r, "hosts", out.LevelOK, "")
}

func TestGHNotInstalled(t *testing.T) {
	notFound := &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	r := run(t, scenario(response{prefix: []string{"gh"}, err: notFound}), "")
	if r.OK {
		t.Fatal("missing gh must not be OK")
	}
	c := expect(t, r, "gh", out.LevelError, out.CodeGHMissing)
	if !strings.Contains(c.Fix, "https://cli.github.com") {
		t.Errorf("fix = %q", c.Fix)
	}
	expect(t, r, "auth", out.LevelSkipped, out.CodeSkipped)
	expect(t, r, "hosts", out.LevelSkipped, out.CodeSkipped)
	expect(t, r, "repo", out.LevelOK, "") // git still works
}

func TestGHTooOld(t *testing.T) {
	r := run(t, scenario(response{prefix: []string{"gh", "--version"}, res: ghx.Stdout("gh version 2.20.2 (2022-11-01)\n")}), "")
	expect(t, r, "gh", out.LevelError, out.CodeGHTooOld)
}

func TestNotLoggedIn(t *testing.T) {
	r := run(t, scenario(response{
		prefix: []string{"gh", "auth", "status"},
		res:    ghx.Exit(1, "You are not logged into any GitHub hosts. To log in, run: gh auth login"),
	}), "")
	c := expect(t, r, "auth", out.LevelError, out.CodeNotLoggedIn)
	if c.Fix != "gh auth login -h github.com" {
		t.Errorf("fix = %q", c.Fix)
	}
}

func TestNotARepository(t *testing.T) {
	r := run(t, scenario(response{prefix: git("rev-parse", "--show-toplevel"), res: ghx.Exit(128, "fatal: not a git repository")}), "")
	expect(t, r, "repo", out.LevelError, out.CodeNotGitRepo)
	expect(t, r, "commit", out.LevelSkipped, out.CodeSkipped)
	expect(t, r, "gate", out.LevelSkipped, out.CodeSkipped)
	expect(t, r, "hosts", out.LevelSkipped, out.CodeSkipped)
}

func TestRepoFlagOverridesRemote(t *testing.T) {
	r := run(t, scenario(), "other/project")
	if got := r.Resolved["repo"]; got.Value != "other/project" || got.Source != "--repo" {
		t.Errorf("resolved repo = %+v", got)
	}
	for _, call := range scenarioCalls(t, "other/project") {
		if len(call) >= 4 && call[3] == "remote" {
			t.Errorf("--repo given, but git remote was read: %v", call)
		}
	}
}

func scenarioCalls(t *testing.T, repo string) [][]string {
	t.Helper()
	f := scenario()
	run(t, f, repo)
	return f.Calls()
}

func TestBadRepoFlag(t *testing.T) {
	r := run(t, scenario(), "not-a-repo")
	expect(t, r, "repo", out.LevelError, out.CodeUsage)
}

func TestDirtyTreeWarns(t *testing.T) {
	r := run(t, scenario(response{prefix: git("status"), res: ghx.Stdout(" M main.go\n?? scratch.txt\n")}), "")
	if !r.OK {
		t.Fatal("a dirty tree is a warning, not an error")
	}
	c := expect(t, r, "worktree", out.LevelWarning, out.CodeDirtyTree)
	if !strings.Contains(c.Message, "2 uncommitted") || !strings.Contains(c.Message, "NOT tested") {
		t.Errorf("message = %q", c.Message)
	}
}

func TestNoHostsWarns(t *testing.T) {
	r := run(t, scenario(response{prefix: []string{"gh", "codespace", "list"}, res: ghx.Stdout("[]")}), "")
	if !r.OK {
		t.Fatal("no hosts yet is a warning, not an error")
	}
	c := expect(t, r, "hosts", out.LevelWarning, out.CodeNoHosts)
	if !strings.Contains(c.Fix, "gh codespace create -R acme/widget -d gatehaul-1") {
		t.Errorf("fix = %q", c.Fix)
	}
}

func TestHostListFailureIsClassified(t *testing.T) {
	r := run(t, scenario(response{
		prefix: []string{"gh", "codespace", "list"},
		res:    ghx.Exit(1, "HTTP 403: Must have admin rights (needs the \"codespace\" scope)"),
	}), "")
	expect(t, r, "hosts", out.LevelError, out.CodeScopeMissing)
}

func TestRender(t *testing.T) {
	r := run(t, scenario(response{prefix: []string{"gh", "codespace", "list"}, res: ghx.Stdout("[]")}), "")
	var b bytes.Buffer
	Render(&b, r)
	text := b.String()
	if !strings.Contains(text, "Fix: gh codespace create") || !strings.Contains(text, "ready: no blocking problem") {
		t.Fatalf("unexpected rendering:\n%s", text)
	}
}
