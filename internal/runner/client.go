package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/host"
)

const (
	// defaultStartRetry is how long a connection that never started is retried.
	defaultStartRetry = 5 * time.Minute
	firstBackoff      = 2 * time.Second
	maxBackoff        = 30 * time.Second
	// replySlack is what the client reads from stdout beyond the payload it
	// expects: shell noise, the protocol lines, the JSON around the data.
	replySlack = 4 << 20
	// maxCmdBytes bounds the command text sent in a header.
	maxCmdBytes = 512 << 10
)

// Client talks to the runner of one host over a host.Transport. It implements
// API.
type Client struct {
	// StartRetry is how long a call is retried while the remote script never
	// started (a codespace that is waking up, ssh refused). After the HELLO
	// line, a call is never retried.
	StartRetry time.Duration

	t     host.Transport
	root  string
	clock clock.Clock
}

var _ API = (*Client)(nil)

// NewClient returns a Client for the runner under root on the host reached by
// t. Waiting between retries goes through c.
func NewClient(t host.Transport, root string, c clock.Clock) *Client {
	return &Client{StartRetry: defaultStartRetry, t: t, root: root, clock: c}
}

type field struct{ key, val string }

// GCResult is the response to "gc".
type GCResult struct {
	Schema  int `json:"schema"`
	Removed int `json:"removed"`
}

// Probe implements API.
func (c *Client) Probe(ctx context.Context) (Probe, error) {
	var p Probe
	err := c.call(ctx, CmdProbe, nil, nil, 0, &p)
	return p, err
}

// Submit implements API.
func (c *Client) Submit(ctx context.Context, r SubmitRequest) (SubmitResult, error) {
	var res SubmitResult
	if !IDPattern.MatchString(r.ID) {
		return res, fmt.Errorf("runner: invalid job id %q", r.ID)
	}
	if !KeyPattern.MatchString(r.Key) {
		return res, fmt.Errorf("runner: invalid branch key %q", r.Key)
	}
	if !SHAPattern.MatchString(r.SHA) {
		return res, fmt.Errorf("runner: invalid commit id %q", r.SHA)
	}
	if r.Kind != KindRun && r.Kind != KindGen {
		return res, fmt.Errorf("runner: invalid job kind %q", r.Kind)
	}
	if r.Prio < 0 || r.Prio > 9 {
		return res, fmt.Errorf("runner: priority %d is not between 0 and 9", r.Prio)
	}
	secs := int64(r.Timeout / time.Second)
	if secs < 1 {
		return res, fmt.Errorf("runner: timeout %s is shorter than one second", r.Timeout)
	}
	if r.Cmd == "" || len(r.Cmd) > maxCmdBytes {
		return res, fmt.Errorf("runner: the command is empty or longer than %d bytes", maxCmdBytes)
	}
	fp := r.Fingerprint
	if fp == "" {
		fp = Fingerprint(r)
	}
	fields := []field{
		{"id", r.ID},
		{"key", r.Key},
		{"sha", r.SHA},
		{"fp", fp},
		{"kind", r.Kind},
		{"prio", strconv.Itoa(r.Prio)},
		{"timeout_s", strconv.FormatInt(secs, 10)},
		{"cmd_b64", base64.StdEncoding.EncodeToString([]byte(r.Cmd))},
	}
	err := c.call(ctx, CmdSubmit, fields, r.Bundle, 0, &res)
	return res, err
}

// Wait implements API. It returns the status of every id, finished or not.
func (c *Client) Wait(ctx context.Context, r WaitRequest) ([]JobStatus, error) {
	if len(r.IDs) == 0 {
		return nil, errors.New("runner: wait needs at least one job id")
	}
	for _, id := range r.IDs {
		if !IDPattern.MatchString(id) {
			return nil, fmt.Errorf("runner: invalid job id %q", id)
		}
	}
	slice := r.Slice
	if slice > MaxWaitSlice {
		slice = MaxWaitSlice
	}
	if slice < 0 {
		slice = 0
	}
	rounded := slice + time.Second - 1
	waitSecs := int64(rounded / time.Second)
	tail := r.TailBytes
	if tail < 0 {
		tail = 0
	}
	if tail > MaxLogChunk {
		tail = MaxLogChunk
	}
	all := "0"
	if r.All {
		all = "1"
	}
	fields := []field{
		{"ids", strings.Join(r.IDs, ",")},
		{"timeout_s", strconv.FormatInt(waitSecs, 10)},
		{"all", all},
		{"tail_bytes", strconv.Itoa(tail)},
	}
	var res WaitResult
	perJob := tail*4/3 + 8192
	extra := len(r.IDs) * perJob
	if err := c.call(ctx, CmdWait, fields, nil, extra, &res); err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// Log implements API.
func (c *Client) Log(ctx context.Context, r LogRequest) (LogChunk, error) {
	var res LogChunk
	if !IDPattern.MatchString(r.ID) {
		return res, fmt.Errorf("runner: invalid job id %q", r.ID)
	}
	maxBytes := r.MaxBytes
	if maxBytes <= 0 || maxBytes > MaxLogChunk {
		maxBytes = MaxLogChunk
	}
	since := r.Since
	if since < 0 {
		since = 0
	}
	tailLines := r.TailLines
	if tailLines < 0 {
		tailLines = 0
	}
	fields := []field{
		{"id", r.ID},
		{"since", strconv.FormatInt(since, 10)},
		{"max_bytes", strconv.Itoa(maxBytes)},
		{"tail_lines", strconv.Itoa(tailLines)},
	}
	if r.Grep != "" {
		fields = append(fields, field{"grep_b64", base64.StdEncoding.EncodeToString([]byte(r.Grep))})
	}
	extra := maxBytes*4/3 + 8192
	err := c.call(ctx, CmdLog, fields, nil, extra, &res)
	return res, err
}

// Cancel implements API.
func (c *Client) Cancel(ctx context.Context, id string) (JobStatus, error) {
	var res JobStatus
	if !IDPattern.MatchString(id) {
		return res, fmt.Errorf("runner: invalid job id %q", id)
	}
	err := c.call(ctx, CmdCancel, []field{{"id", id}}, nil, 0, &res)
	return res, err
}

// Patch implements API. It returns the patch of a gen job that succeeded, once
// its bytes are checked against the size and sha256 the runner announced.
func (c *Client) Patch(ctx context.Context, id string) (PatchData, error) {
	var res PatchData
	if !IDPattern.MatchString(id) {
		return res, fmt.Errorf("runner: invalid job id %q", id)
	}
	extra := MaxPatchBytes*4/3 + 8192
	if err := c.call(ctx, CmdPatch, []field{{"id", id}}, nil, extra, &res); err != nil {
		return PatchData{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(res.DataB64)
	if err != nil {
		return PatchData{}, fmt.Errorf("runner %s: the patch is not valid base64: %v: %w", CmdPatch, err, ErrProtocol)
	}
	if int64(len(raw)) != res.Size {
		return PatchData{}, fmt.Errorf("runner %s: the patch has %d bytes, the runner announced %d: %w",
			CmdPatch, len(raw), res.Size, ErrProtocol)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != res.SHA256 {
		return PatchData{}, fmt.Errorf("runner %s: the sha256 of the patch does not match what the runner announced: %w",
			CmdPatch, ErrProtocol)
	}
	return res, nil
}

// GC removes the jobs that finished more than days days ago and returns how
// many it removed.
func (c *Client) GC(ctx context.Context, days int) (int, error) {
	if days < 0 || days > 3650 {
		return 0, fmt.Errorf("runner: gc days %d is out of range", days)
	}
	var res GCResult
	err := c.call(ctx, CmdGC, []field{{"days", strconv.Itoa(days)}}, nil, 0, &res)
	return res.Removed, err
}

// header builds the request header of the wire format.
func header(sub string, fields []field) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(Marker + " " + sub + "\n")
	for _, f := range fields {
		if f.key == "" || strings.ContainsAny(f.key, "=\n") {
			return nil, fmt.Errorf("runner: invalid header key %q", f.key)
		}
		for _, r := range f.val {
			if !validValueRune(r) {
				return nil, fmt.Errorf("runner: invalid character in header value %q", f.key)
			}
		}
		b.WriteString(f.key + "=" + f.val + "\n")
	}
	b.WriteString("\n")
	return b.Bytes(), nil
}

// validValueRune reports whether r may appear in a header value:
// [A-Za-z0-9._:,/+=-].
func validValueRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._:,/+=-", r)
}

// errTooLarge stops a transport that writes more than expected.
var errTooLarge = errors.New("runner reply too large")

// limitBuffer keeps what the transport writes, up to max bytes.
type limitBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		l.over = true
		return 0, errTooLarge
	}
	return l.buf.Write(p)
}

// reply is what parseReply found in the stdout of one call.
type reply struct {
	hello    bool   // the HELLO line was seen: the remote script started
	complete bool   // BEGIN, the JSON line and END were all seen
	bad      bool   // the lines after BEGIN are not what the protocol says
	rc       int    // the rc of the END line
	payload  []byte // the JSON line
}

// parseReply skips the noise before HELLO, then expects BEGIN, one line of
// JSON and END <rc>.
func parseReply(out []byte) reply {
	var r reply
	const (
		beforeHello = iota
		beforeBegin
		beforeJSON
		beforeEnd
	)
	state := beforeHello
	s := out
	for len(s) > 0 {
		var line []byte
		terminated := false
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line, s, terminated = s[:i], s[i+1:], true
		} else {
			line, s = s, nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch state {
		case beforeHello:
			// Noise without a final newline may sit in front of HELLO on the
			// same line, so the marker is searched for, not expected first.
			if bytes.Contains(line, []byte(Marker+" HELLO")) {
				r.hello = true
				state = beforeBegin
			}
		case beforeBegin:
			if terminated && string(line) == Marker+" BEGIN" {
				state = beforeJSON
			}
		case beforeJSON:
			if !terminated {
				return r
			}
			r.payload = append([]byte(nil), line...)
			state = beforeEnd
		case beforeEnd:
			rest, ok := strings.CutPrefix(string(line), Marker+" END ")
			if !ok || !terminated {
				r.bad = !ok && terminated
				return r
			}
			rc, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				r.bad = true
				return r
			}
			r.rc = rc
			r.complete = true
			return r
		}
	}
	return r
}

// call runs one request and decodes the response into out. extra is the size
// of the payload the reply may carry beyond replySlack.
func (c *Client) call(ctx context.Context, sub string, fields []field, bundle func() (io.ReadCloser, int64, error), extra int, out any) error {
	start := c.clock.Now()
	backoff := firstBackoff
	for {
		att, err := c.attempt(ctx, sub, fields, bundle, extra)
		if err != nil {
			return err
		}
		raw, execErr := att.out, att.execErr
		rep := parseReply(raw.buf.Bytes())
		// Once the runner has spoken, a submit may have taken effect: a reply
		// that cannot be read is then "outcome unknown", never a blind retry.
		spoke := rep.hello || hasMarkers(raw.buf.Bytes())
		protocolErr := func(msg string) error {
			err := fmt.Errorf("runner %s: %s: %w", sub, msg, ErrProtocol)
			if sub == CmdSubmit && spoke {
				err = fmt.Errorf("%w: %w", err, ErrOutcomeUnknown)
			}
			return err
		}
		switch {
		case rep.complete:
			err := decode(sub, rep, out)
			if sub == CmdSubmit && errors.Is(err, ErrProtocol) {
				err = fmt.Errorf("%w: %w", err, ErrOutcomeUnknown)
			}
			return err
		case raw.over:
			return protocolErr("the reply is larger than expected")
		case rep.bad:
			return protocolErr("unexpected lines after BEGIN")
		case spoke:
			return fmt.Errorf("runner %s: the connection ended before the reply (%s): %w",
				sub, errText(execErr, "no transport error"), ErrOutcomeUnknown)
		case host.IsPermanent(execErr):
			// Retrying cannot help (gh missing, expired login, deleted host).
			return fmt.Errorf("runner %s: %w", sub, execErr)
		}

		// The remote script never started: nothing happened there.
		cause := execErr
		if cause == nil {
			cause = fmt.Errorf("the connection ended without output%s", noiseTail(raw.buf.Bytes()))
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("runner %s: %w: %w", sub, cerr, cause)
		}
		if c.clock.Now().Sub(start) >= c.StartRetry {
			return fmt.Errorf("runner %s: %w: %w", sub, ErrNotStarted, cause)
		}
		if err := c.clock.Sleep(ctx, backoff); err != nil {
			return fmt.Errorf("runner %s: %w: %w", sub, err, cause)
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// attempted is the outcome of one connection: what the transport wrote, and
// the transport's own error. What the runner printed decides what that error
// means, so both go back to the caller.
type attempted struct {
	out     *limitBuffer
	execErr error
}

// attempt makes one connection. Its error is for what stops the call at once
// (a bundle that cannot be opened).
func (c *Client) attempt(ctx context.Context, sub string, fields []field, bundle func() (io.ReadCloser, int64, error), extra int) (attempted, error) {
	fs := fields
	var payload io.Reader
	if bundle != nil {
		rc, size, err := bundle()
		if err != nil {
			return attempted{}, fmt.Errorf("runner %s: open the bundle: %w", sub, err)
		}
		defer func() { _ = rc.Close() }()
		if size < 0 {
			return attempted{}, fmt.Errorf("runner %s: the bundle has a negative size", sub)
		}
		fs = append(append([]field(nil), fields...), field{"bundle_len", strconv.FormatInt(size, 10)})
		payload = io.LimitReader(rc, size)
	}
	hdr, err := header(sub, fs)
	if err != nil {
		return attempted{}, err
	}
	var stdin bytes.Buffer
	stdin.Write(Preamble(c.root))
	stdin.Write(script)
	stdin.Write(hdr)
	var in io.Reader = &stdin
	if payload != nil {
		in = io.MultiReader(&stdin, payload)
	}
	out := &limitBuffer{max: replySlack + extra}
	execErr := c.t.Exec(ctx, in, out)
	return attempted{out: out, execErr: execErr}, nil
}

// decode reads the JSON of a complete reply.
func decode(sub string, rep reply, out any) error {
	if rep.rc != 0 {
		var e ErrorResponse
		if err := json.Unmarshal(rep.payload, &e); err != nil || e.Error.Code == "" {
			return fmt.Errorf("runner %s: exit status %d with an unreadable reply: %w", sub, rep.rc, ErrProtocol)
		}
		return &RemoteError{Code: e.Error.Code, Message: e.Error.Message}
	}
	if err := json.Unmarshal(rep.payload, out); err != nil {
		return fmt.Errorf("runner %s: bad JSON in the reply: %v: %w", sub, err, ErrProtocol)
	}
	return nil
}

// hasMarkers reports whether the output carries a BEGIN or END marker: the
// runner answered, even if its HELLO line could not be recognised.
func hasMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(Marker+" BEGIN\n")) || bytes.Contains(b, []byte(Marker+" END "))
}

func errText(err error, none string) string {
	if err == nil {
		return none
	}
	return err.Error()
}

// noiseTail quotes the end of what a connection printed before it ended.
func noiseTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return ": " + strconv.Quote(s)
}
