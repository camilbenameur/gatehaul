// Package hosttest is the conformance suite every host.Backend must pass. It
// drives a real runner through runner.NewClient, so it needs bash, git, flock,
// setsid and timeout on Linux, and a backend whose hosts run on this machine
// (it looks at /proc to check that no job process is left behind).
package hosttest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// Injector is what a backend offers to let the suite inject faults. Scenarios
// that need it are skipped when the backend does not implement it.
type Injector interface {
	// SetEnv adds "KEY=value" to the environment of the remote bash.
	SetEnv(kv ...string)
	// InjectFailBeforeStart makes the next n Exec calls fail before bash runs.
	InjectFailBeforeStart(n int)
	// InjectCutAfter makes the next Exec stop after the first stdout line that
	// contains substr, and fail.
	InjectCutAfter(substr string)
}

// jobKey is the branch key used by every job of the suite.
const jobKey = "main-0a1b2c"

// Run plays every scenario against a fresh backend from newBackend.
func Run(t *testing.T, newBackend func(t *testing.T) host.Backend) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the host conformance suite needs Linux")
	}
	for _, tool := range []string{"bash", "git", "flock", "setsid", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the host conformance suite needs %s on PATH", tool)
		}
	}
	scenarios := []struct {
		name string
		fn   func(t *testing.T, b host.Backend)
	}{
		{"probe_fresh_host", scenarioProbe},
		{"submit_and_wait", scenarioSubmitWait},
		{"failing_command", scenarioFailing},
		{"no_credentials_in_job", scenarioCredentials},
		{"incremental_bundle", scenarioIncremental},
		{"need_full", scenarioNeedFull},
		{"replayed_submit", scenarioReplay},
		{"cut_after_begin", scenarioCut},
		{"retry_before_start", scenarioRetry},
		{"same_key_back_to_back", scenarioSameKey},
		{"timeout", scenarioTimeout},
		{"cancel_running", scenarioCancelRunning},
		{"cancel_queued", scenarioCancelQueued},
		{"lost_job", scenarioLost},
		{"log_filters", scenarioLog},
		{"request_validation", scenarioValidation},
		{"script_is_ascii_lf", scenarioScript},
		{"queued_job_is_replayed_once", scenarioQueuedReplay},
		{"queued_job_lost_twice", scenarioQueuedLostTwice},
		{"log_flood_stops_the_job", scenarioLogFlood},
		{"unremovable_worktree", scenarioUnremovableWorktree},
		{"exit_124_and_137_are_not_timeouts", scenarioExitCodes},
		{"corrupt_runner_is_reinstalled", scenarioCorruptRunner},
		{"recreated_host_has_another_identity", scenarioReplaced},
		{"first_exec_wakes_a_stopped_host", scenarioWake},
		{"gen_patch", scenarioGenPatch},
		{"gen_failure_has_no_patch", scenarioGenFailed},
		{"run_job_has_no_patch", scenarioRunNoPatch},
		{"gen_without_changes_has_an_empty_patch", scenarioGenEmpty},
		{"gen_patch_is_against_the_base_commit", scenarioGenCommitted},
		{"gen_patch_leaves_out_kept_caches", scenarioGenKeptCache},
		{"gen_patch_over_the_limit_fails", scenarioGenPatchTooLarge},
		{"gen_patch_failure_has_a_reason", scenarioGenPatchReason},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.fn(t, newBackend(t))
		})
	}
}

// rig is one host, a client to its runner, and a git repository to send
// commits from.
type rig struct {
	t      *testing.T
	b      host.Backend
	h      host.Host
	c      *runner.Client
	clk    *clock.Fake
	repo   string
	seq    int
	refSeq int
	ids    []string
}

func newRig(t *testing.T, b host.Backend) *rig {
	t.Helper()
	ctx := t.Context()
	hosts, err := b.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var h host.Host
	found := false
	for _, c := range hosts {
		if c.State == host.Running {
			h, found = c, true
			break
		}
	}
	if !found {
		h, err = b.Create(ctx, host.CreateOptions{Num: 1})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := &rig{
		t:    t,
		b:    b,
		h:    h,
		c:    runner.NewClient(b.Transport(h), b.Root(h), clk),
		clk:  clk,
		repo: t.TempDir(),
	}
	git(t, r.repo, "init", "-q")
	t.Cleanup(r.killAll)
	return r
}

func injector(t *testing.T, b host.Backend) Injector {
	t.Helper()
	inj, ok := b.(Injector)
	if !ok {
		t.Skip("the backend cannot inject faults")
	}
	return inj
}

// git runs git in dir with a fixed identity and no user configuration.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// commit adds a file to the repository and returns the new HEAD.
func (r *rig) commit(name, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.repo, name), []byte(content), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", name, err)
	}
	git(r.t, r.repo, "add", "-A")
	git(r.t, r.repo, "commit", "-q", "-m", "add "+name)
	return git(r.t, r.repo, "rev-parse", "HEAD")
}

// bundle makes a bundle of sha, without the history of base when base is set.
func (r *rig) bundle(sha, base string) func() (io.ReadCloser, int64, error) {
	r.t.Helper()
	r.refSeq++
	ref := fmt.Sprintf("refs/gatehaul-test/b%d", r.refSeq)
	git(r.t, r.repo, "update-ref", ref, sha)
	file := filepath.Join(r.t.TempDir(), "job.bundle")
	args := []string{"bundle", "create", file, ref}
	if base != "" {
		args = append(args, "^"+base)
	}
	git(r.t, r.repo, args...)
	return func() (io.ReadCloser, int64, error) {
		f, err := os.Open(file)
		if err != nil {
			return nil, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	}
}

// req builds a run request with a fresh id and 30 s to run.
func (r *rig) req(cmd, sha string) runner.SubmitRequest {
	return r.reqTimeout(cmd, sha, 30*time.Second)
}

func (r *rig) reqTimeout(cmd, sha string, timeout time.Duration) runner.SubmitRequest {
	r.seq++
	req := runner.SubmitRequest{
		ID:      fmt.Sprintf("%d-01TEST%020d", r.h.Num, r.seq),
		Key:     jobKey,
		SHA:     sha,
		Kind:    runner.KindRun,
		Prio:    5,
		Timeout: timeout,
		Cmd:     cmd,
	}
	req.Fingerprint = runner.Fingerprint(req)
	r.ids = append(r.ids, req.ID)
	return req
}

// submit sends req with a full bundle of its commit and expects it accepted.
func (r *rig) submit(req runner.SubmitRequest) runner.SubmitResult {
	r.t.Helper()
	if req.Bundle == nil {
		req.Bundle = r.bundle(req.SHA, "")
	}
	res, err := r.c.Submit(r.t.Context(), req)
	if err != nil {
		r.t.Fatalf("submit %s: %v", req.ID, err)
	}
	if res.Status != runner.SubmitAccepted || res.ID != req.ID {
		r.t.Fatalf("submit %s: got %+v, want accepted", req.ID, res)
	}
	return res
}

func isTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "timed_out", "cancelled", "lost", "infra_failed":
		return true
	}
	return false
}

// waitFor calls Wait until pred holds for every status, or 60 s pass.
func (r *rig) waitFor(ids []string, all bool, pred func(runner.JobStatus) bool) []runner.JobStatus {
	r.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		sts, err := r.c.Wait(r.t.Context(), runner.WaitRequest{
			IDs: ids, Slice: time.Second, All: all, TailBytes: 4096,
		})
		if err != nil {
			r.t.Fatalf("wait %v: %v", ids, err)
		}
		ok := len(sts) == len(ids)
		for _, st := range sts {
			if !pred(st) {
				ok = false
			}
		}
		if ok {
			return sts
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("wait %v: still not done after 60s: %+v", ids, sts)
		}
	}
}

// waitDone returns the final status of a job.
func (r *rig) waitDone(id string) runner.JobStatus {
	r.t.Helper()
	return r.waitFor([]string{id}, true, func(s runner.JobStatus) bool { return isTerminal(s.State) })[0]
}

// waitRunning returns once the job's command runs.
func (r *rig) waitRunning(id string) {
	r.t.Helper()
	sts := r.waitFor([]string{id}, true, func(s runner.JobStatus) bool {
		return s.State == "running" || isTerminal(s.State)
	})
	if sts[0].State != "running" {
		r.t.Fatalf("job %s is %s, want running: %+v", id, sts[0].State, sts[0])
	}
}

func (r *rig) logText(id string) string {
	r.t.Helper()
	ch, err := r.c.Log(r.t.Context(), runner.LogRequest{ID: id})
	if err != nil {
		r.t.Fatalf("log %s: %v", id, err)
	}
	return decode(r.t, ch.DataB64)
}

func decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return string(b)
}

func wantState(t *testing.T, st runner.JobStatus, state string) {
	t.Helper()
	if st.State != state {
		t.Fatalf("job %s is %s (%s), want %s: %+v", st.ID, st.State, st.Reason, state, st)
	}
}

func wantContains(t *testing.T, what, got, sub string) {
	t.Helper()
	if !strings.Contains(got, sub) {
		t.Errorf("%s = %q, want it to contain %q", what, got, sub)
	}
}

func wantRemote(t *testing.T, err error, code string) {
	t.Helper()
	var re *runner.RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a RemoteError with code %s", err, code)
	}
	if re.Code != code {
		t.Fatalf("remote error code = %s (%s), want %s", re.Code, re.Message, code)
	}
}

// procsFor lists the processes whose environment carries GATEHAUL_JOB=id.
func procsFor(id string) []int {
	envs, _ := filepath.Glob("/proc/[0-9]*/environ")
	needle := []byte("GATEHAUL_JOB=" + id + "\x00")
	var pids []int
	for _, e := range envs {
		data, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			if pid, err := strconv.Atoi(strings.Split(e, "/")[2]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// jobShells lists the detached "__job <id>" shells of the runner.
func jobShells(id string) []int {
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	needle := []byte("__job\x00" + id + "\x00")
	var pids []int
	for _, c := range cmdlines {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			if pid, err := strconv.Atoi(strings.Split(c, "/")[2]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func kill(pids []int) {
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// restart simulates a host restart for the detached "__job" shells of id: each
// shell's whole process group dies (the shell and its short-lived children,
// which share its lock descriptor), and restart returns once they are gone.
func restart(t *testing.T, id string) {
	t.Helper()
	shells := jobShells(id)
	if len(shells) == 0 {
		t.Fatalf("no __job shell found for %s", id)
	}
	var group []int
	for _, leader := range shells {
		group = append(group, groupOf(leader)...)
	}
	kill(group)
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range group {
		for {
			if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("process %d of job %s survived the simulated restart", pid, id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// groupOf lists the processes whose process group is leader's ("__job" is
// started by setsid, so it leads its own group).
func groupOf(leader int) []int {
	pids := []int{leader}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, st := range stats {
		data, err := os.ReadFile(st)
		if err != nil {
			continue
		}
		// The command name is in parentheses and may hold spaces: fields
		// start after the last ')'. Field 5 of the file, the process group,
		// is the third one after it.
		i := bytes.LastIndexByte(data, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(data[i+1:]))
		if len(f) < 3 || f[2] != strconv.Itoa(leader) {
			continue
		}
		if pid, err := strconv.Atoi(strings.Split(st, "/")[2]); err == nil && pid != leader {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killAll leaves nothing of the rig's jobs running.
func (r *rig) killAll() {
	for _, id := range r.ids {
		kill(jobShells(id))
		kill(procsFor(id))
	}
}

// waitShells waits until the detached "__job" shell of a job exists.
func (r *rig) waitShells(id string) []int {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if pids := jobShells(id); len(pids) > 0 {
			return pids
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no __job shell for %s after 15s", id)
		}
		if err := (clock.Real{}).Sleep(r.t.Context(), 50*time.Millisecond); err != nil {
			r.t.Fatalf("waiting for the __job shell of %s: %v", id, err)
		}
	}
}

// jobDir is where the host keeps the files of a job.
func (r *rig) jobDir(id string) string { return filepath.Join(r.b.Root(r.h), "jobs", id) }

// age sets the modification time of a job file to d ago.
func (r *rig) age(path string, d time.Duration) {
	r.t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		r.t.Fatalf("age %s: %v", path, err)
	}
}

func (r *rig) noProcs(id string) {
	r.t.Helper()
	if pids := procsFor(id); len(pids) > 0 {
		r.t.Errorf("processes of job %s are still running: %v", id, pids)
	}
}

// ------------------------------------------------------------------ scenarios

func scenarioProbe(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	p1, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p1.Schema != runner.Schema {
		t.Errorf("schema = %d, want %d", p1.Schema, runner.Schema)
	}
	if p1.Runner != runner.Version() {
		t.Errorf("runner = %q, want %q", p1.Runner, runner.Version())
	}
	if p1.HostID == "" || p1.BootID == "" {
		t.Errorf("host_id %q and boot_id %q must be set", p1.HostID, p1.BootID)
	}
	if len(p1.Refs) != 0 {
		t.Errorf("refs = %v, want none", p1.Refs)
	}
	for _, tool := range []string{"flock", "setsid", "timeout", "base64", "sha256sum", "git"} {
		if !p1.Tools[tool] {
			t.Errorf("tool %s is reported missing", tool)
		}
	}
	if p1.Running != nil || p1.QueueDepth != 0 || len(p1.Reconciled) != 0 {
		t.Errorf("a fresh host runs nothing: %+v", p1)
	}
	installed := filepath.Join(b.Root(r.h), "runner-"+runner.Version()+".sh")
	if _, err := os.Stat(installed); err != nil {
		t.Errorf("the runner is not installed: %v", err)
	}
	p2, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if p2.HostID != p1.HostID {
		t.Errorf("host_id changed between probes: %q then %q", p1.HostID, p2.HostID)
	}
}

func scenarioSubmitWait(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req("echo hello; exit 0", sha)
	res := r.submit(req)
	if res.State != "queued" {
		t.Errorf("state after submit = %s, want queued", res.State)
	}
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", st.ExitCode)
	}
	if st.SHA != sha || st.HostID == "" || st.StartedAt == "" || st.EndedAt == "" {
		t.Errorf("status misses sha, host or times: %+v", st)
	}
	wantContains(t, "log tail", decode(t, st.LogTailB64), "hello")
	wantContains(t, "log", r.logText(req.ID), "hello")
	p, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p.Refs[jobKey] != sha {
		t.Errorf("refs[%s] = %q, want %q", jobKey, p.Refs[jobKey], sha)
	}
	if n, err := r.c.GC(t.Context(), 7); err != nil || n != 0 {
		t.Errorf("gc of recent jobs = %d, %v; want 0, nil", n, err)
	}
	r.noProcs(req.ID)
}

func scenarioFailing(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req("echo boom >&2; exit 3", sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "failed")
	if st.ExitCode == nil || *st.ExitCode != 3 {
		t.Errorf("exit_code = %v, want 3", st.ExitCode)
	}
	if st.FailureKind != "user" || st.Stage != "command" {
		t.Errorf("failure_kind %q stage %q, want user and command", st.FailureKind, st.Stage)
	}
	wantContains(t, "log", r.logText(req.ID), "boom")
}

func scenarioCredentials(t *testing.T, b host.Backend) {
	injector(t, b).SetEnv("GITHUB_TOKEN=gatehaul-test-dummy", "GH_TOKEN=gatehaul-test-dummy")
	r := newRig(t, b)
	p, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !p.GitHubTokenEnv {
		t.Errorf("probe does not see the token variable that the host has")
	}
	sha := r.commit("a.txt", "one\n")
	cmd := `echo "token=[${GITHUB_TOKEN-}]"; echo "gh=[${GH_TOKEN-}]"; ` +
		`echo "helper=[$(git config credential.helper)]"; echo "job=$GATEHAUL_JOB"`
	req := r.req(cmd, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	log := r.logText(req.ID)
	wantContains(t, "log", log, "token=[]")
	wantContains(t, "log", log, "gh=[]")
	wantContains(t, "log", log, "helper=[]")
	wantContains(t, "log", log, "job="+req.ID)
	if strings.Contains(log, "gatehaul-test-dummy") {
		t.Errorf("the dummy token leaked into the job: %q", log)
	}
}

func scenarioIncremental(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha1 := r.commit("a.txt", "one\n")
	sha2 := r.commit("b.txt", "two\n")

	req1 := r.req("git rev-parse HEAD", sha1)
	r.submit(req1)
	wantState(t, r.waitDone(req1.ID), "succeeded")
	wantContains(t, "log of job 1", r.logText(req1.ID), sha1)

	req2 := r.req("git rev-parse HEAD", sha2)
	req2.Bundle = r.bundle(sha2, sha1)
	r.submit(req2)
	wantState(t, r.waitDone(req2.ID), "succeeded")
	wantContains(t, "log of job 2", r.logText(req2.ID), sha2)
}

func scenarioNeedFull(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha1 := r.commit("a.txt", "one\n")
	sha2 := r.commit("b.txt", "two\n")

	req := r.req("true", sha2)
	req.Bundle = r.bundle(sha2, sha1)
	_, err := r.c.Submit(t.Context(), req)
	wantRemote(t, err, runner.ErrCodeNeedFull)

	// A refused submit leaves nothing behind: the same id works with a full bundle.
	req.Bundle = r.bundle(sha2, "")
	r.submit(req)
	wantState(t, r.waitDone(req.ID), "succeeded")

	// Without a bundle, the commit must already be on the host.
	sha3 := r.commit("c.txt", "three\n")
	req3 := r.req("true", sha3)
	_, err = r.c.Submit(t.Context(), req3)
	wantRemote(t, err, runner.ErrCodeNeedFull)

	// And a bundle that does not hold the expected commit is refused too.
	req4 := r.req("true", sha3)
	req4.Bundle = r.bundle(sha2, "")
	_, err = r.c.Submit(t.Context(), req4)
	wantRemote(t, err, runner.ErrCodeSHAMismatch)
}

func scenarioReplay(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req("echo once", sha)
	req.Bundle = r.bundle(sha, "")
	r.submit(req)

	again, err := r.c.Submit(t.Context(), req)
	if err != nil {
		t.Fatalf("replayed submit: %v", err)
	}
	if again.Status != runner.SubmitExists || again.ID != req.ID {
		t.Errorf("replay = %+v, want exists for %s", again, req.ID)
	}

	other := req
	other.Cmd = "echo something else"
	other.Fingerprint = runner.Fingerprint(other)
	_, err = r.c.Submit(t.Context(), other)
	wantRemote(t, err, runner.ErrCodeConflict)

	wantState(t, r.waitDone(req.ID), "succeeded")
	// The job ran once.
	if got := strings.Count(r.logText(req.ID), "once"); got != 1 {
		t.Errorf("the command ran %d times, want 1", got)
	}
}

func scenarioCut(t *testing.T, b host.Backend) {
	inj := injector(t, b)
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req("echo cut-ok", sha)
	req.Bundle = r.bundle(sha, "")

	inj.InjectCutAfter(runner.Marker + " BEGIN")
	_, err := r.c.Submit(t.Context(), req)
	if !errors.Is(err, runner.ErrOutcomeUnknown) {
		t.Fatalf("submit cut after BEGIN: error = %v, want ErrOutcomeUnknown", err)
	}

	// The controller never resubmits blindly: it waits on the id it chose.
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	wantContains(t, "log", r.logText(req.ID), "cut-ok")

	// Even a replay finds the one job.
	again, err := r.c.Submit(t.Context(), req)
	if err != nil || again.Status != runner.SubmitExists {
		t.Errorf("replay after the cut = %+v, %v; want exists", again, err)
	}
}

func scenarioRetry(t *testing.T, b host.Backend) {
	inj := injector(t, b)
	r := newRig(t, b)

	inj.InjectFailBeforeStart(2)
	start := r.clk.Now()
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("probe after two failed connections: %v", err)
	}
	if got := r.clk.Now().Sub(start); got != 6*time.Second {
		t.Errorf("backoff waited %s on the clock, want 2s + 4s", got)
	}

	r.c.StartRetry = 10 * time.Second
	inj.InjectFailBeforeStart(1000)
	_, err := r.c.Probe(t.Context())
	if !errors.Is(err, runner.ErrNotStarted) {
		t.Fatalf("probe with no connection: error = %v, want ErrNotStarted", err)
	}
	wantContains(t, "error", err.Error(), "injected failure")
	inj.InjectFailBeforeStart(0)
}

func scenarioSameKey(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	shaA := r.commit("a.txt", "A\n")
	shaB := r.commit("b.txt", "B\n")

	reqA := r.req("sleep 2; git rev-parse HEAD", shaA)
	reqB := r.req("git rev-parse HEAD", shaB)
	r.submit(reqA)
	r.submit(reqB)

	sts := r.waitFor([]string{reqA.ID, reqB.ID}, true, func(s runner.JobStatus) bool { return isTerminal(s.State) })
	for _, st := range sts {
		wantState(t, st, "succeeded")
	}
	logA, logB := r.logText(reqA.ID), r.logText(reqB.ID)
	wantContains(t, "log of A", logA, shaA)
	wantContains(t, "log of B", logB, shaB)
	if strings.Contains(logA, shaB) || strings.Contains(logB, shaA) {
		t.Errorf("a job saw the other's commit: A=%q B=%q", logA, logB)
	}
}

func scenarioTimeout(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.reqTimeout("sleep 30", sha, 2*time.Second)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "timed_out")
	if st.FailureKind != "user" {
		t.Errorf("failure_kind = %q, want user", st.FailureKind)
	}
	r.noProcs(req.ID)
}

func scenarioCancelRunning(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.reqTimeout("sleep 30", sha, 60*time.Second)
	r.submit(req)
	r.waitRunning(req.ID)

	st, err := r.c.Cancel(t.Context(), req.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	wantState(t, st, "cancelled")
	r.noProcs(req.ID)

	// A terminal state is never rewritten.
	again, err := r.c.Cancel(t.Context(), req.ID)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	wantState(t, again, "cancelled")
}

func scenarioCancelQueued(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	first := r.reqTimeout("sleep 30", sha, 60*time.Second)
	second := r.req("echo never", sha)
	r.submit(first)
	r.waitRunning(first.ID)
	r.submit(second)

	st, err := r.c.Cancel(t.Context(), second.ID)
	if err != nil {
		t.Fatalf("cancel of a queued job: %v", err)
	}
	wantState(t, st, "cancelled")
	wantState(t, r.waitDone(second.ID), "cancelled")
	if strings.Contains(r.logText(second.ID), "never") {
		t.Errorf("a cancelled queued job ran")
	}

	st, err = r.c.Cancel(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	wantState(t, st, "cancelled")
	r.noProcs(first.ID)
}

func scenarioLost(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.reqTimeout("sleep 60", sha, 120*time.Second)
	r.submit(req)
	r.waitRunning(req.ID)

	// The host "restarts": the job's shell and everything under it die.
	restart(t, req.ID)
	kill(procsFor(req.ID))

	p, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	found := false
	for _, id := range p.Reconciled {
		if id == req.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("probe reconciled %v, want it to contain %s", p.Reconciled, req.ID)
	}
	st := r.waitDone(req.ID)
	wantState(t, st, "lost")
	if st.FailureKind != "lost" {
		t.Errorf("failure_kind = %q, want lost", st.FailureKind)
	}
	r.noProcs(req.ID)
}

func scenarioLog(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req(`for i in 1 2 3 4 5 6; do echo "line $i"; done; echo other`, sha)
	r.submit(req)
	wantState(t, r.waitDone(req.ID), "succeeded")

	ctx := t.Context()
	full := "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nother\n"
	all, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if got := decode(t, all.DataB64); got != full {
		t.Errorf("log = %q, want %q", got, full)
	}
	if all.Cursor != int64(len(full)) || all.Size != int64(len(full)) || all.Truncated {
		t.Errorf("cursor %d size %d truncated %v, want %d %d false", all.Cursor, all.Size, all.Truncated, len(full), len(full))
	}
	if all.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", all.State)
	}

	rest, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID, Since: all.Cursor})
	if err != nil {
		t.Fatalf("log since the cursor: %v", err)
	}
	if got := decode(t, rest.DataB64); got != "" || rest.Cursor != all.Cursor {
		t.Errorf("log since the end = %q cursor %d, want nothing and cursor %d", got, rest.Cursor, all.Cursor)
	}

	from, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID, Since: 7})
	if err != nil {
		t.Fatalf("log since 7: %v", err)
	}
	if got := decode(t, from.DataB64); got != full[7:] {
		t.Errorf("log since 7 = %q, want %q", got, full[7:])
	}

	grep, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID, Grep: "line [35]"})
	if err != nil {
		t.Fatalf("log with grep: %v", err)
	}
	if got := decode(t, grep.DataB64); got != "line 3\nline 5\n" {
		t.Errorf("log with grep = %q", got)
	}

	tail, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID, TailLines: 2})
	if err != nil {
		t.Fatalf("log with tail: %v", err)
	}
	if got := decode(t, tail.DataB64); got != "line 6\nother\n" {
		t.Errorf("log with tail = %q", got)
	}

	short, err := r.c.Log(ctx, runner.LogRequest{ID: req.ID, MaxBytes: 10})
	if err != nil {
		t.Fatalf("log with max_bytes: %v", err)
	}
	if got := decode(t, short.DataB64); got != "line 1\n" || short.Cursor != 7 || !short.Truncated {
		t.Errorf("log with max_bytes = %q cursor %d truncated %v, want one line, cursor 7, truncated", got, short.Cursor, short.Truncated)
	}

	_, err = r.c.Log(ctx, runner.LogRequest{ID: r.unknownID(), Grep: "x"})
	wantRemote(t, err, runner.ErrCodeUnknownJob)
}

// unknownID is a well-formed id that no job has.
func (r *rig) unknownID() string { return fmt.Sprintf("%d-01TEST%020d", r.h.Num, 99999) }

func scenarioValidation(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	preamble := runner.Preamble(b.Root(r.h))
	script := runner.Script()
	raw := func(request string) string {
		t.Helper()
		var stdin bytes.Buffer
		stdin.Write(preamble)
		stdin.Write(script)
		stdin.WriteString(request)
		var out bytes.Buffer
		if err := b.Transport(r.h).Exec(t.Context(), &stdin, &out); err != nil {
			t.Fatalf("exec: %v", err)
		}
		return out.String()
	}
	bad := [][2]string{
		{"bad id", "GATEHAUL1 cancel\nid=../../etc\n\n"},
		{"unknown command", "GATEHAUL1 rm\n\n"},
		{"bad character", "GATEHAUL1 probe\nx=a;b\n\n"},
		{"missing id", "GATEHAUL1 wait\ntimeout_s=0\n\n"},
		{"unterminated", "GATEHAUL1 probe\nx=a\n"},
		{"not a request", "hello\n\n"},
		{"command in value", "GATEHAUL1 gc\ndays=$(touch pwned)\n\n"},
	}
	for _, c := range bad {
		out := raw(c[1])
		if !strings.Contains(out, `"code":"bad_request"`) || !strings.Contains(out, "GATEHAUL1 END 1") {
			t.Errorf("%s: reply = %q, want a bad_request error", c[0], out)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.Root(r.h)), "pwned")); err == nil {
		t.Errorf("a command inside a header value was executed")
	}
}

// queuedBehindARunningJob submits a long job, then a second one that waits for
// its turn, and returns both.
func queuedBehindARunningJob(r *rig, second string) (first, queued runner.SubmitRequest) {
	sha := r.commit("a.txt", "one\n")
	first = r.reqTimeout("sleep 30", sha, 60*time.Second)
	queued = r.req(second, sha)
	r.submit(first)
	r.waitRunning(first.ID)
	r.submit(queued)
	r.waitShells(queued.ID)
	return first, queued
}

func scenarioQueuedReplay(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	first, queued := queuedBehindARunningJob(r, "echo replayed")

	// The host restarted: the process of the queued job is gone, but nothing of
	// it ever ran. It has been waiting for more than the grace period.
	restart(t, queued.ID)
	r.age(filepath.Join(r.jobDir(queued.ID), "meta.json"), 5*time.Minute)

	p, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	for _, id := range p.Reconciled {
		if id == queued.ID {
			t.Fatalf("a job that never started is launched again, not marked lost: %v", p.Reconciled)
		}
	}
	if _, err := os.Stat(filepath.Join(r.jobDir(queued.ID), "replays")); err != nil {
		t.Fatalf("the replay was not recorded: %v", err)
	}
	r.waitShells(queued.ID)

	// It kept its place: once the first job is gone, it runs, exactly once.
	if _, err := r.c.Cancel(t.Context(), first.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	wantState(t, r.waitDone(queued.ID), "succeeded")
	if got := strings.Count(r.logText(queued.ID), "replayed"); got != 1 {
		t.Errorf("the command ran %d times, want 1", got)
	}
}

func scenarioQueuedLostTwice(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	first, queued := queuedBehindARunningJob(r, "echo never")

	restart(t, queued.ID)
	r.age(filepath.Join(r.jobDir(queued.ID), "meta.json"), 5*time.Minute)
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	r.waitShells(queued.ID)

	// Lost again, still without ever starting: no third attempt.
	restart(t, queued.ID)
	r.age(filepath.Join(r.jobDir(queued.ID), "meta.json"), 5*time.Minute)
	r.age(filepath.Join(r.jobDir(queued.ID), "replays"), 5*time.Minute)
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	st := r.waitDone(queued.ID)
	wantState(t, st, "infra_failed")
	if st.Stage != "queue" || st.FailureKind != "infra" {
		t.Errorf("stage %q kind %q, want queue and infra", st.Stage, st.FailureKind)
	}
	if strings.Contains(r.logText(queued.ID), "never") {
		t.Errorf("the lost job ran")
	}
	if _, err := r.c.Cancel(t.Context(), first.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func scenarioLogFlood(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	flood := `while :; do head -c 1048576 /dev/zero | tr '\0' x; echo; sleep 0.05; done`
	req := r.reqTimeout(flood, sha, 120*time.Second)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "failed")
	if !st.Truncated || st.Stage != "command" || st.FailureKind != "user" {
		t.Errorf("truncated %v stage %q kind %q, want true, command, user", st.Truncated, st.Stage, st.FailureKind)
	}
	wantContains(t, "reason", st.Reason, "output")
	if limit := int64(runner.MaxLogFile) + 8<<20; st.LogSize > limit {
		t.Errorf("the log is %d bytes, the job was not stopped near %d", st.LogSize, runner.MaxLogFile)
	}
	r.noProcs(req.ID)
}

func scenarioUnremovableWorktree(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	root := b.Root(r.h)
	// The test's own cleanup must be able to delete what the job locked.
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+rwx", root).Run() })
	sha := r.commit("a.txt", "one\n")

	// A file that cannot be removed (this is what a container or sudo leaves
	// behind for the next job on the branch).
	first := r.req("mkdir -p locked/sub && touch locked/sub/f && chmod 500 locked/sub && echo made", sha)
	r.submit(first)
	wantState(t, r.waitDone(first.ID), "succeeded")

	second := r.req("git rev-parse HEAD", sha)
	r.submit(second)
	wantState(t, r.waitDone(second.ID), "succeeded")
	wantContains(t, "log", r.logText(second.ID), sha)
}

func scenarioExitCodes(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	for _, code := range []int{124, 137} {
		req := r.req(fmt.Sprintf("exit %d", code), sha)
		r.submit(req)
		st := r.waitDone(req.ID)
		wantState(t, st, "failed")
		if st.ExitCode == nil || *st.ExitCode != code {
			t.Errorf("exit %d: exit_code = %v", code, st.ExitCode)
		}
	}
}

func scenarioCorruptRunner(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	installed := filepath.Join(b.Root(r.h), "runner-"+runner.Version()+".sh")
	// A crash between the write and the disk: the file exists and is wrong.
	if err := os.WriteFile(installed, []byte("not a runner\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("probe with a corrupt installed runner: %v", err)
	}
	got, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, runner.Script()) {
		t.Errorf("the installed runner was not replaced (%v)", err)
	}
}

func scenarioReplaced(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	p1, err := r.c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := b.Delete(t.Context(), r.h); err != nil {
		t.Fatalf("delete: %v", err)
	}
	h2, err := b.Create(t.Context(), host.CreateOptions{Num: r.h.Num})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h2.Name == r.h.Name {
		t.Errorf("the recreated host has the same name %q: it cannot be told apart", h2.Name)
	}
	p2, err := runner.NewClient(b.Transport(h2), b.Root(h2), r.clk).Probe(t.Context())
	if err != nil {
		t.Fatalf("probe of the recreated host: %v", err)
	}
	if p2.HostID == p1.HostID {
		t.Errorf("host_id %q survived the deletion", p1.HostID)
	}
}

func scenarioWake(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	if err := b.Stop(t.Context(), r.h); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := r.c.Probe(t.Context()); err != nil {
		t.Fatalf("probe of a stopped host (the first connection wakes it): %v", err)
	}
	hosts, err := b.Discover(t.Context())
	if err != nil || len(hosts) == 0 {
		t.Fatalf("discover: %v, %v", hosts, err)
	}
	for _, h := range hosts {
		if h.Num == r.h.Num && h.State != host.Running {
			t.Errorf("state after the first connection = %s, want running", h.State)
		}
	}
}

func scenarioScript(t *testing.T, _ host.Backend) {
	s := runner.Script()
	if bytes.ContainsRune(s, '\r') {
		t.Errorf("runner.sh contains a carriage return")
	}
	for i, c := range s {
		if c >= 0x80 || (c < 0x20 && c != '\n' && c != '\t') {
			t.Fatalf("runner.sh byte %d is not printable ASCII: %#x", i, c)
		}
	}
	if !bytes.HasPrefix(s, []byte("#!/usr/bin/env bash\n")) {
		t.Errorf("runner.sh does not start with the bash shebang")
	}
	if len(runner.Version()) != 12 {
		t.Errorf("version %q is not 12 digits long", runner.Version())
	}
}

// ------------------------------------------------------------------ gen

// genReq builds a gen request: like req, with the kind and the fingerprint
// that go with it.
func (r *rig) genReq(cmd, sha string) runner.SubmitRequest {
	req := r.req(cmd, sha)
	req.Kind = runner.KindGen
	req.Fingerprint = runner.Fingerprint(req)
	return req
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// patchBytes fetches the patch of a finished gen job and checks that it agrees
// with the status the runner gave for the job.
func (r *rig) patchBytes(st runner.JobStatus) []byte {
	r.t.Helper()
	p, err := r.c.Patch(r.t.Context(), st.ID)
	if err != nil {
		r.t.Fatalf("patch %s: %v", st.ID, err)
	}
	data, err := base64.StdEncoding.DecodeString(p.DataB64)
	if err != nil {
		r.t.Fatalf("patch %s: bad base64: %v", st.ID, err)
	}
	if p.ID != st.ID {
		r.t.Errorf("patch id = %q, want %q", p.ID, st.ID)
	}
	if got := hexSum(data); got != p.SHA256 || got != st.PatchSHA256 {
		r.t.Errorf("sha256 of the patch bytes = %s, patch reply says %s, status says %s", got, p.SHA256, st.PatchSHA256)
	}
	if int64(len(data)) != p.Size || p.Size != st.PatchSize {
		r.t.Errorf("patch has %d bytes, patch reply says %d, status says %d", len(data), p.Size, st.PatchSize)
	}
	return data
}

func scenarioGenPatch(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	r.commit("a.txt", "one\n")
	sha := r.commit("b.txt", "two\n")

	// One tracked file changed, one deleted, and two files added (one binary,
	// one in a new directory).
	cmd := `printf 'changed\n' > a.txt; rm b.txt; printf 'new\n' > c.txt; ` +
		`printf '\000\001\002binary\377' > bin.dat; mkdir sub; printf 'deep\n' > sub/d.txt`
	req := r.genReq(cmd, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	if st.PatchSHA256 == "" || st.PatchSize <= 0 {
		t.Fatalf("a gen job that changed files has patch_sha256 %q and patch_size %d", st.PatchSHA256, st.PatchSize)
	}
	data := r.patchBytes(st)

	// The patch applied on a fresh checkout of the base commit gives the files
	// the command left behind.
	dir := t.TempDir()
	git(t, dir, "clone", "-q", r.repo, ".")
	git(t, dir, "checkout", "-q", "--detach", sha)
	file := filepath.Join(t.TempDir(), "gen.patch")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatalf("write the patch: %v", err)
	}
	git(t, dir, "apply", "--binary", file)
	want := map[string]string{
		"a.txt":     "changed\n",
		"c.txt":     "new\n",
		"bin.dat":   "\x00\x01\x02binary\xff",
		"sub/d.txt": "deep\n",
	}
	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != content {
			t.Errorf("%s after applying the patch = %q, %v; want %q", name, got, err, content)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Errorf("b.txt still exists after applying the patch")
	}

	// The host's worktree is clean again for the next job of the same branch.
	next := r.req(`s=$(git status --porcelain); echo "porcelain=[$s]"`, sha)
	r.submit(next)
	wantState(t, r.waitDone(next.ID), "succeeded")
	wantContains(t, "log of the next job", r.logText(next.ID), "porcelain=[]")
	r.noProcs(req.ID)
}

func scenarioGenFailed(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.genReq(`printf 'changed\n' > a.txt; exit 3`, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "failed")
	if st.PatchSHA256 != "" || st.PatchSize != 0 {
		t.Errorf("a failed gen job has patch_sha256 %q and patch_size %d", st.PatchSHA256, st.PatchSize)
	}
	_, err := r.c.Patch(t.Context(), req.ID)
	wantRemote(t, err, runner.ErrCodeNoPatch)
}

func scenarioRunNoPatch(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.req("true", sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	if st.PatchSHA256 != "" || st.PatchSize != 0 {
		t.Errorf("a run job has patch_sha256 %q and patch_size %d", st.PatchSHA256, st.PatchSize)
	}
	_, err := r.c.Patch(t.Context(), req.ID)
	wantRemote(t, err, runner.ErrCodeNoPatch)

	// A job that does not exist is unknown, not "without a patch".
	_, err = r.c.Patch(t.Context(), "1-01TEST00000000000009999999")
	wantRemote(t, err, runner.ErrCodeUnknownJob)
}

func scenarioGenEmpty(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")
	req := r.genReq("true", sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	if st.PatchSize != 0 || st.PatchSHA256 != hexSum(nil) {
		t.Errorf("an empty patch has patch_sha256 %q and patch_size %d, want the hash of nothing and 0", st.PatchSHA256, st.PatchSize)
	}
	if data := r.patchBytes(st); len(data) != 0 {
		t.Errorf("the patch of a gen job that changed nothing is %q, want empty", data)
	}
}

// applied checks out sha in a clone of the rig's repository, applies the patch
// there and returns the directory.
func (r *rig) applied(sha string, data []byte) string {
	r.t.Helper()
	dir := r.t.TempDir()
	git(r.t, dir, "clone", "-q", r.repo, ".")
	git(r.t, dir, "checkout", "-q", "--detach", sha)
	file := filepath.Join(r.t.TempDir(), "gen.patch")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		r.t.Fatalf("write the patch: %v", err)
	}
	git(r.t, dir, "apply", "--binary", file)
	return dir
}

// scenarioGenCommitted: a command that commits (here a change to a tracked
// file) moves HEAD of the worktree, but the patch is still against the commit
// that was sent: it holds the committed change and the one made after it.
func scenarioGenCommitted(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	r.commit("a.txt", "one\n")
	sha := r.commit("b.txt", "two\n")

	cmd := `printf 'changed\n' > a.txt; git -c user.name=t -c user.email=t@x commit -q -am x; ` +
		`printf 'later\n' > c.txt`
	req := r.genReq(cmd, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "succeeded")
	if st.PatchSize <= 0 {
		t.Fatalf("the patch of a gen job that committed is empty: %+v", st)
	}
	dir := r.applied(sha, r.patchBytes(st))
	for name, want := range map[string]string{"a.txt": "changed\n", "b.txt": "two\n", "c.txt": "later\n"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s after applying the patch = %q, %v; want %q", name, got, err, want)
		}
	}
	r.noProcs(req.ID)
}

// scenarioGenKeptCache: the cache directories that the reset keeps between
// jobs are not part of the next gen job's patch, even when the repository does
// not ignore them.
func scenarioGenKeptCache(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")

	first := r.req(`mkdir -p node_modules/pkg && printf 'cache\n' > node_modules/x && printf 'cache\n' > node_modules/pkg/y`, sha)
	r.submit(first)
	wantState(t, r.waitDone(first.ID), "succeeded")

	second := r.genReq(`ls node_modules/x && printf 'new\n' > c.txt`, sha)
	r.submit(second)
	st := r.waitDone(second.ID)
	wantState(t, st, "succeeded")
	wantContains(t, "log of the gen job", r.logText(second.ID), "node_modules/x")
	data := r.patchBytes(st)
	if strings.Contains(string(data), "node_modules") {
		t.Errorf("the patch holds files that were in the kept cache before the command:\n%s", data)
	}
	dir := r.applied(sha, data)
	if got, err := os.ReadFile(filepath.Join(dir, "c.txt")); err != nil || string(got) != "new\n" {
		t.Errorf("c.txt after applying the patch = %q, %v; want %q", got, err, "new\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		t.Errorf("node_modules exists after applying the patch")
	}

	// The cache is still there, and a gen job that changes nothing gives an
	// empty patch.
	third := r.genReq("ls node_modules/pkg/y", sha)
	r.submit(third)
	st = r.waitDone(third.ID)
	wantState(t, st, "succeeded")
	if data := r.patchBytes(st); len(data) != 0 {
		t.Errorf("the patch of a gen job that changed nothing is %q, want empty", data)
	}
}

// scenarioGenPatchTooLarge: a command that succeeds but leaves more than 8 MiB
// of new content ends failed at the patch stage, and the host takes the next
// job (the global lock is released, the worktree is clean).
func scenarioGenPatchTooLarge(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")

	req := r.genReq(`head -c 9000000 /dev/zero | tr '\000' a > big.txt`, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	wantState(t, st, "failed")
	if st.Stage != "patch" || st.FailureKind != "user" {
		t.Errorf("stage = %q, failure_kind = %q, want patch and user: %+v", st.Stage, st.FailureKind, st)
	}
	wantContains(t, "reason", st.Reason, "8 MiB")
	if st.PatchSHA256 != "" || st.PatchSize != 0 {
		t.Errorf("a job without a patch has patch_sha256 %q and patch_size %d", st.PatchSHA256, st.PatchSize)
	}
	_, err := r.c.Patch(t.Context(), req.ID)
	wantRemote(t, err, runner.ErrCodeNoPatch)

	next := r.req(`test ! -e big.txt && echo clean`, sha)
	r.submit(next)
	wantState(t, r.waitDone(next.ID), "succeeded")
	wantContains(t, "log of the next job", r.logText(next.ID), "clean")
	r.noProcs(req.ID)
}

// scenarioGenPatchReason: when the patch cannot be computed (here a git
// repository without a commit left inside the tree), the job says why.
func scenarioGenPatchReason(t *testing.T, b host.Backend) {
	r := newRig(t, b)
	sha := r.commit("a.txt", "one\n")

	req := r.genReq(`git init -q sub && printf 'x\n' > sub/f`, sha)
	r.submit(req)
	st := r.waitDone(req.ID)
	if st.State != "failed" && st.State != "infra_failed" {
		t.Fatalf("job %s is %s (%s), want failed or infra_failed: %+v", st.ID, st.State, st.Reason, st)
	}
	if st.Stage != "patch" {
		t.Errorf("stage = %q, want patch: %+v", st.Stage, st)
	}
	if st.Reason == "" || st.Reason == "cannot compute the patch" {
		t.Errorf("reason = %q, want the cause of the failure", st.Reason)
	}
	wantContains(t, "reason", st.Reason, "commit")
	_, err := r.c.Patch(t.Context(), req.ID)
	wantRemote(t, err, runner.ErrCodeNoPatch)
}
