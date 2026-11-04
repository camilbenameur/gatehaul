package detect

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		url, host   string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/acme/widget.git", "github.com", "acme", "widget", true},
		{"https://github.com/acme/widget", "github.com", "acme", "widget", true},
		{"https://user@github.com/acme/widget.git/", "github.com", "acme", "widget", true},
		{"git@github.com:acme/widget.git", "github.com", "acme", "widget", true},
		{"ssh://git@github.com/acme/widget.git", "github.com", "acme", "widget", true},
		{"ssh://git@github.com:22/acme/widget", "github.com", "acme", "widget", true},
		{"git@GitHub.com:acme/widget", "github.com", "acme", "widget", true},
		{"https://ghe.example.com/acme/widget.git", "ghe.example.com", "acme", "widget", true},
		{"https://gitlab.com/acme/widget.git", "github.com", "", "", false},
		{"https://github.com/acme", "github.com", "", "", false},
		{"https://github.com/acme/widget/extra", "github.com", "", "", false},
		{`C:\src\widget`, "github.com", "", "", false},
		{"../widget", "github.com", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := ParseRemoteURL(c.url, c.host)
		if owner != c.owner || repo != c.repo || ok != c.ok {
			t.Errorf("ParseRemoteURL(%q, %q) = %q, %q, %v; want %q, %q, %v", c.url, c.host, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestOrderRemotes(t *testing.T) {
	got := orderRemotes([]string{"fork", "upstream", "origin"})
	want := []string{"origin", "upstream", "fork"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderRemotes = %v, want %v", got, want)
	}
}

func TestRepoPrefersOrigin(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/w", "remote", "get-url", "origin"}, ghx.Stdout("git@github.com:acme/widget.git\n"), nil).
		On([]string{"git", "-C", "/w", "remote", "get-url", "fork"}, ghx.Stdout("https://github.com/me/widget\n"), nil).
		On([]string{"git", "-C", "/w", "remote"}, ghx.Stdout("fork\norigin\n"), nil)
	v, err := Git{Runner: f, Dir: "/w"}.Repo(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if v.Value != "acme/widget" || v.Source != "git remote origin" {
		t.Fatalf("Repo = %+v", v)
	}
}

func TestRepoAmbiguousWithoutOrigin(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/w", "remote", "get-url", "a"}, ghx.Stdout("https://github.com/x/one\n"), nil).
		On([]string{"git", "-C", "/w", "remote", "get-url", "b"}, ghx.Stdout("https://github.com/y/two\n"), nil).
		On([]string{"git", "-C", "/w", "remote"}, ghx.Stdout("a\nb\n"), nil)
	_, err := (Git{Runner: f, Dir: "/w"}).Repo(context.Background(), "github.com")
	if !errors.Is(err, ErrNoGitHubRemote) {
		t.Fatalf("err = %v, want ErrNoGitHubRemote (two GitHub remotes, none preferred)", err)
	}
	var nr *NoGitHubRemoteError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %T, want *NoGitHubRemoteError", err)
	}
	msg, fix := nr.Explain()
	for _, want := range []string{"several git remotes", "a is x/one", "b is y/two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if fix != "pass --repo with the repository you mean: --repo x/one or --repo y/two" {
		t.Errorf("fix = %q", fix)
	}
}

// A checkout whose remotes are on another forge (the usual case: gatehaul run
// from the wrong directory) must not be told to add a GitHub remote.
func TestRepoNonGitHubOrigin(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/w", "remote", "get-url", "origin"}, ghx.Stdout("https://user@git.example.com/team/project/_git/widget\n"), nil).
		On([]string{"git", "-C", "/w", "remote", "get-url", "backup"}, ghx.Stdout("/srv/mirrors/widget.git\n"), nil).
		On([]string{"git", "-C", "/w", "remote"}, ghx.Stdout("backup\norigin\n"), nil)
	_, err := (Git{Runner: f, Dir: "/w"}).Repo(context.Background(), "github.com")
	var nr *NoGitHubRemoteError
	if !errors.As(err, &nr) || !errors.Is(err, ErrNoGitHubRemote) {
		t.Fatalf("err = %v, want a *NoGitHubRemoteError", err)
	}
	want := []Remote{{Name: "origin", Host: "git.example.com"}, {Name: "backup"}}
	if !reflect.DeepEqual(nr.Others, want) {
		t.Errorf("others = %+v, want %+v", nr.Others, want)
	}
	msg, fix := nr.Explain()
	if msg != "no git remote points to github.com (origin is git.example.com, backup is a local path)" {
		t.Errorf("message = %q", msg)
	}
	if strings.Contains(fix, "git remote add") || strings.Contains(msg, "user@") {
		t.Errorf("fix %q must not suggest adding a remote, nor the message %q show credentials", fix, msg)
	}
	if !strings.HasPrefix(fix, "run gatehaul from the checkout of the GitHub repository") || !strings.Contains(fix, "--repo owner/repo") {
		t.Errorf("fix = %q, want the read-only fixes first", fix)
	}
}

func TestRepoWithoutAnyRemote(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git", "-C", "/w", "remote"}, ghx.Stdout(""), nil)
	_, err := (Git{Runner: f, Dir: "/w"}).Repo(context.Background(), "github.com")
	var nr *NoGitHubRemoteError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want a *NoGitHubRemoteError", err)
	}
	msg, fix := nr.Explain()
	if !strings.Contains(msg, "no git remote") {
		t.Errorf("message = %q", msg)
	}
	// The read-only fixes come first; adding a remote is a step for the user.
	ro := strings.Index(fix, "--repo owner/repo")
	add := strings.Index(fix, "git remote add origin https://github.com/OWNER/REPO.git")
	if ro < 0 || add < 0 || add < ro || !strings.Contains(fix, "for the user") {
		t.Errorf("fix = %q", fix)
	}
}

func TestRemoteHost(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widget.git":         "github.com",
		"https://user:secret@git.example.com:8443/x": "git.example.com",
		"ssh://git@ssh.example.com:22/a/b":           "ssh.example.com",
		"git@gitlab.example.org:a/b.git":             "gitlab.example.org",
		"file:///srv/git/widget.git":                 "",
		"/srv/git/widget.git":                        "",
		"../widget":                                  "",
		`C:\src\widget`:                              "",
		"C:/src/widget":                              "",
		"./a:b":                                      "",
	}
	for raw, want := range cases {
		if got := RemoteHost(raw); got != want {
			t.Errorf("RemoteHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDirty(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git", "-C", "/w", "status"}, ghx.Stdout(" M main.go\r\n?? notes.txt\nR  a.go -> b.go\n"), nil)
	got, err := Git{Runner: f, Dir: "/w"}.Dirty(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"main.go", "notes.txt", "a.go -> b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dirty = %q, want %q", got, want)
	}
}

func TestDetectGate(t *testing.T) {
	cases := []struct {
		name   string
		files  fstest.MapFS
		source string
		ok     bool
	}{
		{"config wins", fstest.MapFS{"gatehaul.toml": {}, "go.mod": {}}, "gatehaul.toml", true},
		{"go", fstest.MapFS{"go.mod": {}, "package.json": {Data: []byte(`{"scripts":{"test":"jest"}}`)}}, "go.mod", true},
		{"npm with test script", fstest.MapFS{"package.json": {Data: []byte(`{"scripts":{"test":"vitest run"}}`)}}, "package.json", true},
		{"npm without test script", fstest.MapFS{"package.json": {Data: []byte(`{"scripts":{"build":"tsc"}}`)}}, "", false},
		{"rust", fstest.MapFS{"Cargo.toml": {}}, "Cargo.toml", true},
		{"python", fstest.MapFS{"pyproject.toml": {}}, "pyproject.toml", true},
		{"go.mod is a directory", fstest.MapFS{"go.mod/x": {}}, "", false},
		{"nothing", fstest.MapFS{"README.md": {}}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, ok := DetectGate(c.files)
			if ok != c.ok || g.Source != c.source {
				t.Fatalf("DetectGate = %+v, %v; want source %q, %v", g, ok, c.source, c.ok)
			}
			if ok && g.Command == "" {
				t.Fatal("a detected gate needs a command")
			}
		})
	}
}

func TestGHHost(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if v := GHHost(getenv); v.Value != "github.com" || v.Source != "default" {
		t.Fatalf("default host = %+v", v)
	}
	env["GH_HOST"] = "ghe.example.com"
	if v := GHHost(getenv); v.Value != "ghe.example.com" || v.Source != "GH_HOST" {
		t.Fatalf("GH_HOST host = %+v", v)
	}
}
