package ctl

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/host/fake"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// e2eEnv is a real repository, a real runner on the fake backend and a real
// clock: only the laptop's cache and controller are rebuilt between "restarts".
type e2eEnv struct {
	t       *testing.T
	repo    string
	cache   string
	backend *fake.Backend
}

func newE2E(t *testing.T) *e2eEnv {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the end-to-end test needs linux")
	}
	for _, tool := range []string{"bash", "git", "flock", "setsid", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f.txt")
	git("commit", "-q", "-m", "first")
	return &e2eEnv{t: t, repo: repo, cache: t.TempDir(), backend: fake.New(t.TempDir(), 1)}
}

// controller builds a fresh controller over the same cache and backend.
func (e *e2eEnv) controller() (*Controller, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	c := New(Config{
		Runner: ghx.ExecRunner{}, Clock: clock.Real{}, Backend: e.backend,
		Cache: job.NewCache(e.cache), Stdout: &stdout, Stderr: &stderr, Dir: e.repo, Repo: "o/r",
	})
	return c, &stdout, &stderr
}

func TestEndToEndRunThenWaitAfterARestart(t *testing.T) {
	e := newE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, stdout, stderr := e.controller()
	code := c.Run(ctx, RunOptions{Prio: DefaultPrio, Timeout: time.Minute, Block: time.Minute, Cmd: []string{"echo ok"}})
	if code != out.ExitOK {
		t.Fatalf("run: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), "state=succeeded exit=0") || !strings.Contains(stdout.String(), "ok") {
		t.Fatalf("run stdout %q", stdout.String())
	}
	id := strings.TrimPrefix(strings.Fields(stdout.String())[0], "job=")

	// The laptop restarted: a new controller with the same cache finds the job.
	c2, stdout2, stderr2 := e.controller()
	if code := c2.Wait(ctx, WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("wait: exit %d\nstdout %s\nstderr %s", code, stdout2, stderr2)
	}
	if !strings.HasPrefix(stdout2.String(), "job="+id+" state=succeeded") {
		t.Fatalf("wait stdout %q", stdout2.String())
	}
}

func TestEndToEndRecreatedHostLosesItsJobsAsHostReplaced(t *testing.T) {
	e := newE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, stdout, stderr := e.controller()
	code := c.Run(ctx, RunOptions{Prio: DefaultPrio, Timeout: time.Minute, Block: time.Second, JSON: true, Cmd: []string{"sleep 3"}})
	if code != out.ExitRunning {
		t.Fatalf("run: exit %d, want 3\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	var first Report
	decode(t, stdout, &first)
	if len(first.PendingIDs) != 1 {
		t.Fatalf("report %+v", first)
	}
	id := first.PendingIDs[0]

	// The codespace is deleted and created again under the same name.
	hosts, err := e.backend.Discover(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts %v, %v", hosts, err)
	}
	if err := e.backend.Delete(ctx, hosts[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backend.Create(ctx, host.CreateOptions{Num: 1}); err != nil {
		t.Fatal(err)
	}

	c2, stdout2, stderr2 := e.controller()
	code = c2.Wait(ctx, WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, stdout2, &rep)
	if code != out.ExitInfra || rep.Jobs[0].State != "lost" || rep.Jobs[0].Code != "host_replaced" {
		t.Fatalf("wait: exit %d, report %+v\nstderr %s", code, rep, stderr2)
	}
}

func TestEndToEndLostAnswerIsOutcomeUnknownNeverADoubleJob(t *testing.T) {
	e := newE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Cut the submit's reply only: the probe that comes first must get through.
	e.backend.CutAfter = `"status":"accepted"`
	c, stdout, stderr := e.controller()
	code := c.Run(ctx, RunOptions{Prio: DefaultPrio, Timeout: time.Minute, Block: time.Minute, JSON: true, Cmd: []string{"echo once"}})
	if code != out.ExitInfra {
		t.Fatalf("run: exit %d, want 5\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	first, rest, _ := strings.Cut(stderr.String(), "\n")
	id := strings.TrimPrefix(first, "next: gatehaul wait ")
	if id == first || !strings.Contains(rest, `"outcome_unknown"`) {
		t.Fatalf("stderr %q", stderr.String())
	}

	e.backend.CutAfter = ""
	c2, stdout2, stderr2 := e.controller()
	if code := c2.Wait(ctx, WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("wait: exit %d\nstdout %s\nstderr %s", code, stdout2, stderr2)
	}
	if !strings.Contains(stdout2.String(), "state=succeeded") {
		t.Fatalf("wait stdout %q", stdout2.String())
	}

	hosts, err := e.backend.Discover(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts %v, %v", hosts, err)
	}
	jobs, err := filepath.Glob(filepath.Join(e.backend.Root(hosts[0]), "jobs", "*"))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs on the host: %v (%v), want exactly one", jobs, err)
	}
}

func TestEndToEndGenAppliesThePatchOnceEvenAfterARestart(t *testing.T) {
	e := newE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, stdout, stderr := e.controller()
	code := c.Gen(ctx, GenOptions{RunOptions: RunOptions{Prio: DefaultPrio, Timeout: time.Minute, Block: time.Minute, Cmd: []string{"echo changed > f.txt"}}})
	if code != out.ExitOK {
		t.Fatalf("gen: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), "state=succeeded exit=0") || !strings.Contains(stdout.String(), "patch: applied 1 files") {
		t.Fatalf("gen stdout %q", stdout.String())
	}
	got, err := os.ReadFile(filepath.Join(e.repo, "f.txt"))
	if err != nil || string(got) != "changed\n" {
		t.Fatalf("f.txt = %q, %v; want the command's change applied locally", got, err)
	}
	id := strings.TrimPrefix(strings.Fields(stdout.String())[0], "job=")

	// The laptop restarted and the file was edited again: waiting for the same
	// job must not apply its patch a second time.
	if err := os.WriteFile(filepath.Join(e.repo, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, stdout2, stderr2 := e.controller()
	if code := c2.Wait(ctx, WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("wait: exit %d\nstdout %s\nstderr %s", code, stdout2, stderr2)
	}
	if !strings.Contains(stdout2.String(), "patch: already applied 1 files") {
		t.Fatalf("wait stdout %q", stdout2.String())
	}
	if got, _ := os.ReadFile(filepath.Join(e.repo, "f.txt")); string(got) != "hello\n" {
		t.Fatalf("f.txt = %q after the second wait", got)
	}
}
