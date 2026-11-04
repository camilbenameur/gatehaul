package ctl

import (
	"context"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// A checkout whose origin is on another forge is usually the wrong directory:
// its Fix line must not tell an agent to add a GitHub remote to it.
func TestDetectRepoNonGitHubOrigin(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", ".", "remote", "get-url", "origin"}, ghx.Stdout("https://git.example.com/team/widget.git\n"), nil).
		On([]string{"git", "-C", ".", "remote"}, ghx.Stdout("origin\n"), nil)
	_, e := DetectRepo(context.Background(), f, "", "", "github.com")
	if e == nil {
		t.Fatal("DetectRepo succeeded without a GitHub remote")
	}
	if e.Code != out.CodeNoGitHubRemote || e.Exit != out.ExitInfra {
		t.Errorf("error = %s exit %d", e.Code, e.Exit)
	}
	if !strings.Contains(e.Message, "origin is git.example.com") {
		t.Errorf("message = %q, want what was found", e.Message)
	}
	if strings.Contains(e.Fix, "git remote add") {
		t.Errorf("fix = %q must not suggest git remote add", e.Fix)
	}
	if !strings.HasPrefix(e.Fix, "run gatehaul from the checkout of the GitHub repository") || !strings.Contains(e.Fix, "--repo owner/repo") {
		t.Errorf("fix = %q", e.Fix)
	}
}

func TestDetectRepoWithoutAnyRemote(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git", "-C", ".", "remote"}, ghx.Stdout(""), nil)
	_, e := DetectRepo(context.Background(), f, "", "", "github.com")
	if e == nil || e.Code != out.CodeNoGitHubRemote {
		t.Fatalf("error = %+v", e)
	}
	if i := strings.Index(e.Fix, "git remote add"); i < strings.Index(e.Fix, "--repo owner/repo") {
		t.Errorf("fix = %q: the read-only fixes must come first", e.Fix)
	}
}
