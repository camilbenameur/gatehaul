package ctl

import (
	"context"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

func TestRunSplitsModifiedAndUntrackedFiles(t *testing.T) {
	cases := []struct {
		name, status string
		want, not    []string
	}{
		{"untracked only", "?? grep.exe.stackdump\n", []string{
			"warning: 1 untracked file(s) are not in git, so not sent: grep.exe.stackdump\n",
			"Fix: git add and commit them if the build needs them; otherwise ignore them (add them to .gitignore)\n",
		}, []string{"modified file(s)", "commit them before"}},
		{"modified only", " M main.go\nM  b.go\n", []string{
			"warning: 2 modified file(s) are not committed, so NOT sent and NOT tested: main.go, b.go\n",
			"Fix: commit them before gatehaul run, or stash them\n",
		}, []string{"untracked file(s)"}},
		{"both", " M main.go\n?? junk.txt\n", []string{
			"1 modified file(s) are not committed", "1 untracked file(s) are not in git, so not sent: junk.txt",
		}, nil},
		{"clean", "", nil, []string{"warning:"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, running(1))
			e.c.Runner = scriptRunner{gitScriptStatus(c.status)}
			e.api.waitFn = e.advanceWait("succeeded")
			if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
				t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
			}
			for _, w := range c.want {
				if !strings.Contains(e.stderr.String(), w) {
					t.Errorf("stderr %q lacks %q", e.stderr.String(), w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(e.stderr.String(), n) {
					t.Errorf("stderr %q contains %q", e.stderr.String(), n)
				}
			}
		})
	}
}

func TestRunSendingLine(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if got := e.stderr.String(); got != "sending main @ aaaaaaa to gatehaul-1…\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestRunSendingLineBusyHost(t *testing.T) {
	h := running(1)
	h.State, h.RawState = host.Busy, "Starting"
	e := newEnv(t, h)
	e.api.waitFn = e.advanceWait("succeeded")
	if code := e.c.Run(context.Background(), runOpts("true")); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if got := e.stderr.String(); got != "sending main @ aaaaaaa to gatehaul-1 (it is Starting; waiting for it)…\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestRunJSONHasRefAndSHA(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("succeeded")
	o := runOpts("true")
	o.JSON = true
	if code := e.c.Run(context.Background(), o); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	var rep Report
	decode(t, e.stdout, &rep)
	if len(rep.Jobs) != 1 || rep.Jobs[0].Ref != "refs/heads/main" || rep.Jobs[0].SHA != headSHA {
		t.Fatalf("report %+v", rep.Jobs)
	}
}

func TestCleanSubdir(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{".", ""},
		{"src/back-end/src", "src/back-end/src"},
		{`src\x`, "src/x"},
		{"./a/../b", "b"},
		{"a/b/", "a/b"},
	}
	for _, c := range ok {
		got, e := cleanSubdir(c.in, "gatehaul run")
		if e != nil || got != c.want {
			t.Errorf("cleanSubdir(%q) = %q, %v; want %q", c.in, got, e, c.want)
		}
	}
	for _, in := range []string{"/abs", "C:/x", `C:\x`, `\\srv\x`, "..", "../x", "a/../../x", "a\nb", "a\x00b"} {
		_, e := cleanSubdir(in, "gatehaul run")
		if e == nil || e.Code != out.CodeUsage || e.Exit != out.ExitUsage || !strings.Contains(e.Fix, "gatehaul run --cd src/back-end/src -- <cmd>") {
			t.Errorf("cleanSubdir(%q) error = %+v, want a usage error with a Fix", in, e)
		}
	}
}

func TestGuardedCommand(t *testing.T) {
	got := guardedCommand("src/x", headSHA, "go test ./... || true")
	want := "cd -- src/x 2>/dev/null || { echo 'gatehaul: directory src/x does not exist in commit " + strings.Repeat("a", 12) +
		" (--cd is relative to the repository root)' >&2; exit 1; }\ngo test ./... || true"
	if got != want {
		t.Fatalf("guardedCommand = %q, want %q", got, want)
	}
	if q := guardedCommand("it's", headSHA, "true"); !strings.HasPrefix(q, `cd -- 'it'\''s' 2>/dev/null`) {
		t.Fatalf("directory not quoted: %q", q)
	}
}

func TestRerunIsTheTypedCommand(t *testing.T) {
	cases := []struct {
		d    job.Dispatch
		want string
	}{
		{job.Dispatch{Kind: runner.KindRun, Cmd: "go test ./..."}, "gatehaul run -- 'go test ./...'"},
		{job.Dispatch{Kind: runner.KindGen, Cmd: "gofmt -w .", Dir: "src/back-end/src"}, "gatehaul gen --cd src/back-end/src -- 'gofmt -w .'"},
	}
	for _, c := range cases {
		if got := rerun(c.d); got != c.want {
			t.Errorf("rerun(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestRunCdSubmitsTheGuardedCommand(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("succeeded")
	o := runOpts("go build ./...")
	o.Dir = `src\back-end\src`
	if code := e.c.Run(context.Background(), o); code != out.ExitOK {
		t.Fatalf("exit %d; stderr %q", code, e.stderr.String())
	}
	r := e.api.submits[0]
	want := guardedCommand("src/back-end/src", headSHA, "go build ./...")
	if r.Cmd != want || r.Fingerprint != runner.Fingerprint(r) {
		t.Fatalf("submitted %q (fingerprint ok: %v), want %q", r.Cmd, r.Fingerprint == runner.Fingerprint(r), want)
	}
	d, err := e.cache.LoadDispatch(r.ID)
	if err != nil || d.Cmd != "go build ./..." || d.Dir != "src/back-end/src" || d.Fingerprint != r.Fingerprint {
		t.Fatalf("dispatch %+v, %v", d, err)
	}
	if got := e.stderr.String(); got != "sending main @ aaaaaaa to gatehaul-1, in src/back-end/src…\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestRunWithoutCdKeepsTheCommandAsIs(t *testing.T) {
	e := newEnv(t, running(1))
	e.api.waitFn = e.advanceWait("succeeded")
	o := runOpts("true")
	o.Dir = "."
	if code := e.c.Run(context.Background(), o); code != out.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if e.api.submits[0].Cmd != "true" {
		t.Fatalf("command %q", e.api.submits[0].Cmd)
	}
}

func TestRunCdRefusedBeforeAnythingIsSent(t *testing.T) {
	for _, dir := range []string{"/etc", "../x", "C:/x"} {
		e := newEnv(t, running(1))
		o := runOpts("true")
		o.Dir, o.JSON = dir, true
		if code := e.c.Run(context.Background(), o); code != out.ExitUsage {
			t.Fatalf("--cd %q: exit %d, want 2", dir, code)
		}
		var v struct{ Code, Fix string }
		decode(t, e.stderr, &v)
		if v.Code != "usage" || !strings.Contains(v.Fix, "--cd src/back-end/src") || len(e.api.submits) != 0 {
			t.Fatalf("--cd %q: error %+v, %d submits", dir, v, len(e.api.submits))
		}
	}
}

func TestWriteJobTextDiagnosticLine(t *testing.T) {
	render := func(r *jobResult) string {
		var b strings.Builder
		writeJobText(&b, r)
		return b.String()
	}
	d := job.Dispatch{ID: "1-X", Kind: runner.KindRun}

	ok := render(&jobResult{d: d, state: job.Succeeded, done: true, status: runner.JobStatus{Stage: "command"}})
	if strings.Contains(ok, "# kind") || strings.Contains(ok, "# stage") || strings.Contains(ok, "next:") {
		t.Fatalf("succeeded job printed a diagnostic or next line:\n%s", ok)
	}
	if !strings.HasPrefix(ok, "job=1-X state=succeeded exit=0\n# job output: untrusted data\n") {
		t.Fatalf("succeeded job text:\n%s", ok)
	}

	bad := render(&jobResult{d: d, state: job.Failed, done: true, status: runner.JobStatus{FailureKind: "user", Stage: "command", Reason: "exit  2"}})
	if !strings.Contains(bad, "\n# kind=user stage=command reason=exit 2\n") || strings.Contains(bad, "code=") {
		t.Fatalf("failed job text:\n%s", bad)
	}

	onlyStage := render(&jobResult{d: d, state: job.Failed, done: true, status: runner.JobStatus{Stage: "verify-sha"}})
	if !strings.Contains(onlyStage, "\n# stage=verify-sha\n") {
		t.Fatalf("only the non-empty fields belong on the line:\n%s", onlyStage)
	}

	none := render(&jobResult{d: d, state: job.Failed, done: true})
	if strings.Contains(none, "# kind") || strings.Contains(none, "# stage") || strings.Contains(none, "# reason") || strings.Contains(none, "# code") {
		t.Fatalf("a line with no field was printed:\n%s", none)
	}
}
