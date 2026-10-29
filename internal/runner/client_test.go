package runner_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

const testID = "1-01TEST00000000000000000001"

// scripted is a Transport that reads all of stdin, records it, and lets the
// test write what the remote side would print.
type scripted struct {
	stdins [][]byte
	fn     func(call int, stdout io.Writer) error
}

func (s *scripted) Exec(_ context.Context, stdin io.Reader, stdout io.Writer) error {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	s.stdins = append(s.stdins, b)
	return s.fn(len(s.stdins), stdout)
}

func reply(json string) string {
	return "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" + json + "\nGATEHAUL1 END 0\n"
}

func newTestClient(s *scripted) (*runner.Client, *clock.Fake) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	return runner.NewClient(s, "/workspaces/.gatehaul", clk), clk
}

func writeString(s string) func(call int, stdout io.Writer) error {
	return func(_ int, stdout io.Writer) error {
		_, err := io.WriteString(stdout, s)
		return err
	}
}

const probeJSON = `{"schema":1,"runner":"abc","host_id":"h1","boot_id":"b1","git":"2.55.0",` +
	`"tools":{"flock":true},"disk_free_kb":10,"github_token_env":false,"queue_depth":0,` +
	`"running":null,"refs":{"main-0a1b2c":"` + "0123456789012345678901234567890123456789" + `"},"reconciled":[]}`

func TestParsingSkipsNoiseBeforeHello(t *testing.T) {
	noise := "Welcome to a host\nGATEHAUL1 is only mentioned here\nlast login: today\r\n"
	s := &scripted{fn: writeString(noise + reply(probeJSON))}
	c, _ := newTestClient(s)
	p, err := c.Probe(t.Context())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p.HostID != "h1" || p.Git != "2.55.0" || !p.Tools["flock"] || p.Running != nil {
		t.Errorf("probe = %+v", p)
	}
	if got := p.Refs["main-0a1b2c"]; got != "0123456789012345678901234567890123456789" {
		t.Errorf("refs = %v", p.Refs)
	}
}

func TestHelloMayFollowNoiseOnTheSameLine(t *testing.T) {
	// Noise without a final newline: HELLO is not at the start of its line.
	out := "last login: today, no newline" + reply(probeJSON)
	c, _ := newTestClient(&scripted{fn: writeString(out)})
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func validSubmit() runner.SubmitRequest {
	return runner.SubmitRequest{
		ID: testID, Key: "main-0a1b2c", SHA: strings.Repeat("d", 40), Kind: runner.KindRun,
		Prio: 5, Timeout: time.Minute, Cmd: "true",
	}
}

func TestSubmitReplyThatCannotBeReadAfterHelloIsOutcomeUnknown(t *testing.T) {
	huge := func(_ int, w io.Writer) error {
		_, _ = io.WriteString(w, "GATEHAUL1 HELLO v\n")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
	}
	cases := map[string]func(int, io.Writer) error{
		"not json":        writeString("GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\nhello\nGATEHAUL1 END 0\n"),
		"line after json": writeString("GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n{}\nnoise\nGATEHAUL1 END 0\n"),
		"too large":       huge,
	}
	for name, fn := range cases {
		s := &scripted{fn: fn}
		c, _ := newTestClient(s)
		_, err := c.Submit(t.Context(), validSubmit())
		if !errors.Is(err, runner.ErrProtocol) || !errors.Is(err, runner.ErrOutcomeUnknown) {
			t.Errorf("%s: error = %v, want ErrProtocol and ErrOutcomeUnknown (the job may exist)", name, err)
		}
		if len(s.stdins) != 1 {
			t.Errorf("%s: %d connections, want 1", name, len(s.stdins))
		}
	}
}

func TestMarkersWithoutHelloAreNotAScriptThatNeverStarted(t *testing.T) {
	out := "garbage\nGATEHAUL1 BEGIN\n{}\n"
	s := &scripted{fn: writeString(out)}
	c, _ := newTestClient(s)
	_, err := c.Submit(t.Context(), validSubmit())
	if !errors.Is(err, runner.ErrOutcomeUnknown) || errors.Is(err, runner.ErrNotStarted) {
		t.Fatalf("error = %v, want ErrOutcomeUnknown and not ErrNotStarted", err)
	}
	if len(s.stdins) != 1 {
		t.Errorf("%d connections, want 1: a submit that may have run is never repeated", len(s.stdins))
	}
}

func TestPermanentTransportErrorIsNotRetried(t *testing.T) {
	failure := &host.ExecError{Op: "gh codespace ssh", Err: errors.New("gh is not installed"), Permanent: true}
	s := &scripted{fn: func(int, io.Writer) error { return failure }}
	c, clk := newTestClient(s)
	start := clk.Now()
	_, err := c.Probe(t.Context())
	if !host.IsPermanent(err) {
		t.Fatalf("error = %v, want the permanent transport error", err)
	}
	if errors.Is(err, runner.ErrNotStarted) {
		t.Errorf("a permanent failure is reported as it is, not as a script that did not start")
	}
	if len(s.stdins) != 1 || clk.Now().Sub(start) != 0 {
		t.Errorf("%d connections, waited %s: want one attempt and no wait", len(s.stdins), clk.Now().Sub(start))
	}
}

func TestParsingToleratesCRLF(t *testing.T) {
	out := strings.ReplaceAll(reply(probeJSON), "\n", "\r\n")
	c, _ := newTestClient(&scripted{fn: writeString(out)})
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe with CRLF line ends: %v", err)
	}
}

func TestMissingEndIsOutcomeUnknown(t *testing.T) {
	for name, out := range map[string]string{
		"hello only":   "GATEHAUL1 HELLO v\n",
		"after begin":  "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n",
		"json no end":  "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" + probeJSON + "\n",
		"cut end line": "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" + probeJSON + "\nGATEHAUL1 END",
		"cut json":     "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" + probeJSON[:20],
	} {
		s := &scripted{fn: func(_ int, w io.Writer) error {
			_, _ = io.WriteString(w, out)
			return errors.New("connection reset")
		}}
		c, _ := newTestClient(s)
		_, err := c.Probe(t.Context())
		if !errors.Is(err, runner.ErrOutcomeUnknown) {
			t.Errorf("%s: error = %v, want ErrOutcomeUnknown", name, err)
		}
		if errors.Is(err, runner.ErrNotStarted) {
			t.Errorf("%s: a stream that started must not be ErrNotStarted", name)
		}
		if len(s.stdins) != 1 {
			t.Errorf("%s: %d connections, want 1 (never retried after HELLO)", name, len(s.stdins))
		}
	}
}

func TestEndWithErrorIsRemoteError(t *testing.T) {
	out := "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" +
		`{"schema":1,"error":{"code":"need_full","message":"send a full bundle"}}` +
		"\nGATEHAUL1 END 1\n"
	c, _ := newTestClient(&scripted{fn: writeString(out)})
	_, err := c.Probe(t.Context())
	var re *runner.RemoteError
	if !errors.As(err, &re) || re.Code != runner.ErrCodeNeedFull || re.Message != "send a full bundle" {
		t.Fatalf("error = %v, want a need_full RemoteError", err)
	}
}

func TestUnparsableIsProtocolError(t *testing.T) {
	for name, out := range map[string]string{
		"not json":         "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\nhello\nGATEHAUL1 END 0\n",
		"error without it": "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n{}\nGATEHAUL1 END 1\n",
		"line after json":  "GATEHAUL1 HELLO v\nGATEHAUL1 BEGIN\n" + probeJSON + "\nnoise\nGATEHAUL1 END 0\n",
	} {
		c, _ := newTestClient(&scripted{fn: writeString(out)})
		_, err := c.Probe(t.Context())
		if !errors.Is(err, runner.ErrProtocol) {
			t.Errorf("%s: error = %v, want ErrProtocol", name, err)
		}
	}
}

func TestReplyTooLargeIsProtocolError(t *testing.T) {
	huge := "GATEHAUL1 HELLO v\n" + strings.Repeat("x", 5<<20)
	c, _ := newTestClient(&scripted{fn: writeString(huge)})
	_, err := c.Probe(t.Context())
	if !errors.Is(err, runner.ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}

func TestRetriesWhileTheScriptNeverStarted(t *testing.T) {
	failure := &host.ExecError{Op: "gh codespace ssh", StderrTail: "connection refused", Err: errors.New("exit status 255")}
	s := &scripted{fn: func(call int, w io.Writer) error {
		if call <= 2 {
			_, _ = io.WriteString(w, "Warning: noise from the shell\n")
			return failure
		}
		_, err := io.WriteString(w, reply(probeJSON))
		return err
	}}
	c, clk := newTestClient(s)
	start := clk.Now()
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(s.stdins) != 3 {
		t.Errorf("%d connections, want 3", len(s.stdins))
	}
	if got := clk.Now().Sub(start); got != 6*time.Second {
		t.Errorf("waited %s, want 2s then 4s", got)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	s := &scripted{fn: func(call int, w io.Writer) error {
		if call <= 8 {
			return errors.New("refused")
		}
		_, err := io.WriteString(w, reply(probeJSON))
		return err
	}}
	c, clk := newTestClient(s)
	start := clk.Now()
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 2 + 4 + 8 + 16 + 30 + 30 + 30 + 30
	if got := clk.Now().Sub(start); got != 150*time.Second {
		t.Errorf("waited %s, want 150s", got)
	}
}

func TestGivesUpWithErrNotStarted(t *testing.T) {
	s := &scripted{fn: func(int, io.Writer) error {
		return &host.ExecError{Op: "gh codespace ssh", StderrTail: "no such codespace", Err: errors.New("exit status 1")}
	}}
	c, _ := newTestClient(s)
	c.StartRetry = 10 * time.Second
	_, err := c.Probe(t.Context())
	if !errors.Is(err, runner.ErrNotStarted) {
		t.Fatalf("error = %v, want ErrNotStarted", err)
	}
	if !strings.Contains(err.Error(), "no such codespace") {
		t.Errorf("error %q lost the transport's text", err)
	}
	var ee *host.ExecError
	if !errors.As(err, &ee) {
		t.Errorf("error %v does not unwrap to the transport's error", err)
	}
	// Attempts at 0 s, 2 s, 6 s and 14 s.
	if len(s.stdins) != 4 {
		t.Errorf("%d connections, want 4", len(s.stdins))
	}
}

func TestSilentTransportIsRetried(t *testing.T) {
	s := &scripted{fn: func(call int, w io.Writer) error {
		if call == 1 {
			return nil // exited without a word: the script never ran
		}
		_, err := io.WriteString(w, reply(probeJSON))
		return err
	}}
	c, _ := newTestClient(s)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(s.stdins) != 2 {
		t.Errorf("%d connections, want 2", len(s.stdins))
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := &scripted{fn: func(int, io.Writer) error {
		cancel()
		return context.Canceled
	}}
	c, _ := newTestClient(s)
	_, err := c.Probe(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, runner.ErrNotStarted) {
		t.Errorf("a cancelled call is not a runner that did not start")
	}
	if len(s.stdins) != 1 {
		t.Errorf("%d connections, want 1", len(s.stdins))
	}
}

func TestSubmitSendsPreambleScriptHeaderAndBundle(t *testing.T) {
	opened := 0
	req := runner.SubmitRequest{
		ID:      testID,
		Key:     "main-0a1b2c",
		SHA:     strings.Repeat("a", 40),
		Kind:    runner.KindGen,
		Prio:    3,
		Timeout: 90 * time.Second,
		Cmd:     "make test && echo 'done'",
		Bundle: func() (io.ReadCloser, int64, error) {
			opened++
			return io.NopCloser(strings.NewReader("BUNDLE")), 6, nil
		},
	}
	s := &scripted{fn: func(call int, w io.Writer) error {
		if call == 1 {
			return errors.New("connection refused")
		}
		_, err := io.WriteString(w, reply(`{"schema":1,"id":"`+testID+`","status":"accepted","state":"queued"}`))
		return err
	}}
	c, _ := newTestClient(s)
	res, err := c.Submit(t.Context(), req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Status != runner.SubmitAccepted || res.State != "queued" || res.ID != testID {
		t.Errorf("result = %+v", res)
	}
	if opened != 2 {
		t.Errorf("Bundle was called %d times, want once per connection (2)", opened)
	}

	in := s.stdins[1]
	pre := runner.Preamble("/workspaces/.gatehaul")
	if !bytes.HasPrefix(in, pre) {
		t.Fatalf("stdin does not start with the preamble")
	}
	rest := in[len(pre):]
	if !bytes.HasPrefix(rest, runner.Script()) {
		t.Fatalf("the script does not follow the preamble")
	}
	rest = rest[len(runner.Script()):]
	want := "GATEHAUL1 submit\n" +
		"id=" + testID + "\n" +
		"key=main-0a1b2c\n" +
		"sha=" + req.SHA + "\n" +
		"fp=" + runner.Fingerprint(req) + "\n" +
		"kind=gen\n" +
		"prio=3\n" +
		"timeout_s=90\n" +
		"cmd_b64=" + base64.StdEncoding.EncodeToString([]byte(req.Cmd)) + "\n" +
		"bundle_len=6\n" +
		"\nBUNDLE"
	if string(rest) != want {
		t.Errorf("request = %q\nwant      %q", rest, want)
	}
}

func TestSubmitRejectsBadRequestsLocally(t *testing.T) {
	good := runner.SubmitRequest{
		ID: testID, Key: "main-0a1b2c", SHA: strings.Repeat("b", 40), Kind: runner.KindRun,
		Prio: 5, Timeout: time.Minute, Cmd: "true",
	}
	s := &scripted{fn: writeString("")}
	c, _ := newTestClient(s)
	for name, mutate := range map[string]func(*runner.SubmitRequest){
		"id":      func(r *runner.SubmitRequest) { r.ID = "../x" },
		"key":     func(r *runner.SubmitRequest) { r.Key = "Main" },
		"sha":     func(r *runner.SubmitRequest) { r.SHA = "abc" },
		"kind":    func(r *runner.SubmitRequest) { r.Kind = "exec" },
		"prio":    func(r *runner.SubmitRequest) { r.Prio = 10 },
		"timeout": func(r *runner.SubmitRequest) { r.Timeout = 500 * time.Millisecond },
		"command": func(r *runner.SubmitRequest) { r.Cmd = "" },
	} {
		r := good
		mutate(&r)
		if _, err := c.Submit(t.Context(), r); err == nil {
			t.Errorf("%s: the bad request was sent", name)
		}
	}
	if len(s.stdins) != 0 {
		t.Errorf("%d connections for requests that are invalid", len(s.stdins))
	}
}

func TestSubmitBundleErrorStopsAtOnce(t *testing.T) {
	req := runner.SubmitRequest{
		ID: testID, Key: "main-0a1b2c", SHA: strings.Repeat("c", 40), Kind: runner.KindRun,
		Prio: 5, Timeout: time.Minute, Cmd: "true",
		Bundle: func() (io.ReadCloser, int64, error) { return nil, 0, errors.New("disk on fire") },
	}
	s := &scripted{fn: writeString("")}
	c, _ := newTestClient(s)
	_, err := c.Submit(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("error = %v, want the bundle's error", err)
	}
	if len(s.stdins) != 0 {
		t.Errorf("a connection was opened without a bundle")
	}
}

func TestWaitHeaderAndResult(t *testing.T) {
	s := &scripted{fn: writeString(reply(
		`{"schema":1,"jobs":[{"schema":1,"id":"` + testID + `","state":"succeeded","exit_code":0,` +
			`"log_tail_b64":"aGk=","queue_pos":0}]}`))}
	c, _ := newTestClient(s)
	sts, err := c.Wait(t.Context(), runner.WaitRequest{
		IDs: []string{testID, "2-01TEST00000000000000000002"}, Slice: 4500 * time.Millisecond, All: true, TailBytes: 2048,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(sts) != 1 || sts[0].State != "succeeded" || sts[0].ExitCode == nil || *sts[0].ExitCode != 0 {
		t.Errorf("statuses = %+v", sts)
	}
	in := string(s.stdins[0])
	want := "GATEHAUL1 wait\nids=" + testID + ",2-01TEST00000000000000000002\ntimeout_s=5\nall=1\ntail_bytes=2048\n\n"
	if !strings.HasSuffix(in, want) {
		t.Errorf("request = %q, want it to end with %q", in[len(in)-len(want)-20:], want)
	}

	if _, err := c.Wait(t.Context(), runner.WaitRequest{}); err == nil {
		t.Errorf("a wait without ids was accepted")
	}
	if _, err := c.Wait(t.Context(), runner.WaitRequest{IDs: []string{"nope"}}); err == nil {
		t.Errorf("a wait on a malformed id was accepted")
	}
}

func TestWaitCapsTheSlice(t *testing.T) {
	s := &scripted{fn: writeString(reply(`{"schema":1,"jobs":[]}`))}
	c, _ := newTestClient(s)
	if _, err := c.Wait(t.Context(), runner.WaitRequest{IDs: []string{testID}, Slice: time.Hour, TailBytes: 1 << 30}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	in := string(s.stdins[0])
	if !strings.Contains(in, "\ntimeout_s=60\n") || !strings.Contains(in, "\ntail_bytes=102400\n") {
		t.Errorf("the slice and the tail are not capped: %q", in[len(in)-120:])
	}
}

func TestLogHeaderAndResult(t *testing.T) {
	s := &scripted{fn: writeString(reply(
		`{"schema":1,"id":"` + testID + `","state":"running","data_b64":"aGk=","cursor":2,"size":9,"truncated":true}`))}
	c, _ := newTestClient(s)
	ch, err := c.Log(t.Context(), runner.LogRequest{ID: testID, Since: 5, MaxBytes: 1 << 30, Grep: "fail|error", TailLines: 20})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if ch.Cursor != 2 || ch.Size != 9 || !ch.Truncated || ch.DataB64 != "aGk=" || ch.State != "running" {
		t.Errorf("chunk = %+v", ch)
	}
	in := string(s.stdins[0])
	want := "GATEHAUL1 log\nid=" + testID + "\nsince=5\nmax_bytes=102400\ntail_lines=20\n" +
		"grep_b64=" + base64.StdEncoding.EncodeToString([]byte("fail|error")) + "\n\n"
	if !strings.HasSuffix(in, want) {
		t.Errorf("request does not end with %q: %q", want, in[len(in)-len(want)-20:])
	}
}

func TestCancelAndGC(t *testing.T) {
	status := `{"schema":1,"id":"` + testID + `","state":"cancelled","exit_code":null}`
	s := &scripted{fn: func(call int, w io.Writer) error {
		if call == 1 {
			_, err := io.WriteString(w, reply(status))
			return err
		}
		_, err := io.WriteString(w, reply(`{"schema":1,"removed":3}`))
		return err
	}}
	c, _ := newTestClient(s)
	st, err := c.Cancel(t.Context(), testID)
	if err != nil || st.State != "cancelled" || st.ExitCode != nil {
		t.Fatalf("cancel = %+v, %v", st, err)
	}
	if !strings.HasSuffix(string(s.stdins[0]), "GATEHAUL1 cancel\nid="+testID+"\n\n") {
		t.Errorf("cancel request = %q", s.stdins[0][len(s.stdins[0])-60:])
	}
	n, err := c.GC(t.Context(), 7)
	if err != nil || n != 3 {
		t.Fatalf("gc = %d, %v; want 3", n, err)
	}
	if !strings.HasSuffix(string(s.stdins[1]), "GATEHAUL1 gc\ndays=7\n\n") {
		t.Errorf("gc request = %q", s.stdins[1][len(s.stdins[1])-40:])
	}
}

func TestPreambleIsSentFirstOnEveryCall(t *testing.T) {
	s := &scripted{fn: writeString(reply(probeJSON))}
	c, _ := newTestClient(s)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !bytes.HasPrefix(s.stdins[0], runner.Preamble("/workspaces/.gatehaul")) {
		t.Errorf("stdin does not start with the preamble")
	}
	if !bytes.HasSuffix(s.stdins[0], []byte("GATEHAUL1 probe\n\n")) {
		t.Errorf("stdin does not end with the probe request")
	}
}
