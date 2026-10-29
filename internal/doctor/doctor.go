// Package doctor checks everything gatehaul needs on this machine and reports,
// for every problem, the command that fixes it. It never stops at the first
// failure: checks that depend on a failed one are reported as skipped.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// Report is the result of a doctor run, printed as is by --json.
type Report struct {
	OK       bool                    `json:"ok"`
	Checks   []out.Check             `json:"checks"`
	Resolved map[string]detect.Value `json:"resolved"`
	Hosts    []ghx.Codespace         `json:"hosts,omitempty"`
}

// Options configures a run. Zero values use the real environment.
type Options struct {
	Runner ghx.Runner
	Dir    string                 // working directory; "" means "."
	Repo   string                 // --repo owner/repo, overrides the git remote
	Getenv func(string) string    // defaults to os.Getenv
	DirFS  func(dir string) fs.FS // defaults to os.DirFS
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Run performs every check.
func Run(ctx context.Context, o Options) Report {
	if o.Runner == nil {
		o.Runner = ghx.ExecRunner{}
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.DirFS == nil {
		o.DirFS = detect.DirFS
	}
	r := Report{Resolved: map[string]detect.Value{}}
	add := func(c out.Check) { r.Checks = append(r.Checks, c) }

	host := detect.GHHost(o.Getenv)
	r.Resolved["gh_host"] = host
	h := host.Value

	// gh and its authentication.
	ghOK := false
	switch v, err := ghx.GHVersion(ctx, o.Runner); {
	case ghx.IsNotFound(err):
		add(out.Fail("gh", out.CodeGHMissing, "gh is not installed", "install gh from https://cli.github.com, then run: gh auth login -h "+h))
	case err != nil:
		add(out.Fail("gh", out.CodeGHError, "cannot run gh: "+err.Error(), "run gh --version by hand to see the error"))
	case v.Less(ghx.MinGHVersion):
		add(out.Fail("gh", out.CodeGHTooOld, fmt.Sprintf("gh %s is older than %s", v, ghx.MinGHVersion), "upgrade gh: https://cli.github.com"))
	default:
		ghOK = true
		add(out.OK("gh", "gh "+v.String()))
		r.Resolved["gh_version"] = detect.Value{Value: v.String(), Source: "gh --version"}
	}

	authOK, scopeUsable := false, false
	if !ghOK {
		add(out.Skip("auth", "gh"))
		add(out.Skip("scope", "gh"))
	} else {
		acc, err := ghx.ActiveAccount(ctx, o.Runner, h)
		if err != nil {
			add(authFailure(err, h))
			add(out.Skip("scope", "auth"))
		} else {
			authOK = true
			add(out.OK("auth", fmt.Sprintf("logged in to %s as %s (token from %s)", h, acc.Login, acc.TokenSource)))
			r.Resolved["account"] = detect.Value{Value: acc.Login, Source: "gh auth status"}

			switch has, known := acc.HasScope("codespace"); {
			case !known:
				scopeUsable = true
				add(out.Warn("scope", out.CodeScopeMissing,
					"gh does not report the scopes of this token (token from "+acc.TokenSource+"), so the codespace scope cannot be checked",
					"if codespace commands fail, run: gh auth refresh -h "+h+" -s codespace"))
			case !has:
				add(out.Fail("scope", out.CodeScopeMissing, "the gh token lacks the codespace scope", "gh auth refresh -h "+h+" -s codespace"))
			default:
				scopeUsable = true
				add(out.OK("scope", "the gh token has the codespace scope"))
			}
		}
	}

	// git and the repository.
	g := detect.Git{Runner: o.Runner, Dir: o.Dir}
	gitOK := false
	switch v, err := g.Version(ctx); {
	case ghx.IsNotFound(err):
		add(out.Fail("git", out.CodeGitMissing, "git is not installed", "install git "+detect.MinGitVersion.String()+" or later: https://git-scm.com"))
	case err != nil:
		add(out.Fail("git", out.CodeGitMissing, "cannot run git: "+err.Error(), "run git --version by hand to see the error"))
	case v.Less(detect.MinGitVersion):
		add(out.Fail("git", out.CodeGitTooOld, fmt.Sprintf("git %s is older than %s", v, detect.MinGitVersion), "upgrade git: https://git-scm.com"))
	default:
		gitOK = true
		add(out.OK("git", "git "+v.String()))
	}

	top := ""
	var topErr error
	if gitOK {
		top, topErr = g.TopLevel(ctx)
	}

	repoOK := false
	repo := ""
	switch {
	case o.Repo != "":
		if !repoRE.MatchString(o.Repo) {
			add(out.Fail("repo", out.CodeUsage, fmt.Sprintf("--repo %q is not of the form owner/repo", o.Repo), "pass --repo owner/repo"))
			break
		}
		repoOK, repo = true, o.Repo
		r.Resolved["repo"] = detect.Value{Value: o.Repo, Source: "--repo"}
		add(out.OK("repo", o.Repo+" (from --repo)"))
	case !gitOK:
		add(out.Skip("repo", "git"))
	case topErr != nil:
		add(notRepo(o.Dir, topErr))
	default:
		v, err := g.Repo(ctx, h)
		switch {
		case errors.Is(err, detect.ErrNoGitHubRemote):
			add(out.Fail("repo", out.CodeNoGitHubRemote, "no git remote points to "+h,
				"add one (git remote add origin https://"+h+"/OWNER/REPO.git) or pass --repo owner/repo"))
		case err != nil:
			add(out.Fail("repo", out.CodeNotGitRepo, "cannot read the git remotes: "+err.Error(), "run git remote -v by hand to see the error"))
		default:
			repoOK, repo = true, v.Value
			r.Resolved["repo"] = v
			add(out.OK("repo", v.Value+" (from "+v.Source+")"))
		}
	}

	// What would be sent, and what would run.
	switch {
	case !gitOK:
		add(out.Skip("commit", "git"))
	case topErr != nil || top == "":
		add(out.Skip("commit", "repo"))
	default:
		head, err := g.Head(ctx)
		switch {
		case errors.Is(err, detect.ErrNoCommit):
			add(out.Fail("commit", out.CodeNoCommit, "the repository has no commit yet",
				"commit your work: gatehaul sends commits, never the working tree"))
		case err != nil:
			add(out.Fail("commit", out.CodeNotGitRepo, "cannot read HEAD: "+err.Error(), "run git rev-parse HEAD by hand to see the error"))
		default:
			r.Resolved["commit"] = head
			add(out.OK("commit", "HEAD is "+short(head.Value)))
		}

		dirty, err := g.Dirty(ctx)
		switch {
		case err != nil:
			add(out.Fail("worktree", out.CodeNotGitRepo, "cannot read git status: "+err.Error(), "run git status by hand to see the error"))
		case len(dirty) > 0:
			add(out.Warn("worktree", out.CodeDirtyTree,
				fmt.Sprintf("%d uncommitted file(s) are NOT sent and NOT tested: %s", len(dirty), list(dirty, 5)),
				"commit them before gatehaul run, or stash them"))
		default:
			add(out.OK("worktree", "clean"))
		}
	}

	if top == "" {
		add(out.Skip("gate", "repo"))
	} else if gate, ok := detect.DetectGate(o.DirFS(top)); ok {
		r.Resolved["gate"] = detect.Value{Value: gate.Command, Source: gate.Source, Fallback: "gatehaul gate -- <cmd>, or a gatehaul.toml"}
		add(out.OK("gate", gate.Command+" (from "+gate.Source+")"))
	} else {
		add(out.Warn("gate", out.CodeNoGate, "no test command recognised (looked for gatehaul.toml, go.mod, package.json with a test script, Cargo.toml, pyproject.toml)",
			"run gatehaul gate -- <your test command>, or add a gatehaul.toml"))
	}

	// The codespaces gatehaul may use.
	switch {
	case !authOK:
		add(out.Skip("hosts", "auth"))
	case !scopeUsable:
		add(out.Skip("hosts", "scope"))
	case !repoOK:
		add(out.Skip("hosts", "repo"))
	default:
		hosts, err := ghx.ListHosts(ctx, o.Runner, repo)
		var ghErr *ghx.GHError
		switch {
		case errors.As(err, &ghErr):
			code, fix := ghx.Classify(h, string(ghErr.Result.Stderr))
			add(out.Fail("hosts", code, err.Error(), fix))
		case err != nil:
			add(out.Fail("hosts", out.CodeGHError, "cannot list codespaces: "+err.Error(), "run gh codespace list --repo "+repo+" by hand to see the error"))
		case len(hosts) == 0:
			add(out.Warn("hosts", out.CodeNoHosts, "no codespace of "+repo+" has a display name starting with "+ghx.DisplayPrefix,
				"gh codespace create -R "+repo+" -d "+ghx.DisplayPrefix+"1 --idle-timeout 60m --retention-period 72h (this uses your Codespaces quota)"))
		default:
			r.Hosts = hosts
			add(out.OK("hosts", describeHosts(hosts)))
		}
	}

	r.OK = true
	for _, c := range r.Checks {
		if c.Level == out.LevelError {
			r.OK = false
		}
	}
	return r
}

func authFailure(err error, host string) out.Check {
	var ghErr *ghx.GHError
	switch {
	case errors.As(err, &ghErr):
		code, fix := ghx.Classify(host, string(ghErr.Result.Stderr))
		return out.Fail("auth", code, err.Error(), fix)
	case errors.Is(err, ghx.ErrNotLoggedIn):
		return out.Fail("auth", out.CodeNotLoggedIn, err.Error(), "gh auth login -h "+host)
	default:
		return out.Fail("auth", out.CodeGHError, "cannot read gh auth status: "+err.Error(), "run gh auth status by hand to see the error")
	}
}

func notRepo(dir string, err error) out.Check {
	if errors.Is(err, detect.ErrNotRepo) {
		return out.Fail("repo", out.CodeNotGitRepo, dir+" is not inside a git repository", "cd into your project, or pass --repo owner/repo")
	}
	return out.Fail("repo", out.CodeNotGitRepo, "cannot run git in "+dir+": "+err.Error(), "run git rev-parse --show-toplevel by hand to see the error")
}

func describeHosts(hosts []ghx.Codespace) string {
	parts := make([]string, 0, len(hosts))
	for _, c := range hosts {
		parts = append(parts, c.DisplayName+" ("+c.State+")")
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d codespace(s): %s", len(hosts), strings.Join(parts, ", "))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func list(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// Render prints the report for humans: one line per check, then its fix.
func Render(w io.Writer, r Report) {
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-8s %-9s %s\n", c.Level, c.Name, c.Message)
		if c.Fix != "" {
			fmt.Fprintf(w, "%-8s %-9s Fix: %s\n", "", "", c.Fix)
		}
	}
	if r.OK {
		fmt.Fprintln(w, "\nready: no blocking problem")
	} else {
		fmt.Fprintln(w, "\nnot ready: fix the errors above, then run gatehaul doctor again")
	}
}
