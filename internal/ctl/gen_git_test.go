package ctl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// These tests run the real internal/patch on a real repository built in a
// temporary directory; only the host is faked. They skip when git is missing.

const (
	origF   = "one\ntwo\nthree\n"
	changeF = "one\nTWO\nthree\n"
)

// genEnv is a repository with one commit, and a controller over a fake host.
type genEnv struct {
	t      *testing.T
	repo   string
	sha    string
	c      *Controller
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	api    *fakeAPI
	cache  *job.Cache
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	b, err := exec.Command("git", argv...).Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %v: %v\n%s", args, err, stderr)
	}
	return string(b)
}

func putFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileText(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

func newGenEnv(t *testing.T) *genEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "config", "core.autocrlf", "false")
	putFile(t, repo, "f.txt", origF)
	putFile(t, repo, "pkg/g.go", "package pkg\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "first")

	e := &genEnv{
		t: t, repo: repo, sha: strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD")),
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, api: newFakeAPI(), cache: job.NewCache(t.TempDir()),
	}
	be := &fakeBackend{hosts: []host.Host{running(1)}}
	e.c = New(Config{
		Runner:  ghx.ExecRunner{},
		Clock:   clock.NewFake(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)),
		Backend: be,
		API:     func(host.Host) runner.API { return e.api },
		Cache:   e.cache,
		Entropy: &counter{},
		Stdout:  e.stdout,
		Stderr:  e.stderr,
		Dir:     repo,
		Repo:    "o/r",
	})
	return e
}

// makePatch returns the patch a gen job would send when mutate changes a clone
// of the repository, computed the way the host computes it.
func (e *genEnv) makePatch(mutate func(dir string)) []byte {
	e.t.Helper()
	parent := e.t.TempDir()
	gitIn(e.t, parent, "clone", "-q", "-c", "core.autocrlf=false", e.repo, "clone")
	clone := filepath.Join(parent, "clone")
	mutate(clone)
	gitIn(e.t, clone, "add", "-A", "-N")
	return []byte(gitIn(e.t, clone, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames", "HEAD"))
}

func (e *genEnv) twoFiles() []byte {
	return e.makePatch(func(d string) {
		putFile(e.t, d, "f.txt", changeF)
		putFile(e.t, d, "pkg/new.go", "package pkg\n")
	})
}

func (e *genEnv) sensitiveFile() []byte {
	return e.makePatch(func(d string) {
		putFile(e.t, d, "gatehaul.toml", "[gate]\ncmd = \"true\"\n")
	})
}

func (e *genEnv) reset() {
	e.stdout.Reset()
	e.stderr.Reset()
}

func (e *genEnv) gen(o GenOptions) int {
	e.reset()
	return e.c.Gen(context.Background(), o)
}

func TestGenAppliesThePatchToTheWorkingTree(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))
	if code := e.gen(genOpts("gofmt", "-w", ".")); code != out.ExitOK {
		t.Fatalf("exit %d; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	lines := strings.Split(e.stdout.String(), "\n")
	want := "patch: applied 2 files (sha256 " + sumHex(data)[:12] + ")"
	if !strings.HasPrefix(lines[0], "job=1-") || !strings.HasSuffix(lines[0], " state=succeeded exit=0") || lines[1] != want {
		t.Fatalf("stdout %q, want a second line %q", e.stdout.String(), want)
	}
	if got := fileText(t, e.repo, "f.txt"); got != changeF {
		t.Fatalf("f.txt = %q", got)
	}
	if got := fileText(t, e.repo, "pkg/new.go"); got != "package pkg\n" {
		t.Fatalf("pkg/new.go = %q", got)
	}

	id := e.api.submits[0].ID
	if e.api.submits[0].Kind != runner.KindGen || e.api.submits[0].SHA != e.sha {
		t.Errorf("submitted %q at %q, want a gen job of %s", e.api.submits[0].Kind, e.api.submits[0].SHA, e.sha)
	}
	a, ok, err := e.cache.LoadApplied(id)
	if err != nil || !ok || a.SHA256 != sumHex(data) || len(a.Files) != 2 || a.AppliedAt == "" {
		t.Fatalf("applied.json: %+v, %v, %v", a, ok, err)
	}
	if got, ok, _ := e.cache.LoadPatch(id); !ok || !bytes.Equal(got, data) {
		t.Error("the patch was not kept in the cache")
	}
}

func TestGenJSONReportsTheAppliedPatch(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))
	o := genOpts("gofmt", "-w", ".")
	o.JSON = true
	if code := e.gen(o); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	var rep Report
	decode(t, e.stdout, &rep)
	p := rep.Jobs[0].Patch
	if p == nil || !p.Applied || p.SHA256 != sumHex(data) || len(p.Files) != 2 || len(p.Problems) != 0 || len(p.SensitivePaths) != 0 {
		t.Fatalf("report %+v", rep.Jobs[0])
	}
	if !strings.Contains(e.stdout.String(), `"sensitive_paths": []`) {
		t.Errorf("stdout lacks an empty sensitive_paths:\n%s", e.stdout.String())
	}
}

func TestGenRefusesASensitivePatchAndChangesNothing(t *testing.T) {
	e := newGenEnv(t)
	data := e.sensitiveFile()
	serveGen(e.api, data, sumHex(data))
	o := genOpts("true")
	o.JSON = true
	if code := e.gen(o); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	var v errJSON
	decode(t, e.stderr, &v)
	if v.Code != string(out.CodePatchRefused) || !strings.Contains(v.Fix, "--allow-sensitive") || v.ID == "" {
		t.Fatalf("error %+v", v)
	}
	var rep Report
	decode(t, e.stdout, &rep)
	p := rep.Jobs[0].Patch
	if rep.Exit != out.ExitPatchRefused || p == nil || p.Applied || len(p.Problems) == 0 || p.Problems[0].Path != "gatehaul.toml" {
		t.Fatalf("report %+v", rep)
	}
	if exists(e.repo, "gatehaul.toml") {
		t.Fatal("a refused patch changed the working tree")
	}
	if _, ok, _ := e.cache.LoadApplied(v.ID); ok {
		t.Error("a refused patch was recorded as applied")
	}
}

func TestGenTextListsOneLinePerRefusedPath(t *testing.T) {
	e := newGenEnv(t)
	data := e.sensitiveFile()
	serveGen(e.api, data, sumHex(data))
	if code := e.gen(genOpts("true")); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6", code)
	}
	if !strings.Contains(e.stdout.String(), "\nrefused gatehaul.toml: ") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
	if !strings.Contains(e.stderr.String(), "error: ") || !strings.Contains(e.stderr.String(), "\nFix: the patch was not applied; review the listed paths") {
		t.Fatalf("stderr %q", e.stderr.String())
	}
}

func TestGenAllowSensitiveLetsTheSensitivePatchThrough(t *testing.T) {
	e := newGenEnv(t)
	data := e.sensitiveFile()
	serveGen(e.api, data, sumHex(data))
	o := genOpts("true")
	o.AllowSensitive = true
	if code := e.gen(o); code != out.ExitOK {
		t.Fatalf("exit %d; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	if !exists(e.repo, "gatehaul.toml") {
		t.Fatal("the patch was not applied")
	}
	if !strings.Contains(e.stdout.String(), "\nsensitive gatehaul.toml\n") || !strings.Contains(e.stdout.String(), "patch: applied 1 files") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestGenConflictThenApplyAfterRevertingTheLocalEdit(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	serveGen(e.api, data, sumHex(data))
	putFile(t, e.repo, "f.txt", "one\nLOCAL\nthree\n") // the same line the patch changes

	o := genOpts("gofmt", "-w", ".")
	o.JSON = true
	if code := e.gen(o); code != out.ExitPatchRefused {
		t.Fatalf("exit %d, want 6; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	var v errJSON
	jsonAfterNoise(t, e.stderr, &v)
	if v.Code != string(out.CodePatchConflict) || !strings.Contains(v.Fix, "gatehaul apply "+v.ID) {
		t.Fatalf("error %+v", v)
	}
	if got := fileText(t, e.repo, "f.txt"); got != "one\nLOCAL\nthree\n" {
		t.Fatalf("a conflicting patch touched the local edit: %q", got)
	}
	if exists(e.repo, "pkg/new.go") {
		t.Fatal("a conflicting patch was partly applied")
	}
	if _, ok, _ := e.cache.LoadApplied(v.ID); ok {
		t.Fatal("a conflicting patch was recorded as applied")
	}

	putFile(t, e.repo, "f.txt", origF)
	e.reset()
	if code := e.c.Apply(context.Background(), ApplyOptions{}); code != out.ExitOK {
		t.Fatalf("apply: exit %d; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	if got := fileText(t, e.repo, "f.txt"); got != changeF || !exists(e.repo, "pkg/new.go") {
		t.Fatalf("apply left f.txt = %q", got)
	}
	if !strings.Contains(e.stdout.String(), "patch: applied 2 files") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
	if e.api.patchCalls != 1 {
		t.Errorf("%d patch fetches, want 1: apply uses the cached copy", e.api.patchCalls)
	}

	// A second apply finds it applied and does nothing.
	putFile(t, e.repo, "f.txt", origF)
	e.reset()
	if code := e.c.Apply(context.Background(), ApplyOptions{ID: v.ID}); code != out.ExitOK {
		t.Fatalf("second apply: exit %d; stderr %q", code, e.stderr.String())
	}
	if got := fileText(t, e.repo, "f.txt"); got != origF {
		t.Fatalf("a second apply applied the patch again: %q", got)
	}
	if !strings.Contains(e.stdout.String(), "patch: already applied 2 files") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestSecondWaitDoesNotApplyTheGenPatchAgain(t *testing.T) {
	e := newGenEnv(t)
	data := e.twoFiles()
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "running", HostID: "hid-1"}}, nil
	}
	o := genOpts("gofmt", "-w", ".")
	o.Block = time.Second
	if code := e.gen(o); code != out.ExitRunning {
		t.Fatalf("gen: exit %d, want 3; stderr %q", code, e.stderr.String())
	}
	id := e.api.submits[0].ID
	if exists(e.repo, "pkg/new.go") {
		t.Fatal("a patch was applied before the job ended")
	}

	serveGen(e.api, data, sumHex(data))
	e.reset()
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("wait: exit %d; stdout %q; stderr %q", code, e.stdout.String(), e.stderr.String())
	}
	if got := fileText(t, e.repo, "f.txt"); got != changeF || !strings.Contains(e.stdout.String(), "patch: applied 2 files") {
		t.Fatalf("wait did not apply the patch: f.txt = %q, stdout %q", got, e.stdout.String())
	}

	putFile(t, e.repo, "f.txt", origF)
	e.reset()
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("second wait: exit %d; stderr %q", code, e.stderr.String())
	}
	if got := fileText(t, e.repo, "f.txt"); got != origF {
		t.Fatalf("the second wait applied the patch again: %q", got)
	}
	if !strings.Contains(e.stdout.String(), "patch: already applied 2 files") || e.api.patchCalls != 1 {
		t.Fatalf("stdout %q, %d patch fetches", e.stdout.String(), e.api.patchCalls)
	}
}

func TestApplyAllowSensitiveAfterARefusal(t *testing.T) {
	e := newGenEnv(t)
	data := e.sensitiveFile()
	serveGen(e.api, data, sumHex(data))
	if code := e.gen(genOpts("true")); code != out.ExitPatchRefused {
		t.Fatalf("gen: exit %d, want 6", code)
	}
	e.reset()
	if code := e.c.Apply(context.Background(), ApplyOptions{}); code != out.ExitPatchRefused {
		t.Fatalf("apply: exit %d, want 6 again", code)
	}
	if exists(e.repo, "gatehaul.toml") {
		t.Fatal("a refused patch changed the working tree")
	}
	e.reset()
	if code := e.c.Apply(context.Background(), ApplyOptions{AllowSensitive: true}); code != out.ExitOK {
		t.Fatalf("apply --allow-sensitive: exit %d; stderr %q", code, e.stderr.String())
	}
	if !exists(e.repo, "gatehaul.toml") || !strings.Contains(e.stdout.String(), "sensitive gatehaul.toml") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
	if e.api.patchCalls != 1 {
		t.Errorf("%d patch fetches, want 1", e.api.patchCalls)
	}
}
