package patch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

const gitTimeout = 2 * time.Minute

var errNoEnvRunner = errors.New("patch: the runner cannot set environment variables (it must implement ghx.EnvRunner)")

// gitCmd runs git through a ghx.Runner. Every call disables hooks and the
// user's quoting of paths; unless worktree is set, it also pins the settings
// that would change how blobs and names are read (line endings, case).
type gitCmd struct {
	r        ghx.Runner
	dir      string
	env      []string
	worktree bool
}

func (g gitCmd) argv(args []string) []string {
	argv := []string{"git"}
	if g.dir != "" {
		argv = append(argv, "-C", g.dir)
	}
	argv = append(argv, "-c", "core.hooksPath=", "-c", "core.quotepath=false")
	if !g.worktree {
		argv = append(argv, "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.ignorecase=false")
	}
	return append(argv, args...)
}

func (g gitCmd) run(ctx context.Context, stdin io.Reader, args ...string) (ghx.Result, error) {
	argv := g.argv(args)
	if len(g.env) == 0 {
		return g.r.Run(ctx, argv, stdin, gitTimeout)
	}
	er, ok := g.r.(ghx.EnvRunner)
	if !ok {
		return ghx.Result{}, errNoEnvRunner
	}
	return er.RunEnv(ctx, g.env, argv, stdin, gitTimeout)
}

// out runs git and returns its stdout; a non-zero exit is an error. The runner
// keeps at most its first 8 MiB of output, and out hands back that start
// without complaint: use outAll when a cut-off answer would be wrong.
func (g gitCmd) out(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	res, err := g.run(ctx, stdin, args...)
	if err != nil {
		return nil, fmt.Errorf("patch: git %s: %w", args[0], err)
	}
	if !res.OK() {
		return nil, fmt.Errorf("patch: git %s: exit %d: %s", args[0], res.ExitCode, oneLine(res.Stderr))
	}
	return res.Stdout, nil
}

// outAll is out for a command whose whole answer matters, such as a list of
// paths: output that the runner had to cut is an error.
func (g gitCmd) outAll(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	res, err := g.run(ctx, stdin, args...)
	if err != nil {
		return nil, fmt.Errorf("patch: git %s: %w", args[0], err)
	}
	if !res.OK() {
		return nil, fmt.Errorf("patch: git %s: exit %d: %s", args[0], res.ExitCode, oneLine(res.Stderr))
	}
	if res.Truncated {
		return nil, fmt.Errorf("patch: git %s: the answer is too large to be read in full", args[0])
	}
	return res.Stdout, nil
}

// topLevel returns the root of the work tree that contains dir. Patch paths
// are relative to that root, and git apply only touches paths below the
// directory it runs in.
func topLevel(ctx context.Context, r ghx.Runner, dir string, worktree bool) (string, error) {
	g := gitCmd{r: r, dir: dir, worktree: worktree}
	out, err := g.out(ctx, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", errors.New("patch: git did not print the top level of the work tree")
	}
	return top, nil
}

// oneLine turns command output into a single line that is safe to print: runs
// of white space collapse, other control characters become '?', and the length
// is bounded.
func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
	const maxLen = 400
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}
