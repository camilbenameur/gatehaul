package ctl

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// These tests run the real internal/patch on real repositories, as the tests
// of gen_git_test.go do; only the host is faked. They cover what ties a patch
// to its base and its tree, and what keeps a write from leaving a half-applied
// tree.

// runningGen dispatches a gen job that is still running when gen returns, and
// returns its id.
func (e *genEnv) runningGen(o GenOptions) string {
	e.t.Helper()
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "running", HostID: "hid-1"}}, nil
	}
	o.Block = time.Second
	if code := e.gen(o); code != out.ExitRunning {
		e.t.Fatalf("gen: exit %d, want 3; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	return e.api.submits[0].ID
}

// wait runs wait on the job with the JSON output, and decodes the report.
func (e *genEnv) wait(id string) (int, Report) {
	e.t.Helper()
	e.reset()
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var rep Report
	decode(e.t, e.stdout, &rep)
	return code, rep
}

func (e *genEnv) commitFile(rel, content string) {
	e.t.Helper()
	putFile(e.t, e.repo, rel, content)
	gitIn(e.t, e.repo, "add", "-A")
	gitIn(e.t, e.repo, "commit", "-q", "-m", "more "+rel)
}

// controllerWith returns a controller like e.c with its own output buffers.
func (e *genEnv) controllerWith(mutate func(*Config)) (*Controller, *bytes.Buffer, *bytes.Buffer) {
	cfg := e.c.Config
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cfg.Stdout, cfg.Stderr = stdout, stderr
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg), stdout, stderr
}

// applyHook is the real runner, except that the outcome of the real "git
// apply" of a patch (not its checks) is whatever after makes of it, once git
// has written the files.
type applyHook struct {
	ghx.ExecRunner
	after func(res ghx.Result, err error) (ghx.Result, error)
}

func (h applyHook) RunEnv(ctx context.Context, env, argv []string, stdin io.Reader, timeout time.Duration) (ghx.Result, error) {
	res, err := h.ExecRunner.RunEnv(ctx, env, argv, stdin, timeout)
	isWrite := slices.Contains(argv, "apply") && !slices.Contains(argv, "--check") &&
		!slices.Contains(argv, "--cached") && !slices.Contains(argv, "--reverse")
	if isWrite {
		return h.after(res, err)
	}
	return res, err
}

// diesIn runs f and reports whether it panicked: a process that dies leaves
// whatever was on disk.
func diesIn(f func()) (died bool) {
	defer func() {
		if recover() != nil {
			died = true
		}
	}()
	f()
	return false
}

func (e *genEnv) wantTreeAsCommitted() {
	e.t.Helper()
	if got := fileText(e.t, e.repo, "f.txt"); got != origF {
		e.t.Errorf("f.txt = %q, want the committed text", got)
	}
	if exists(e.repo, "pkg/new.go") {
		e.t.Error("pkg/new.go is in the tree")
	}
}

func (e *genEnv) wantPatchedTree() {
	e.t.Helper()
	if got := fileText(e.t, e.repo, "f.txt"); got != changeF {
		e.t.Errorf("f.txt = %q, want the patched text", got)
	}
	if !exists(e.repo, "pkg/new.go") {
		e.t.Error("pkg/new.go is not in the tree")
	}
}

func (e *genEnv) stateFile(id, name string) string {
	return filepath.Join(e.cache.JobDir(id), name)
}

func TestWaitRefusesAPatchWhoseBaseMovedOn(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))

	// A commit changes f.txt, which the patch edits too: the patch would still
	// fit, but it was made for another version of the file.
	e.commitFile("f.txt", "one\ntwo\nthree\nfour\n")
	serveGen(e.api, data, sumHex(data))
	code, rep := e.wait(id)
	if code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stderr %q", code, e.stderr.String())
	}
	j := rep.Jobs[0]
	if j.Code != string(out.CodeStaleBase) || j.Patch == nil || j.Patch.Applied {
		t.Fatalf("report %+v", j)
	}
	var v errJSON
	jsonAfterNoise(t, e.stderr, &v)
	if v.Code != string(out.CodeStaleBase) || v.ID != id || !strings.Contains(v.Fix, "git checkout "+e.sha) || !strings.Contains(v.Fix, "gatehaul apply "+id) {
		t.Fatalf("error %+v", v)
	}
	if got := fileText(t, e.repo, "f.txt"); got != "one\ntwo\nthree\nfour\n" || exists(e.repo, "pkg/new.go") {
		t.Fatalf("a stale patch changed the tree: f.txt = %q", got)
	}
	if _, ok, _ := e.cache.LoadApplied(id); ok {
		t.Error("a stale patch was recorded as applied")
	}
}

func TestWaitAppliesAPatchWhenHeadMovedOnWithoutTouchingItsFiles(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))

	e.commitFile("other.txt", "unrelated\n") // the dispatched commit is an ancestor of HEAD
	serveGen(e.api, data, sumHex(data))
	code, rep := e.wait(id)
	if code != out.ExitOK {
		t.Fatalf("exit %d, want 0; stderr %q", code, e.stderr.String())
	}
	if p := rep.Jobs[0].Patch; p == nil || !p.Applied || p.AlreadyApplied {
		t.Fatalf("report %+v", rep.Jobs[0])
	}
	e.wantPatchedTree()
}

func TestWaitRefusesAPatchWhenTheDispatchedCommitIsNotAnAncestor(t *testing.T) {
	e := newGenEnv(t)
	e.commitFile("other.txt", "unrelated\n")
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))

	// Another branch, forked before the dispatched commit: it has the same
	// files, but the commit the job ran on is not in its history.
	gitIn(t, e.repo, "checkout", "-q", "-b", "side", "HEAD~1")
	e.commitFile("side.txt", "side\n")
	serveGen(e.api, data, sumHex(data))
	code, rep := e.wait(id)
	if code != out.ExitPatchRefused || rep.Jobs[0].Code != string(out.CodeStaleBase) {
		t.Fatalf("exit %d, report %+v; stderr %q", code, rep.Jobs[0], e.stderr.String())
	}
	if !strings.Contains(e.stderr.String(), "not one of its ancestors") {
		t.Errorf("stderr %q does not say why", e.stderr.String())
	}
	e.wantTreeAsCommitted()
}

func TestGenOnAnotherRefDoesNotApplyToTheCurrentBranch(t *testing.T) {
	e := newGenEnv(t)
	gitIn(t, e.repo, "checkout", "-q", "-b", "feature")
	e.commitFile("f.txt", "one\ntwo\nthree\nfour\n")
	gitIn(t, e.repo, "checkout", "-q", "main")
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))

	// The job runs on feature, but the tree is on main.
	o := genOpts("gofmt", "-w", ".")
	o.Ref = "feature"
	o.JSON = true
	if code := e.gen(o); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	var rep Report
	decode(t, e.stdout, &rep)
	if rep.Jobs[0].Code != string(out.CodeStaleBase) {
		t.Fatalf("report %+v", rep.Jobs[0])
	}
	e.wantTreeAsCommitted()
}

func TestWaitFromAnotherWorkTreeIsRefused(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))

	// A second clone of the same repository (the same owner/repo).
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	putFile(t, other, "f.txt", origF)
	gitIn(t, other, "add", "-A")
	gitIn(t, other, "commit", "-q", "-m", "first")
	c2, stdout2, stderr2 := e.controllerWith(func(cfg *Config) { cfg.Dir = other })

	serveGen(e.api, data, sumHex(data))
	code := c2.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, stdout2, &rep)
	if code != out.ExitPatchRefused || rep.Jobs[0].Code != string(out.CodeWrongRepo) {
		t.Fatalf("exit %d, report %+v; stderr %q", code, rep.Jobs[0], stderr2.String())
	}
	if got := fileText(t, other, "f.txt"); got != origF {
		t.Errorf("the other work tree was written: f.txt = %q", got)
	}
	if _, ok, _ := e.cache.LoadApplied(id); ok {
		t.Fatal("a refused patch was recorded as applied: the right tree would never get it")
	}

	// The right tree still gets it.
	if code, _ := e.wait(id); code != out.ExitOK {
		t.Fatalf("wait in the right tree: exit %d; stderr %q", code, e.stderr.String())
	}
	e.wantPatchedTree()
}

// wantWriteFailureUndone runs wait with a runner whose real "git apply" writes
// the files and then fails as after says, and expects the tree back as it was.
func wantWriteFailureUndone(t *testing.T, after func(ghx.Result, error) (ghx.Result, error)) {
	t.Helper()
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))
	serveGen(e.api, data, sumHex(data))

	c2, stdout2, stderr2 := e.controllerWith(func(cfg *Config) { cfg.Runner = applyHook{after: after} })
	code := c2.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, stdout2, &rep)
	if code != out.ExitInfra || rep.Jobs[0].Code != string(out.CodePatchError) {
		t.Fatalf("exit %d, report %+v; stderr %q", code, rep.Jobs[0], stderr2.String())
	}
	if rep.Jobs[0].Next != "gatehaul apply "+id {
		t.Errorf("next = %q", rep.Jobs[0].Next)
	}
	e.wantTreeAsCommitted()
	if st := gitIn(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("git status shows changes: %q", st)
	}
	if exists(e.cache.JobDir(id), "applying") {
		t.Error("the apply marker was left behind")
	}
	if _, ok, _ := e.cache.LoadApplied(id); ok {
		t.Error("a failed write was recorded as applied")
	}

	// The retry is not told that the tree conflicts with the patch.
	e.reset()
	if code := e.c.Apply(context.Background(), ApplyOptions{ID: id}); code != out.ExitOK {
		t.Fatalf("retry: exit %d; stderr %q", code, e.stderr.String())
	}
	e.wantPatchedTree()
}

func TestAWriteThatExitsNonZeroHalfWayLeavesTheTreeAsItWas(t *testing.T) {
	wantWriteFailureUndone(t, func(ghx.Result, error) (ghx.Result, error) {
		return ghx.Result{ExitCode: 1, Stderr: []byte("error: unable to write file pkg/new.go\n")}, nil
	})
}

func TestAWriteThatIsKilledHalfWayLeavesTheTreeAsItWas(t *testing.T) {
	wantWriteFailureUndone(t, func(ghx.Result, error) (ghx.Result, error) {
		return ghx.Result{}, io.ErrUnexpectedEOF
	})
}

func TestAStaleApplyMarkerIsRestoredBeforeAnythingElse(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))
	serveGen(e.api, data, sumHex(data))

	// The process dies once git has written the files: no restore, no record.
	dying, _, _ := e.controllerWith(func(cfg *Config) {
		cfg.Runner = applyHook{after: func(ghx.Result, error) (ghx.Result, error) { panic("the process died") }}
	})
	if !diesIn(func() { dying.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}) }) {
		t.Fatal("the simulated crash did not happen")
	}
	e.wantPatchedTree()
	if !exists(e.cache.JobDir(id), "applying") {
		t.Fatal("a process that died left no marker")
	}
	if _, ok, _ := e.cache.LoadApplied(id); ok {
		t.Fatal("applied.json was written by a process that died")
	}

	e.reset()
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute})
	if code != out.ExitOK {
		t.Fatalf("exit %d; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	if !strings.Contains(e.stderr.String(), "did not end; the working tree was restored first (2 file(s) put back") {
		t.Errorf("stderr %q does not say that the tree was restored", e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "patch: applied 2 files") {
		t.Errorf("stdout %q", e.stdout.String())
	}
	e.wantPatchedTree()
	if exists(e.cache.JobDir(id), "applying") {
		t.Error("the marker is still there")
	}
	if _, ok, _ := e.cache.LoadApplied(id); !ok {
		t.Error("the patch was not recorded")
	}
}

func TestTwoAppliesOfTheSameJobAreSerialised(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	id := e.runningGen(genOpts("gofmt", "-w", "."))
	serveGen(e.api, data, sumHex(data))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	stdouts := make([]string, 2)
	for i := range codes {
		c, stdout, _ := e.controllerWith(nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.Apply(context.Background(), ApplyOptions{ID: id})
			stdouts[i] = stdout.String()
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != out.ExitOK {
			t.Errorf("apply %d: exit %d; stdout %q", i, code, stdouts[i])
		}
	}
	all := strings.Join(stdouts, "\n")
	if n := strings.Count(all, "patch: applied 2 files"); n != 1 {
		t.Errorf("%d calls applied the patch, want exactly one; stdout:\n%s", n, all)
	}
	if n := strings.Count(all, "patch: already applied 2 files"); n != 1 {
		t.Errorf("%d calls reported it as already applied, want exactly one; stdout:\n%s", n, all)
	}
	e.wantPatchedTree()
	if _, ok, _ := e.cache.LoadApplied(id); !ok {
		t.Error("the patch was not recorded")
	}
}

func TestALostAppliedRecordIsRebuiltFromTheTree(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))
	if code := e.gen(genOpts("gofmt", "-w", ".")); code != out.ExitOK {
		t.Fatalf("gen: exit %d; stderr %q", code, e.stderr.String())
	}
	id := e.api.submits[0].ID
	e.wantPatchedTree()

	// The record is lost (a failed rename, a process killed before it).
	if err := os.Remove(e.stateFile(id, "applied.json")); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() int{
		func() int { return e.c.Apply(context.Background(), ApplyOptions{}) },
		func() int { return e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}) },
	} {
		e.reset()
		if code := call(); code != out.ExitOK {
			t.Fatalf("exit %d, want 0; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
		}
		if !strings.Contains(e.stdout.String(), "patch: already applied 2 files") || strings.Contains(e.stderr.String(), "error") {
			t.Fatalf("stdout %q; stderr %q", e.stdout.String(), e.stderr.String())
		}
		if a, ok, err := e.cache.LoadApplied(id); err != nil || !ok || a.SHA256 != sumHex(data) {
			t.Fatalf("applied.json: %+v, %v, %v", a, ok, err)
		}
		// Again from nothing: both ways give the same answer.
		if err := os.Remove(e.stateFile(id, "applied.json")); err != nil {
			t.Fatal(err)
		}
	}
	e.wantPatchedTree()
}

func TestAConflictDoesNotBlameLocalEditsWhenGitSaysAFileIsInTheWay(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))
	putFile(t, e.repo, "pkg/new.go", "package other\n") // an untracked file where the patch adds one
	o := genOpts("gofmt", "-w", ".")
	o.JSON = true
	if code := e.gen(o); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stderr %q", code, e.stderr.String())
	}
	var v errJSON
	jsonAfterNoise(t, e.stderr, &v)
	if v.Code != string(out.CodePatchConflict) || !strings.Contains(v.Fix, "already exists") || strings.Contains(v.Fix, "local") {
		t.Fatalf("error %+v", v)
	}
	if !strings.Contains(e.stderr.String(), "pkg/new.go") {
		t.Errorf("stderr %q does not name the file", e.stderr.String())
	}
}

func TestAllowSensitiveOfGenIsKeptForWait(t *testing.T) {
	e := newGenEnv(t)
	data := e.sensitiveFile()
	o := genOpts("true")
	o.AllowSensitive = true
	id := e.runningGen(o)
	if d, err := e.cache.LoadDispatch(id); err != nil || !d.AllowSensitive {
		t.Fatalf("dispatch %+v, %v", d, err)
	}

	serveGen(e.api, data, sumHex(data))
	if code, _ := e.wait(id); code != out.ExitOK {
		t.Fatalf("wait: exit %d; stderr %q", code, e.stderr.String())
	}
	if !exists(e.repo, "gatehaul.toml") {
		t.Fatal("wait lost the --allow-sensitive of gen")
	}
}

func TestApplyWithoutAnIDReportsAJobThatIsStillRunning(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seedGen(t, e, id)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: id, State: "running"}); err != nil {
		t.Fatal(err)
	}
	e.api.waitFn = e.advanceWait("running")

	if code := e.c.Apply(context.Background(), ApplyOptions{JSON: true}); code != out.ExitRunning {
		t.Fatalf("exit %d, want 3; stderr %q", code, e.stderr.String())
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodeJobNotDone) || v.Fix != "gatehaul wait "+id || v.ID != id {
		t.Fatalf("error %+v", v)
	}
	if len(e.api.submits) != 0 {
		t.Fatal("apply must not dispatch a second job")
	}
}

func TestApplyWithoutAnIDTakesTheLatestGenJobWhateverItsState(t *testing.T) {
	e := newEnv(t, running(1))
	older, newer := "1-01JB0000000000000000000001", "1-01JB0000000000000000000002"
	seedGen(t, e, older)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: older, State: "succeeded", PatchSHA256: strings.Repeat("c", 64), PatchSize: 10}); err != nil {
		t.Fatal(err)
	}
	seedGen(t, e, newer)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: newer, State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if code := e.c.Apply(context.Background(), ApplyOptions{JSON: true}); code != out.ExitRed {
		t.Fatalf("exit %d, want 1 (the newest job failed); stderr %q", code, e.stderr.String())
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodeNoPatch) || v.ID != newer {
		t.Fatalf("error %+v: a stale patch of an older job must not be brought back", v)
	}
	if e.api.patchCalls != 0 {
		t.Fatalf("%d patch fetches", e.api.patchCalls)
	}
}
