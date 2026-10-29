package ghx

import (
	"context"
	"errors"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/out"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"gh version 2.102.0 (2026-09-30)\nhttps://github.com/cli/cli/releases/tag/v2.102.0", Version{2, 102, 0}, true},
		{"git version 2.47.1.windows.1", Version{2, 47, 1}, true},
		{"git version 2.31", Version{2, 31, 0}, true},
		{"no version here", Version{}, false},
	}
	for _, c := range cases {
		got, ok := ParseVersion(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestVersionLess(t *testing.T) {
	if !(Version{2, 39, 9}).Less(Version{2, 40, 0}) {
		t.Error("2.39.9 should be older than 2.40.0")
	}
	if (Version{2, 102, 0}).Less(Version{2, 40, 0}) {
		t.Error("2.102.0 should not be older than 2.40.0 (numeric, not lexical, comparison)")
	}
	if (Version{2, 40, 0}).Less(Version{2, 40, 0}) {
		t.Error("a version is not older than itself")
	}
}

// authJSON has the shape printed by gh 2.102 for "gh auth status --json hosts".
const authJSON = `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octo","tokenSource":"keyring","scopes":"codespace, gist, read:org, repo, workflow","gitProtocol":"https"}]}}`

var authArgv = []string{"gh", "auth", "status"}

func TestActiveAccount(t *testing.T) {
	f := (&Fake{}).On(authArgv, Stdout(authJSON), nil)
	acc, err := ActiveAccount(context.Background(), f, "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Login != "octo" {
		t.Fatalf("login = %q", acc.Login)
	}
	if has, known := acc.HasScope("codespace"); !has || !known {
		t.Fatalf("HasScope(codespace) = %v, %v", has, known)
	}
	if has, known := acc.HasScope("admin:org"); has || !known {
		t.Fatalf("HasScope(admin:org) = %v, %v", has, known)
	}
}

func TestActiveAccountFailedState(t *testing.T) {
	js := `{"hosts":{"github.com":[{"state":"error","active":true,"host":"github.com","login":"octo","error":"token is invalid"}]}}`
	f := (&Fake{}).On(authArgv, Stdout(js), nil)
	if _, err := ActiveAccount(context.Background(), f, "github.com"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestActiveAccountNoAccount(t *testing.T) {
	f := (&Fake{}).On(authArgv, Exit(1, "You are not logged into any GitHub hosts. To log in, run: gh auth login"), nil)
	_, err := ActiveAccount(context.Background(), f, "github.com")
	var ghErr *GHError
	if !errors.As(err, &ghErr) {
		t.Fatalf("err = %v, want *GHError", err)
	}
	if code, _ := Classify("github.com", string(ghErr.Result.Stderr)); code != out.CodeNotLoggedIn {
		t.Fatalf("Classify = %s, want %s", code, out.CodeNotLoggedIn)
	}
}

func TestHasScopeUnknown(t *testing.T) {
	if _, known := (Account{TokenSource: "GH_TOKEN"}).HasScope("codespace"); known {
		t.Fatal("scopes are unknown when gh reports none")
	}
}

func TestListHostsFiltersOnDisplayName(t *testing.T) {
	js := `[
	 {"name":"fluffy-space-x1","displayName":"gatehaul-1","state":"Available","repository":"acme/widget","machineName":"basicLinux32gb","lastUsedAt":"2026-10-29T10:00:00Z"},
	 {"name":"other-name-y2","displayName":"my dev box","state":"Shutdown","repository":"acme/widget","machineName":"basicLinux32gb","lastUsedAt":"2026-10-28T10:00:00Z"},
	 {"name":"fuzzy-space-z3","displayName":"gatehaul-2","state":"Shutdown","repository":"acme/widget","machineName":"basicLinux32gb","lastUsedAt":"2026-10-27T10:00:00Z"}
	]`
	f := (&Fake{}).On([]string{"gh", "codespace", "list", "--repo", "acme/widget"}, Stdout(js), nil)
	hosts, err := ListHosts(context.Background(), f, "acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].DisplayName != "gatehaul-1" || hosts[1].DisplayName != "gatehaul-2" {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		stderr string
		want   out.Code
	}{
		{"unknown flag: --json", out.CodeGHTooOld},
		{"HTTP 403: Must have admin rights to Repository. (needs the \"codespace\" scope)", out.CodeScopeMissing},
		{"error getting codespaces: this API requires the codespace scope", out.CodeScopeMissing},
		{"HTTP 401: Bad credentials (https://api.github.com/user)", out.CodeNotLoggedIn},
		{"To get started with GitHub CLI, please run:  gh auth login", out.CodeNotLoggedIn},
		{"API rate limit exceeded for user", out.CodeRateLimited},
		{"dial tcp: lookup api.github.com: no such host", out.CodeNetwork},
		{"something nobody has seen before", out.CodeGHError},
	}
	for _, c := range cases {
		code, fix := Classify("github.com", c.stderr)
		if code != c.want {
			t.Errorf("Classify(%q) = %s, want %s", c.stderr, code, c.want)
		}
		if fix == "" {
			t.Errorf("Classify(%q) gave no fix", c.stderr)
		}
	}
}
