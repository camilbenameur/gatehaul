package patch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

// These tests run ApplySafely and Recover on real repositories in temporary
// directories. A failure of the write phase, and a process that dies, are
// simulated by a runner that does the real "git apply" and then fails or
// panics.

const (
	apOrig    = "one\ntwo\nthree\n"
	apChanged = "one\nTWO\nthree\n"
)

func apGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="}, args...)
	cmd := exec.Command("git", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func apWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func apRead(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func apExists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// apFix is a repository with one commit, the patch that edits f.txt, deletes
// pkg/g.go and adds pkg/sub/deep/new.go (in a directory that does not exist),
// and an empty state directory.
type apFix struct {
	repo     string
	stateDir string
	data     []byte
	files    []string
}

func apNewFix(t *testing.T) *apFix {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	repo := t.TempDir()
	apGit(t, repo, "init", "-q", "-b", "main")
	apGit(t, repo, "config", "core.autocrlf", "false")
	apWrite(t, repo, "f.txt", apOrig)
	apWrite(t, repo, "pkg/g.go", "package pkg\n")
	apGit(t, repo, "add", "-A")
	apGit(t, repo, "commit", "-q", "-m", "first")

	apWrite(t, repo, "f.txt", apChanged)
	apWrite(t, repo, "pkg/sub/deep/new.go", "package deep\n")
	if err := os.Remove(filepath.Join(repo, "pkg", "g.go")); err != nil {
		t.Fatal(err)
	}
	apGit(t, repo, "add", "-A", "-N")
	data := apGit(t, repo, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames", "HEAD")
	apGit(t, repo, "reset", "-q", "--hard")
	apGit(t, repo, "clean", "-fdq")

	return &apFix{
		repo: repo, stateDir: t.TempDir(), data: []byte(data),
		files: []string{"f.txt", "pkg/g.go", "pkg/sub/deep/new.go"},
	}
}

func (f *apFix) apply(t *testing.T, r ghx.Runner) error {
	t.Helper()
	return ApplySafely(context.Background(), r, f.repo, f.data, f.files, f.stateDir)
}

// wantOriginal fails unless the work tree is as the commit left it.
func (f *apFix) wantOriginal(t *testing.T) {
	t.Helper()
	if got := apRead(t, f.repo, "f.txt"); got != apOrig {
		t.Errorf("f.txt = %q, want the original", got)
	}
	if !apExists(f.repo, "pkg/g.go") || apRead(t, f.repo, "pkg/g.go") != "package pkg\n" {
		t.Error("pkg/g.go was not put back")
	}
	if apExists(f.repo, "pkg/sub") {
		t.Error("pkg/sub, which the patch creates, is still there")
	}
	if st := apGit(t, f.repo, "status", "--porcelain"); st != "" {
		t.Errorf("git status:\n%s", st)
	}
	f.wantNoState(t)
}

// wantPatched fails unless the patch is in the work tree.
func (f *apFix) wantPatched(t *testing.T) {
	t.Helper()
	if got := apRead(t, f.repo, "f.txt"); got != apChanged {
		t.Errorf("f.txt = %q, want the patched text", got)
	}
	if apExists(f.repo, "pkg/g.go") {
		t.Error("pkg/g.go should be deleted")
	}
	if got := apRead(t, f.repo, "pkg/sub/deep/new.go"); got != "package deep\n" {
		t.Errorf("new.go = %q", got)
	}
}

func (f *apFix) wantNoState(t *testing.T) {
	t.Helper()
	for _, name := range []string{applyingMarker, preimageDir} {
		if apExists(f.stateDir, name) {
			t.Errorf("%s is still in the state directory", name)
		}
	}
}

// apHook is a runner that runs git for real. before runs ahead of the real
// "git apply" (not its --check) and after may replace its outcome; the
// environments given to every "git apply" are kept.
type apHook struct {
	ghx.ExecRunner
	before func()
	after  func(res ghx.Result, err error) (ghx.Result, error)
	envs   [][]string
}

func (h *apHook) RunEnv(ctx context.Context, env, argv []string, stdin io.Reader, timeout time.Duration) (ghx.Result, error) {
	isApply := slices.Contains(argv, "apply")
	write := isApply && !slices.Contains(argv, "--check") && !slices.Contains(argv, "--reverse")
	if write && h.before != nil {
		h.before()
	}
	res, err := h.ExecRunner.RunEnv(ctx, env, argv, stdin, timeout)
	if isApply {
		h.envs = append(h.envs, env)
	}
	if write && h.after != nil {
		return h.after(res, err)
	}
	return res, err
}

// apCrash runs f and reports whether it panicked: a process that dies leaves
// whatever was on disk, and nothing runs after the panic.
func apCrash(f func()) (crashed bool) {
	defer func() {
		if recover() != nil {
			crashed = true
		}
	}()
	f()
	return false
}

func TestApplySafelyAppliesThePatchAndClearsItsState(t *testing.T) {
	f := apNewFix(t)
	if err := f.apply(t, ghx.ExecRunner{}); err != nil {
		t.Fatal(err)
	}
	f.wantPatched(t)
	f.wantNoState(t)
}

// apWriteFails runs ApplySafely over a runner whose real "git apply" writes the
// files and then fails as after says, and expects the tree back as it was.
func apWriteFails(t *testing.T, after func(ghx.Result, error) (ghx.Result, error), want func(error) bool) {
	t.Helper()
	f := apNewFix(t)
	err := f.apply(t, &apHook{after: after})
	if err == nil || !want(err) {
		t.Fatalf("err = %v", err)
	}
	var ae *ApplyError
	if errors.As(err, &ae) {
		t.Fatalf("a failed write is not a conflict: %v", err)
	}
	f.wantOriginal(t)

	// The retry works: nothing is left of the first try.
	if err := f.apply(t, ghx.ExecRunner{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	f.wantPatched(t)
}

func TestApplySafelyRestoresTheTreeWhenGitExitsNonZero(t *testing.T) {
	after := func(ghx.Result, error) (ghx.Result, error) {
		return ghx.Result{ExitCode: 1, Stderr: []byte("error: unable to write file f.txt\n")}, nil
	}
	apWriteFails(t, after, func(err error) bool { return strings.Contains(err.Error(), "unable to write") })
}

func TestApplySafelyRestoresTheTreeWhenGitIsKilled(t *testing.T) {
	killed := errors.New("signal: killed")
	after := func(ghx.Result, error) (ghx.Result, error) { return ghx.Result{}, killed }
	apWriteFails(t, after, func(err error) bool { return errors.Is(err, killed) })
}

func TestApplySafelyWriteIgnoresTheCancellationOfItsContext(t *testing.T) {
	f := apNewFix(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &apHook{before: cancel}
	if err := ApplySafely(ctx, h, f.repo, f.data, f.files, f.stateDir); err != nil {
		t.Fatalf("a Ctrl-C during the write must not stop it: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the hook did not cancel the context")
	}
	f.wantPatched(t)
	f.wantNoState(t)
}

func TestApplySafelyStopsBeforeTheWriteWhenTheContextIsCancelled(t *testing.T) {
	f := apNewFix(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ApplySafely(ctx, ghx.ExecRunner{}, f.repo, f.data, f.files, f.stateDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	f.wantOriginal(t)
}

func TestRecoverRestoresTheFilesOfAnApplyThatDied(t *testing.T) {
	f := apNewFix(t)
	h := &apHook{after: func(ghx.Result, error) (ghx.Result, error) { panic("the process died") }}
	if !apCrash(func() { _ = f.apply(t, h) }) {
		t.Fatal("the simulated crash did not happen")
	}
	f.wantPatched(t)
	if !apExists(f.stateDir, applyingMarker) {
		t.Fatal("a process that died left no marker")
	}

	// Nothing is applied on top of a tree that is not restored.
	if err := f.apply(t, ghx.ExecRunner{}); !errors.Is(err, ErrUnrecovered) {
		t.Fatalf("err = %v, want ErrUnrecovered", err)
	}

	changed, err := Recover(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	if want := []string{"f.txt", "pkg/g.go", "pkg/sub/deep/new.go"}; !slices.Equal(changed, want) {
		t.Fatalf("Recover changed %v, want %v", changed, want)
	}
	f.wantOriginal(t)

	if changed, err := Recover(f.stateDir); err != nil || changed != nil {
		t.Fatalf("second Recover = %v, %v; want nothing to do", changed, err)
	}
	if err := f.apply(t, ghx.ExecRunner{}); err != nil {
		t.Fatalf("apply after recovery: %v", err)
	}
	f.wantPatched(t)
}

func TestRecoverLeavesFilesThatWereNotWrittenAlone(t *testing.T) {
	f := apNewFix(t)
	h := &apHook{before: func() { panic("the process died before git wrote anything") }}
	if !apCrash(func() { _ = f.apply(t, h) }) {
		t.Fatal("the simulated crash did not happen")
	}
	changed, err := Recover(f.stateDir)
	if err != nil || changed == nil || len(changed) != 0 {
		t.Fatalf("Recover = %v, %v; want an empty, non-nil list: there was a marker and nothing to put back", changed, err)
	}
	f.wantOriginal(t)
}

func TestRecoverWithoutAMarkerDoesNothing(t *testing.T) {
	changed, err := Recover(t.TempDir())
	if err != nil || changed != nil {
		t.Fatalf("Recover = %v, %v", changed, err)
	}
}

func TestRecoverKeepsTheMarkerWhenTheSavedFilesAreGone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, applyingMarker), []byte("applying\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(dir); err == nil {
		t.Fatal("Recover succeeded without a manifest")
	}
	if !apExists(dir, applyingMarker) {
		t.Fatal("the marker was removed although nothing was restored")
	}
}

func TestApplySafelyReportsAPatchThatIsAlreadyInTheTree(t *testing.T) {
	f := apNewFix(t)
	if err := f.apply(t, ghx.ExecRunner{}); err != nil {
		t.Fatal(err)
	}
	// A second try, as after a lost record: the forward patch no longer fits,
	// but its reverse does.
	err := ApplySafely(context.Background(), ghx.ExecRunner{}, f.repo, f.data, f.files, t.TempDir())
	if !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("err = %v, want ErrAlreadyApplied", err)
	}
	f.wantPatched(t)
}

func TestApplySafelyConflictChangesNothing(t *testing.T) {
	f := apNewFix(t)
	apWrite(t, f.repo, "f.txt", "one\nLOCAL\nthree\n")
	err := f.apply(t, ghx.ExecRunner{})
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *ApplyError", err)
	}
	if !slices.Contains(ae.Paths, "f.txt") {
		t.Errorf("conflict paths %v lack f.txt", ae.Paths)
	}
	if got := apRead(t, f.repo, "f.txt"); got != "one\nLOCAL\nthree\n" {
		t.Errorf("f.txt = %q: the local edit was touched", got)
	}
	if apExists(f.repo, "pkg/sub") || !apExists(f.repo, "pkg/g.go") {
		t.Error("a patch that does not fit was partly applied")
	}
	f.wantNoState(t)
}

func TestApplySafelyRunsGitWithTheCLocale(t *testing.T) {
	f := apNewFix(t)
	h := &apHook{}
	if err := f.apply(t, h); err != nil {
		t.Fatal(err)
	}
	if len(h.envs) != 2 {
		t.Fatalf("%d git apply calls, want the check and the write", len(h.envs))
	}
	for _, env := range h.envs {
		if !slices.Contains(env, "LC_ALL=C") {
			t.Errorf("git apply ran with env %v, want LC_ALL=C", env)
		}
	}
}

func TestApplySafelyRefusesAPathOutsideTheTree(t *testing.T) {
	f := apNewFix(t)
	f.files = []string{"f.txt", "../outside.txt"}
	if err := f.apply(t, ghx.ExecRunner{}); err == nil {
		t.Fatal("a path outside the work tree was accepted")
	}
	f.wantOriginal(t)
}
