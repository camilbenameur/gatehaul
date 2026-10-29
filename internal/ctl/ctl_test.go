package ctl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

var headSHA = strings.Repeat("a", 40)

// counter is a deterministic entropy source.
type counter struct{ n byte }

func (r *counter) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

// scriptRunner is a ghx.Fake that also creates the bundle file git would write.
type scriptRunner struct{ fake *ghx.Fake }

func (s scriptRunner) Run(ctx context.Context, argv []string, stdin io.Reader, timeout time.Duration) (ghx.Result, error) {
	if len(argv) >= 6 && argv[3] == "bundle" && argv[4] == "create" {
		if err := os.WriteFile(argv[5], []byte("BUNDLE"), 0o600); err != nil {
			return ghx.Result{}, err
		}
	}
	return s.fake.Run(ctx, argv, stdin, timeout)
}

func gitScript() *ghx.Fake {
	d := []string{"git", "-C", "."}
	with := func(args ...string) []string { return append(append([]string(nil), d...), args...) }
	return (&ghx.Fake{}).
		On(with("rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"), ghx.Stdout(headSHA+"\n"), nil).
		On(with("symbolic-ref"), ghx.Stdout("refs/heads/main\n"), nil).
		On(with("status"), ghx.Stdout(""), nil).
		On(with("update-ref"), ghx.Result{}, nil).
		On(with("bundle", "create"), ghx.Result{}, nil).
		On(with("merge-base"), ghx.Result{}, nil)
}

// fakeAPI is a runner.API that records what it is asked.
type fakeAPI struct {
	mu         sync.Mutex
	probe      runner.Probe
	probeErr   error
	submitErrs []error
	submits    []runner.SubmitRequest
	bundles    []string
	waitFn     func(runner.WaitRequest) ([]runner.JobStatus, error)
	logFn      func(runner.LogRequest) (runner.LogChunk, error)
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{probe: runner.Probe{
		Schema: 1, HostID: "hid-1", Refs: map[string]string{},
		Tools: map[string]bool{"flock": true, "setsid": true, "timeout": true, "base64": true, "sha256sum": true},
	}}
}

func (f *fakeAPI) Probe(context.Context) (runner.Probe, error) { return f.probe, f.probeErr }

func (f *fakeAPI) Submit(_ context.Context, r runner.SubmitRequest) (runner.SubmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, r)
	body := ""
	if r.Bundle != nil {
		rc, _, err := r.Bundle()
		if err != nil {
			return runner.SubmitResult{}, err
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		body = string(b)
	}
	f.bundles = append(f.bundles, body)
	if len(f.submitErrs) > 0 {
		err := f.submitErrs[0]
		f.submitErrs = f.submitErrs[1:]
		if err != nil {
			return runner.SubmitResult{}, err
		}
	}
	return runner.SubmitResult{Schema: 1, ID: r.ID, Status: runner.SubmitAccepted, State: "queued"}, nil
}

func (f *fakeAPI) Wait(_ context.Context, r runner.WaitRequest) ([]runner.JobStatus, error) {
	return f.waitFn(r)
}

func (f *fakeAPI) Log(_ context.Context, r runner.LogRequest) (runner.LogChunk, error) {
	return f.logFn(r)
}

func (f *fakeAPI) Cancel(context.Context, string) (runner.JobStatus, error) {
	return runner.JobStatus{}, fmt.Errorf("not used")
}

// fakeBackend is a host.Backend with scripted hosts.
type fakeBackend struct {
	mu          sync.Mutex
	hosts       []host.Host
	created     []host.CreateOptions
	stopped     []string
	deleted     []string
	invalidated int
}

func (b *fakeBackend) Discover(context.Context) ([]host.Host, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]host.Host(nil), b.hosts...), nil
}

func (b *fakeBackend) Create(_ context.Context, o host.CreateOptions) (host.Host, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created = append(b.created, o)
	h := host.Host{Name: fmt.Sprintf("cs-%d", o.Num), DisplayName: host.DisplayName(o.Num), Num: o.Num, State: host.Busy}
	b.hosts = append(b.hosts, h)
	return h, nil
}

func (b *fakeBackend) Stop(_ context.Context, h host.Host) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = append(b.stopped, h.DisplayName)
	return nil
}

func (b *fakeBackend) Delete(_ context.Context, h host.Host) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, h.DisplayName)
	return nil
}

func (b *fakeBackend) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invalidated++
}

func (b *fakeBackend) Transport(host.Host) host.Transport { return nil }
func (b *fakeBackend) Root(host.Host) string              { return "/root" }

type env struct {
	c      *Controller
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	api    *fakeAPI
	be     *fakeBackend
	clk    *clock.Fake
	git    *ghx.Fake
	cache  *job.Cache
}

func running(n int) host.Host {
	return host.Host{Name: fmt.Sprintf("cs-%d", n), DisplayName: host.DisplayName(n), Num: n, State: host.Running, RawState: "Available"}
}

func newEnv(t *testing.T, hosts ...host.Host) *env {
	t.Helper()
	e := &env{}
	e.stdout, e.stderr, e.api = &bytes.Buffer{}, &bytes.Buffer{}, newFakeAPI()
	e.be = &fakeBackend{hosts: hosts}
	e.clk = clock.NewFake(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	e.git = gitScript()
	e.cache = job.NewCache(t.TempDir())
	cfg := Config{Runner: scriptRunner{e.git}, Clock: e.clk, Backend: e.be}
	cfg.API = func(host.Host) runner.API { return e.api }
	cfg.Cache, cfg.Entropy = e.cache, &counter{}
	cfg.Stdout, cfg.Stderr = e.stdout, e.stderr
	cfg.Dir, cfg.Repo = ".", "o/r"
	e.c = New(cfg)
	return e
}

// advanceWait makes Wait behave like a host that waited the whole slice.
func (e *env) advanceWait(state string) func(runner.WaitRequest) ([]runner.JobStatus, error) {
	return func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		e.clk.Advance(r.Slice)
		var sts []runner.JobStatus
		for _, id := range r.IDs {
			sts = append(sts, runner.JobStatus{Schema: 1, ID: id, State: state, HostID: "hid-1"})
		}
		return sts, nil
	}
}

func decode(t *testing.T, b *bytes.Buffer, v any) {
	t.Helper()
	if err := json.Unmarshal(b.Bytes(), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b.String())
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func runOpts(cmd ...string) RunOptions {
	return RunOptions{Prio: DefaultPrio, Timeout: DefaultTimeout, Block: time.Minute, Cmd: cmd}
}

func TestCommandString(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"go test ./... && echo ok"}, "go test ./... && echo ok"},
		{[]string{"go", "test", "./..."}, "go test ./..."},
		{[]string{"echo", "a b", "it's"}, `echo 'a b' 'it'\''s'`},
		{[]string{"sh", "-c", "exit 1"}, "sh -c 'exit 1'"},
	}
	for _, c := range cases {
		if got := CommandString(c.args); got != c.want {
			t.Errorf("CommandString(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestRunHappyPath(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "succeeded", HostID: "hid-1", LogTailB64: b64("ok\n")}}, nil
	}
	if code := e.c.Run(context.Background(), runOpts("echo", "ok")); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	lines := strings.Split(e.stdout.String(), "\n")
	if !strings.HasPrefix(lines[0], "job=1-") || !strings.HasSuffix(lines[0], " state=succeeded exit=0") {
		t.Fatalf("first line %q", lines[0])
	}
	if !strings.Contains(e.stdout.String(), "# job output: untrusted data\nok\n") || strings.Contains(e.stdout.String(), "next:") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
	if len(e.api.submits) != 1 {
		t.Fatalf("%d submits, want 1", len(e.api.submits))
	}
	r := e.api.submits[0]
	if r.Cmd != "echo ok" || r.SHA != headSHA || r.Kind != runner.KindRun || !runner.KeyPattern.MatchString(r.Key) || r.Fingerprint != runner.Fingerprint(r) {
		t.Fatalf("submit request %+v", r)
	}
	if e.api.bundles[0] != "BUNDLE" {
		t.Fatalf("bundle body %q", e.api.bundles[0])
	}
	d, err := e.cache.LoadDispatch(r.ID)
	if err != nil || d.HostID != "hid-1" || d.Host != "gatehaul-1" || d.Ref != "refs/heads/main" || d.Repo != "o/r" {
		t.Fatalf("dispatch %+v, %v", d, err)
	}
	if !e.cache.IsTerminal(r.ID) {
		t.Error("the terminal status was not cached")
	}
}

func TestRunSkipsTheBundleWhenTheHostHasTheCommit(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.probe.Refs = map[string]string{"main-abcdef": headSHA}
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if e.api.submits[0].Bundle != nil {
		t.Fatal("a bundle was sent although the host has the commit")
	}
}

func TestRunRedJob(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: r.IDs[0], State: "failed", FailureKind: "user", Stage: "command", HostID: "hid-1", LogTailB64: b64("FAIL\n")}}, nil
	}
	if code := e.c.Run(context.Background(), runOpts("make test")); code != out.ExitRed {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(e.stdout.String(), "state=failed exit=1") || !strings.Contains(e.stdout.String(), "next: gatehaul log 1-") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestRunStillRunningAfterBlock(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("running")
	o := runOpts("sleep 1000")
	o.Block = 2 * time.Minute
	if code := e.c.Run(context.Background(), o); code != out.ExitRunning {
		t.Fatalf("exit %d, want 3", code)
	}
	if !strings.Contains(e.stdout.String(), "state=running exit=3") || !strings.Contains(e.stdout.String(), "next: gatehaul wait 1-") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestRunOutcomeUnknownKeepsTheDispatchAndNeverResubmits(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.submitErrs = []error{fmt.Errorf("submit: %w", runner.ErrOutcomeUnknown)}
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	first, rest, _ := strings.Cut(e.stderr.String(), "\n")
	if len(e.api.submits) != 1 {
		t.Fatalf("%d submits, want 1: a lost answer is never retried blindly", len(e.api.submits))
	}
	id := e.api.submits[0].ID
	if first != "next: gatehaul wait "+id {
		t.Fatalf("first stderr line %q", first)
	}
	var v struct{ Code, Fix string }
	if err := json.Unmarshal([]byte(rest), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rest)
	}
	if v.Code != "outcome_unknown" || v.Fix != "gatehaul wait "+id {
		t.Fatalf("error %+v", v)
	}
	if _, err := e.cache.LoadDispatch(id); err != nil {
		t.Fatalf("dispatch.json was not kept: %v", err)
	}
}

func TestRunNeedFullResendsUnderTheSameID(t *testing.T) {
	e := newEnv(t, running(1))
	base := strings.Repeat("c", 40)
	e.api.probe.Refs = map[string]string{"k": base}
	e.api.submitErrs = []error{&runner.RemoteError{Code: runner.ErrCodeNeedFull, Message: "missing prerequisite"}}
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if len(e.api.submits) != 2 || e.api.submits[0].ID != e.api.submits[1].ID {
		t.Fatalf("submits %d, want 2 with the same id", len(e.api.submits))
	}
	var bundles [][]string
	for _, c := range e.git.Calls() {
		if len(c) > 4 && c[3] == "bundle" {
			bundles = append(bundles, c)
		}
	}
	if len(bundles) != 2 || !strings.Contains(strings.Join(bundles[0], " "), "^"+base) || strings.Contains(strings.Join(bundles[1], " "), "^") {
		t.Fatalf("bundle commands %v: want incremental then full", bundles)
	}
}

func TestRunWithoutHosts(t *testing.T) {
	e := newEnv(t)
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "no_hosts" || v.Fix != "gatehaul up" {
		t.Fatalf("error %+v", v)
	}
}

func TestRunWakesAStoppedHost(t *testing.T) {
	h := running(1)
	h.State, h.RawState = host.Stopped, "Shutdown"
	e := newEnv(t, h)
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(e.stderr.String(), "waking gatehaul-1") {
		t.Fatalf("stderr %q", e.stderr.String())
	}
}

func TestRunMissingToolOnTheHost(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.probe.Tools["flock"] = false
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitInfra {
		t.Fatalf("exit %d", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "missing_tool" || !strings.Contains(v.Fix, "gh codespace ssh -c cs-1") {
		t.Fatalf("error %+v", v)
	}
	if len(e.api.submits) != 0 {
		t.Fatal("a job was submitted to a host that lacks flock")
	}
}

func TestWaitByPrefix(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: id, State: "succeeded", HostID: "hid-1"}}, nil
	}
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{"01jb"}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if !strings.HasPrefix(e.stdout.String(), "job="+id+" state=succeeded exit=0") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestWaitWithoutIDPicksTheHeadJob(t *testing.T) {
	e := newEnv(t, running(1))
	other := "1-01JB0000000000000000000002"
	mine := "1-01JB0000000000000000000003"
	seed(t, e, other, strings.Repeat("b", 40))
	seed(t, e, mine, headSHA)
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		if len(r.IDs) != 1 || r.IDs[0] != mine {
			t.Errorf("waited on %v, want only %s", r.IDs, mine)
		}
		return []runner.JobStatus{{Schema: 1, ID: mine, State: "failed", HostID: "hid-1"}}, nil
	}
	o := WaitOptions{Block: time.Minute, JSON: true}
	if code := e.c.Wait(context.Background(), o); code != out.ExitRed {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	var rep Report
	decode(t, e.stdout, &rep)
	if len(rep.CompletedIDs) != 1 || rep.CompletedIDs[0] != mine || len(rep.PendingIDs) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestWaitAmbiguousPrefix(t *testing.T) {
	e := newEnv(t, running(1))
	seed(t, e, "1-01JB0000000000000000000001", headSHA)
	seed(t, e, "1-01JB0000000000000000000002", headSHA)
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{"01JB"}, Block: time.Minute, JSON: true}); code != out.ExitUsage {
		t.Fatalf("exit %d, want 2", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "ambiguous_job" || !strings.Contains(v.Fix, "1-01JB0000000000000000000001") {
		t.Fatalf("error %+v", v)
	}
}

func TestWaitHostReplaced(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		return []runner.JobStatus{{Schema: 1, ID: id, State: "running", HostID: "hid-NEW"}}, nil
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	if code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var rep Report
	decode(t, e.stdout, &rep)
	if rep.Jobs[0].State != "lost" || rep.Jobs[0].Code != "host_replaced" || !strings.HasPrefix(rep.Jobs[0].Next, "gatehaul run -- ") {
		t.Fatalf("job %+v", rep.Jobs[0])
	}
	if !e.cache.IsTerminal(id) {
		t.Error("the lost status was not cached")
	}
}

func TestWaitHostReplacedSeenAsUnknownJob(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.probe.HostID = "hid-NEW"
	e.api.waitFn = func(runner.WaitRequest) ([]runner.JobStatus, error) {
		return nil, &runner.RemoteError{Code: runner.ErrCodeUnknownJob, Message: "no such job"}
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, e.stdout, &rep)
	if code != out.ExitInfra || rep.Jobs[0].Code != "host_replaced" {
		t.Fatalf("exit %d, report %+v", code, rep)
	}
}

func TestWaitAnyReportsCompletedAndPending(t *testing.T) {
	e := newEnv(t, running(1))
	a, b := "1-01JB0000000000000000000001", "1-01JB0000000000000000000002"
	seed(t, e, a, headSHA)
	seed(t, e, b, headSHA)
	e.api.waitFn = func(r runner.WaitRequest) ([]runner.JobStatus, error) {
		if r.All {
			t.Error("--any must not ask the host to wait for all")
		}
		return []runner.JobStatus{
			{Schema: 1, ID: a, State: "succeeded", HostID: "hid-1"},
			{Schema: 1, ID: b, State: "running", HostID: "hid-1"},
		}, nil
	}
	code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{a, b}, Any: true, Block: time.Minute, JSON: true})
	var rep Report
	decode(t, e.stdout, &rep)
	if code != out.ExitOK || len(rep.CompletedIDs) != 1 || rep.CompletedIDs[0] != a || len(rep.PendingIDs) != 1 || rep.PendingIDs[0] != b {
		t.Fatalf("exit %d, report %+v", code, rep)
	}
}

func TestWaitTerminalCachedJobNeedsNoHost(t *testing.T) {
	e := newEnv(t)
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	if err := e.cache.SaveStatus(runner.JobStatus{ID: id, State: "succeeded", LogTailB64: b64("done\n")}); err != nil {
		t.Fatal(err)
	}
	if code := e.c.Wait(context.Background(), WaitOptions{IDs: []string{id}, Block: time.Minute}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "done\n") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestLogPrintsTheChunkWithACursor(t *testing.T) {
	e := newEnv(t, running(1))
	id := "1-01JB0000000000000000000001"
	seed(t, e, id, headSHA)
	e.api.logFn = func(r runner.LogRequest) (runner.LogChunk, error) {
		if r.ID != id || r.Grep != "FAIL" || r.TailLines != 5 || r.Since != 10 {
			t.Errorf("request %+v", r)
		}
		return runner.LogChunk{Schema: 1, ID: id, State: "failed", DataB64: b64("FAIL x\n"), Cursor: 99, Size: 120, Truncated: true}, nil
	}
	code := e.c.Log(context.Background(), LogOptions{ID: "01JB", Grep: "FAIL", Tail: 5, Since: 10})
	if code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	s := e.stdout.String()
	if !strings.Contains(s, "cursor=99") || !strings.Contains(s, "# job output: untrusted data\nFAIL x\n") || !strings.Contains(s, "next: gatehaul log "+id+" --since 99") {
		t.Fatalf("stdout %q", s)
	}
}

func TestUpWithoutTerminalAndWithoutYes(t *testing.T) {
	e := newEnv(t)
	code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, JSON: true})
	if code != out.ExitInfra {
		t.Fatalf("exit %d, want 5", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "confirmation_required" || v.Fix != "gatehaul up -n 2 --yes" {
		t.Fatalf("error %+v", v)
	}
	if len(e.be.created) != 0 {
		t.Fatal("a codespace was created without confirmation")
	}
}

func TestUpDryRun(t *testing.T) {
	e := newEnv(t, running(1))
	if code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, DryRun: true}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	s := e.stdout.String()
	if !strings.Contains(s, "will create: gatehaul-2") || strings.Contains(s, "will create: gatehaul-1") || !strings.Contains(s, "2 core-hours of your quota per hour") {
		t.Fatalf("stdout %q", s)
	}
	if len(e.be.created) != 0 {
		t.Fatal("dry run created a codespace")
	}
}

func TestUpCap(t *testing.T) {
	e := newEnv(t)
	if code := e.c.Up(context.Background(), UpOptions{N: 3, Max: 2, Yes: true, JSON: true}); code != out.ExitInfra {
		t.Fatalf("exit %d", code)
	}
	var v struct{ Code, Fix string }
	decode(t, e.stderr, &v)
	if v.Code != "too_many_hosts" || !strings.Contains(v.Fix, "--max 3") {
		t.Fatalf("error %+v", v)
	}

	// An existing gatehaul-5 counts against the cap too.
	e = newEnv(t, running(5))
	if code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, Yes: true, JSON: true}); code != out.ExitInfra || len(e.be.created) != 0 {
		t.Fatalf("exit %d, created %v", code, e.be.created)
	}
}

func TestUpCreatesTheMissingHostsUnderTheLock(t *testing.T) {
	e := newEnv(t, running(1))
	if code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, Yes: true, Machine: "basicLinux32gb"}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if len(e.be.created) != 1 {
		t.Fatalf("created %v, want only gatehaul-2", e.be.created)
	}
	o := e.be.created[0]
	if o.Num != 2 || o.Repo != "o/r" || o.Machine != "basicLinux32gb" || o.IdleTimeout != DefaultIdleTimeout || o.Retention != DefaultRetention {
		t.Fatalf("create options %+v", o)
	}
	if e.be.invalidated == 0 {
		t.Error("the host list was not refreshed after taking the lock")
	}
	if _, err := os.Stat(e.cache.Dir() + "/up.lock"); err != nil {
		t.Errorf("no lock file: %v", err)
	}
	// A second run finds nothing to do.
	e.stdout.Reset()
	if code := e.c.Up(context.Background(), UpOptions{N: 2, Max: 2, Yes: true}); code != out.ExitOK || !strings.Contains(e.stdout.String(), "nothing to create") {
		t.Fatalf("second up: exit %d, stdout %q", code, e.stdout.String())
	}
}

func TestUpPromptsOnATerminal(t *testing.T) {
	e := newEnv(t)
	e.c.Stdin = strings.NewReader("n\n")
	e.c.IsTerminal = func() bool { return true }
	if code := e.c.Up(context.Background(), UpOptions{N: 1, Max: 2}); code != out.ExitOK || len(e.be.created) != 0 {
		t.Fatalf("declined: exit %d, created %v", code, e.be.created)
	}
	e.c.Stdin = strings.NewReader("y\n")
	if code := e.c.Up(context.Background(), UpOptions{N: 1, Max: 2}); code != out.ExitOK || len(e.be.created) != 1 {
		t.Fatalf("accepted: exit %d, created %v", code, e.be.created)
	}
}

func TestDownSkipsABusyHost(t *testing.T) {
	e := newEnv(t, running(1), running(2))
	busy, idle := newFakeAPI(), newFakeAPI()
	busy.probe.Running = &runner.RunningJob{ID: "1-01JB0000000000000000000001", AgeS: 5}
	e.c.API = func(h host.Host) runner.API {
		if h.Num == 1 {
			return busy
		}
		return idle
	}
	if code := e.c.Down(context.Background(), DownOptions{}); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	if len(e.be.stopped) != 1 || e.be.stopped[0] != "gatehaul-2" {
		t.Fatalf("stopped %v, want only gatehaul-2", e.be.stopped)
	}
	if !strings.Contains(e.stdout.String(), "gatehaul-1: skipped: job 1-01JB0000000000000000000001 is running") {
		t.Fatalf("stdout %q", e.stdout.String())
	}
}

func TestDownDeleteIncludesStoppedHosts(t *testing.T) {
	stopped := running(2)
	stopped.State = host.Stopped
	e := newEnv(t, running(1), stopped)
	if code := e.c.Down(context.Background(), DownOptions{Delete: true}); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if len(e.be.deleted) != 2 {
		t.Fatalf("deleted %v", e.be.deleted)
	}
}

// seed stores a dispatched job on gatehaul-1.
func seed(t *testing.T, e *env, id, sha string) {
	t.Helper()
	err := e.cache.SaveDispatch(job.Dispatch{
		ID: id, Repo: "o/r", Host: "gatehaul-1", HostNum: 1, HostID: "hid-1", Ref: "refs/heads/main",
		Key: "main-abcdef", SHA: sha, Kind: runner.KindRun, Cmd: "make test", Fingerprint: "fp",
		SubmittedAt: "2026-10-29T12:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
}
