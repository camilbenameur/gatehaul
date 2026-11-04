package doctor

import (
	"bytes"
	"context"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/camilbenameur/gatehaul/internal/detect"
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

// onPath is a gatehaul binary that "gatehaul" on PATH resolves to.
var onPath = detect.Invocation{
	Executable: func() (string, error) { return "/opt/bin/gatehaul", nil },
	LookPath:   func(string) (string, error) { return "/opt/bin/gatehaul", nil },
	SameFile:   func(a, b string) bool { return a == b },
}

func run(t *testing.T, f *ghx.Fake, repo string) Report {
	t.Helper()
	r := Run(context.Background(), Options{
		Runner: f,
		Dir:    dir,
		Repo:   repo,
		Getenv: func(string) string { return "" },
		DirFS:  goProject,
		Self:   onPath,
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

// Run from a checkout of another forge's repository, doctor says what it found
// and leads with read-only fixes, never with adding a remote.
func TestNonGitHubOriginLeadsWithReadOnlyFixes(t *testing.T) {
	r := run(t, scenario(response{prefix: git("remote", "get-url", "origin"), res: ghx.Stdout("git@git.example.com:team/widget.git\n")}), "")
	c := expect(t, r, "repo", out.LevelError, out.CodeNoGitHubRemote)
	if c.Message != "no git remote points to github.com (origin is git.example.com)" {
		t.Errorf("message = %q", c.Message)
	}
	if strings.Contains(c.Fix, "git remote add") || !strings.HasPrefix(c.Fix, "run gatehaul from the checkout of the GitHub repository") {
		t.Errorf("fix = %q", c.Fix)
	}
}

func TestPathCheck(t *testing.T) {
	notFound := func(string) (string, error) { return "", exec.ErrNotFound }
	cases := []struct {
		name   string
		inv    detect.Invocation
		env    string
		level  out.Level
		code   out.Code
		prefix string
		msg    string
	}{
		{"on PATH", onPath, "", out.LevelOK, "", "gatehaul", "gatehaul on PATH is this binary"},
		{"not on PATH", detect.Invocation{
			Executable: func() (string, error) { return "/home/me/.local/bin/gatehaul", nil }, LookPath: notFound,
		}, "", out.LevelWarning, out.CodeNotOnPath, "/home/me/.local/bin/gatehaul", "gatehaul is not on PATH"},
		{"another gatehaul on PATH", detect.Invocation{
			Executable: func() (string, error) { return "/home/me/.local/bin/gatehaul", nil },
			LookPath:   func(string) (string, error) { return "/usr/bin/gatehaul", nil },
			SameFile:   func(a, b string) bool { return a == b },
		}, "", out.LevelWarning, out.CodeNotOnPath, "/home/me/.local/bin/gatehaul", "the gatehaul on PATH (/usr/bin/gatehaul) is not this binary"},
		{"override", detect.Invocation{LookPath: notFound}, "bash tools/gh.sh", out.LevelOK, "", "bash tools/gh.sh", "$GATEHAUL_NEXT"},
		{"binary not found", detect.Invocation{
			Executable: func() (string, error) { return "", exec.ErrNotFound }, LookPath: notFound,
		}, "", out.LevelWarning, out.CodeNotOnPath, "gatehaul", "cannot locate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{detect.NextEnv: c.env}
			r := Run(context.Background(), Options{
				Runner: scenario(), Dir: dir, Getenv: func(k string) string { return env[k] }, DirFS: goProject, Self: c.inv,
			})
			ch := expect(t, r, "path", c.level, c.code)
			if err := ch.Validate(); err != nil {
				t.Error(err)
			}
			if !strings.Contains(ch.Message, c.msg) {
				t.Errorf("message = %q, want %q", ch.Message, c.msg)
			}
			if got := r.Resolved["self"].Value; got != c.prefix {
				t.Errorf("resolved self = %q, want %q", got, c.prefix)
			}
			if !r.OK {
				t.Errorf("the path check never blocks: %+v", r.Checks)
			}
		})
	}
}

func TestNoRemoteAtAll(t *testing.T) {
	r := run(t, scenario(response{prefix: git("remote"), res: ghx.Stdout("")}), "")
	c := expect(t, r, "repo", out.LevelError, out.CodeNoGitHubRemote)
	if !strings.HasPrefix(c.Fix, "run gatehaul from the checkout") || !strings.Contains(c.Fix, "for the user to do (git remote add") {
		t.Errorf("fix = %q", c.Fix)
	}
}

func TestDirtyTreeWarns(t *testing.T) {
	r := run(t, scenario(response{prefix: git("status"), res: ghx.Stdout(" M main.go\n?? scratch.txt\n")}), "")
	if !r.OK {
		t.Fatal("a dirty tree is a warning, not an error")
	}
	c := expect(t, r, "worktree", out.LevelWarning, out.CodeDirtyTree)
	if c.Message != "1 modified file(s) are not committed, so NOT sent and NOT tested: main.go" || c.Fix != "commit them before gatehaul run, or stash them" {
		t.Errorf("message = %q, fix = %q", c.Message, c.Fix)
	}
	u := expect(t, r, "untracked", out.LevelWarning, out.CodeUntracked)
	if u.Message != "1 untracked file(s) are not in git, so not sent: scratch.txt" || !strings.Contains(u.Fix, ".gitignore") {
		t.Errorf("message = %q, fix = %q", u.Message, u.Fix)
	}
}

func TestOnlyModifiedFilesWarnAsDirty(t *testing.T) {
	r := run(t, scenario(response{prefix: git("status"), res: ghx.Stdout("M  a.go\n M b.go\n")}), "")
	expect(t, r, "worktree", out.LevelWarning, out.CodeDirtyTree)
	for _, c := range r.Checks {
		if c.Name == "untracked" {
			t.Errorf("unexpected untracked check: %+v", c)
		}
	}
}

func TestOnlyUntrackedFilesAreASofterWarning(t *testing.T) {
	r := run(t, scenario(response{prefix: git("status"), res: ghx.Stdout("?? grep.exe.stackdump\n")}), "")
	if !r.OK {
		t.Fatal("untracked files are a warning, not an error")
	}
	expect(t, r, "worktree", out.LevelOK, "")
	c := expect(t, r, "untracked", out.LevelWarning, out.CodeUntracked)
	if strings.Contains(c.Message, "NOT tested") || strings.Contains(c.Fix, "stash") || !strings.Contains(c.Message, "grep.exe.stackdump") {
		t.Errorf("message = %q, fix = %q", c.Message, c.Fix)
	}
}

func TestGateInASubdirectory(t *testing.T) {
	f := scenario()
	r := Run(context.Background(), Options{
		Runner: f, Dir: dir, Getenv: func(string) string { return "" }, Self: onPath,
		DirFS: func(string) fs.FS {
			return fstest.MapFS{"README.md": {}, "src/back-end/src/go.mod": {Data: []byte("module x\n")}}
		},
	})
	for _, c := range r.Checks {
		if err := c.Validate(); err != nil {
			t.Errorf("invalid check: %v", err)
		}
	}
	if !r.OK {
		t.Fatal("a gate in a subdirectory is a warning, not an error")
	}
	c := expect(t, r, "gate", out.LevelWarning, out.CodeGateSubdir)
	if c.Fix != "gatehaul run --cd src/back-end/src -- go test ./..." {
		t.Errorf("fix = %q", c.Fix)
	}
	for _, want := range []string{"go.mod in src/back-end/src", "gatehaul run -- <cmd> works without it"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q lacks %q", c.Message, want)
		}
	}
	if got := r.Resolved["gate"]; got.Source != "src/back-end/src/go.mod" {
		t.Errorf("resolved gate = %+v", got)
	}
}

func TestNoGateAnywhere(t *testing.T) {
	r := Run(context.Background(), Options{
		Runner: scenario(), Dir: dir, Getenv: func(string) string { return "" }, Self: onPath,
		DirFS: func(string) fs.FS { return fstest.MapFS{"README.md": {}} },
	})
	for _, c := range r.Checks {
		if err := c.Validate(); err != nil {
			t.Errorf("invalid check: %v", err)
		}
	}
	c := expect(t, r, "gate", out.LevelWarning, out.CodeNoGate)
	if !strings.Contains(c.Message, "gatehaul run -- <cmd> works without it") || strings.Contains(c.Fix, "gatehaul gate --") {
		t.Errorf("message = %q, fix = %q", c.Message, c.Fix)
	}
	if !strings.Contains(c.Fix, "gatehaul run --cd <dir> -- <your test command>") {
		t.Errorf("fix = %q", c.Fix)
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
