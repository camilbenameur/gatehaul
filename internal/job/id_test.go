package job

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/runner"
)

func zeros() *bytes.Reader { return bytes.NewReader(make([]byte, 10)) }

func TestNewIDFormat(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	id, err := NewID(2, now, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !runner.IDPattern.MatchString(id) {
		t.Fatalf("id %q does not match runner.IDPattern", id)
	}
	if !strings.HasPrefix(id, "2-") || len(id) != 2+26 {
		t.Fatalf("id %q: want 2-<26 characters>", id)
	}
}

func TestNewIDVectors(t *testing.T) {
	id, err := NewID(1, time.UnixMilli(0), zeros())
	if err != nil || id != "1-"+strings.Repeat("0", 26) {
		t.Fatalf("zero id = %q, %v", id, err)
	}
	last := time.UnixMilli(1<<48 - 1)
	id, err = NewID(1, last, bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))
	if err != nil || id != "1-7"+strings.Repeat("Z", 25) {
		t.Fatalf("max id = %q, %v", id, err)
	}
	// One millisecond is the lowest bit of the 48-bit time, so it shows in
	// character 10 (the timestamp occupies the first 10 characters).
	id, err = NewID(1, time.UnixMilli(1), zeros())
	if err != nil || id != "1-0000000001"+strings.Repeat("0", 16) {
		t.Fatalf("1 ms id = %q, %v", id, err)
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prev := ""
	for i := 0; i < 50; i++ {
		id, err := NewID(1, base.Add(time.Duration(i)*time.Millisecond*37), zeros())
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("id %q does not sort after %q", id, prev)
		}
		prev = id
	}
}

func TestNewIDErrors(t *testing.T) {
	now := time.UnixMilli(1)
	for _, host := range []int{0, -1, 10000} {
		if _, err := NewID(host, now, zeros()); err == nil {
			t.Errorf("host %d: want an error", host)
		}
	}
	if _, err := NewID(1, now, bytes.NewReader([]byte{1, 2})); err == nil {
		t.Error("short entropy: want an error")
	}
	if _, err := NewID(1, time.UnixMilli(-5), zeros()); err == nil {
		t.Error("negative time: want an error")
	}
}

func TestHostNum(t *testing.T) {
	cases := []struct {
		id   string
		want int
		ok   bool
	}{
		{"2-01JB0000000000000000000000", 2, true},
		{"17-01JB0000000000000000000000", 17, true},
		{"0-01JB", 0, false},
		{"02-01JB", 0, false},
		{"x-01JB", 0, false},
		{"-01JB", 0, false},
		{"nodash", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := HostNum(c.id)
		if got != c.want || ok != c.ok {
			t.Errorf("HostNum(%q) = %d, %v; want %d, %v", c.id, got, ok, c.want, c.ok)
		}
	}
}

func TestBranchKeyPatternAndLength(t *testing.T) {
	refs := []string{
		"refs/heads/main", "refs/heads/a/b", "refs/heads/A", "refs/heads/é", "HEAD",
		"refs/heads/" + strings.Repeat("x", 100), "refs/heads/---", "refs/heads/feature/ÉTÉ-2026_final",
		"refs/tags/v1.0.0", "",
	}
	for _, ref := range refs {
		k := BranchKey(ref)
		if !runner.KeyPattern.MatchString(k) {
			t.Errorf("BranchKey(%q) = %q does not match runner.KeyPattern", ref, k)
		}
		if len(k) > 32+1+6 {
			t.Errorf("BranchKey(%q) = %q is longer than 39 characters", ref, k)
		}
	}
}

func TestBranchKeyDoesNotCollide(t *testing.T) {
	refs := []string{"refs/heads/a/b", "refs/heads/a-b", "refs/heads/A", "refs/heads/a", "refs/heads/é", "refs/heads/ü", "HEAD", "refs/tags/a"}
	seen := map[string]string{}
	for _, ref := range refs {
		k := BranchKey(ref)
		if other, dup := seen[k]; dup {
			t.Errorf("BranchKey(%q) and BranchKey(%q) are both %q", ref, other, k)
		}
		seen[k] = ref
	}
}

func TestBranchKeySlug(t *testing.T) {
	cases := []struct{ ref, prefix string }{
		{"refs/heads/main", "main-"},
		{"refs/heads/feature/Login Page!!", "feature-login-page-"},
		{"refs/heads/é", "ref-"},
		{"HEAD", "head-"},
		{"refs/heads/" + strings.Repeat("a", 40), strings.Repeat("a", 32) + "-"},
		{"refs/heads/" + strings.Repeat("a", 31) + "/b", strings.Repeat("a", 31) + "-"},
	}
	for _, c := range cases {
		if k := BranchKey(c.ref); !strings.HasPrefix(k, c.prefix) {
			t.Errorf("BranchKey(%q) = %q, want prefix %q", c.ref, k, c.prefix)
		}
	}
	if a, b := BranchKey("refs/heads/main"), BranchKey("refs/heads/main"); a != b {
		t.Errorf("BranchKey is not deterministic: %q then %q", a, b)
	}
}
