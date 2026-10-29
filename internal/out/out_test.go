package out

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		check   Check
		wantErr bool
	}{
		{"ok without fix", OK("gh", "gh 2.102.0"), false},
		{"skipped without fix", Skip("auth", "gh"), false},
		{"warning with fix", Warn("gate", CodeNoGate, "no gate", "gatehaul gate -- make test"), false},
		{"error with fix", Fail("scope", CodeScopeMissing, "no scope", "gh auth refresh -s codespace"), false},
		{"warning without fix", Warn("gate", CodeNoGate, "no gate", ""), true},
		{"error with blank fix", Fail("scope", CodeScopeMissing, "no scope", "   "), true},
		{"error without code", Check{Name: "x", Level: LevelError, Message: "m", Fix: "f"}, true},
		{"unknown level", Check{Name: "x", Level: "fatal", Message: "m"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.check.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() = %v, want error: %v", err, c.wantErr)
			}
		})
	}
}

func TestWriteErrorText(t *testing.T) {
	var b bytes.Buffer
	WriteError(&b, false, &Error{Code: CodeUsage, Message: "unknown command \"x\"", Fix: "gatehaul help"})
	got := b.String()
	if !strings.Contains(got, "error: unknown command") || !strings.HasSuffix(got, "Fix: gatehaul help\n") {
		t.Fatalf("unexpected text output:\n%s", got)
	}
}

func TestWriteErrorJSON(t *testing.T) {
	var b bytes.Buffer
	WriteError(&b, true, &Error{Code: CodeScopeMissing, Message: "no scope", Fix: "gh auth refresh -s codespace"})
	var got struct {
		Error bool   `json:"error"`
		Code  string `json:"code"`
		Fix   string `json:"fix"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", b.String(), err)
	}
	if !got.Error || got.Code != string(CodeScopeMissing) || got.Fix == "" {
		t.Fatalf("unexpected JSON: %+v", got)
	}
}
