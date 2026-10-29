// Package bundle builds the git bundle that carries a commit to a host. When
// the host's probe says which commits it already holds, the bundle is
// incremental: it leaves out everything reachable from those commits.
package bundle

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

const (
	gitQuick  = 30 * time.Second
	gitBundle = 10 * time.Minute
)

// RefPrefix is where Create parks the commit while git reads it.
const RefPrefix = "refs/gatehaul/send/"

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// HostHas reports whether the host already holds sha, so that no bundle needs
// to be sent. hostRefs is the probe's map of branch key to last commit.
func HostHas(hostRefs map[string]string, sha string) bool {
	for _, v := range hostRefs {
		if v == sha {
			return true
		}
	}
	return false
}

// Bases returns the commits the host has that can serve as prerequisites of a
// bundle of sha: values of hostRefs that look like commit ids and for which
// isAncestor reports true (the commit exists locally and is an ancestor of
// sha). The result is deduplicated and sorted.
func Bases(hostRefs map[string]string, sha string, isAncestor func(base string) bool) []string {
	seen := map[string]bool{}
	var bases []string
	for _, v := range hostRefs {
		if v == sha || !shaRE.MatchString(v) || seen[v] {
			continue
		}
		seen[v] = true
		if isAncestor(v) {
			bases = append(bases, v)
		}
	}
	sort.Strings(bases)
	return bases
}

// IsAncestor reports whether base exists in the repository at dir and is an
// ancestor of sha. Any failure, including a missing commit, is false: the
// bundle is then just a little bigger.
func IsAncestor(ctx context.Context, r ghx.Runner, dir, base, sha string) bool {
	res, err := r.Run(ctx, []string{"git", "-C", dir, "merge-base", "--is-ancestor", base, sha}, nil, gitQuick)
	return err == nil && res.OK()
}

// Create writes to outPath a bundle of sha that does not contain bases. It
// parks sha under a temporary ref named after the job id, because git bundle
// needs a ref and a detached or foreign commit has none, and deletes that ref
// again even when the bundle fails.
func Create(ctx context.Context, r ghx.Runner, dir, id, sha string, bases []string, outPath string) (err error) {
	ref := RefPrefix + id
	if res, err := r.Run(ctx, []string{"git", "-C", dir, "update-ref", ref, sha}, nil, gitQuick); err != nil {
		return fmt.Errorf("git update-ref: %w", err)
	} else if !res.OK() {
		return gitError("update-ref", res)
	}
	defer func() {
		// The caller's context may be what failed: clean up regardless.
		res, derr := r.Run(context.WithoutCancel(ctx), []string{"git", "-C", dir, "update-ref", "-d", ref}, nil, gitQuick)
		if derr == nil && !res.OK() {
			derr = gitError("update-ref -d", res)
		}
		if derr != nil && err == nil {
			err = derr
		}
	}()

	argv := []string{"git", "-C", dir, "bundle", "create", outPath, ref}
	for _, b := range bases {
		argv = append(argv, "^"+b)
	}
	res, runErr := r.Run(ctx, argv, nil, gitBundle)
	if runErr != nil {
		_ = os.Remove(outPath)
		return fmt.Errorf("git bundle create: %w", runErr)
	}
	if !res.OK() {
		_ = os.Remove(outPath)
		return gitError("bundle create", res)
	}
	return nil
}

func gitError(what string, res ghx.Result) error {
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = "no message"
	}
	return fmt.Errorf("git %s exited with %d: %s", what, res.ExitCode, msg)
}
