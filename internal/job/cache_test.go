package job

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/runner"
)

const (
	id1 = "1-01JB0000000000000000000001"
	id2 = "1-01JB0000000000000000000002"
	id3 = "2-01JC0000000000000000000003"
)

func dispatch(id, repo, sha string) Dispatch {
	return Dispatch{
		ID: id, Repo: repo, Host: "gatehaul-1", HostNum: 1, HostID: "hid", Ref: "refs/heads/main",
		Key: "main-abcdef", SHA: sha, Kind: runner.KindRun, Cmd: "true", Fingerprint: "fp",
		SubmittedAt: "2026-10-29T12:00:00Z",
	}
}

func TestDispatchRoundTrip(t *testing.T) {
	c := NewCache(t.TempDir())
	want := dispatch(id1, "o/r", strings.Repeat("a", 40))
	if err := c.SaveDispatch(want); err != nil {
		t.Fatal(err)
	}
	got, err := c.LoadDispatch(id1)
	if err != nil {
		t.Fatal(err)
	}
	want.Schema = CacheSchema
	if got != want {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if _, err := c.LoadDispatch(id2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: err = %v, want ErrNotFound", err)
	}
	entries, err := os.ReadDir(filepath.Join(c.Dir(), "jobs", id1))
	if err != nil || len(entries) != 1 {
		t.Fatalf("job dir should hold only dispatch.json (no temp files): %v, %v", entries, err)
	}
}

func TestSaveDispatchRejectsBadID(t *testing.T) {
	c := NewCache(t.TempDir())
	for _, id := range []string{"", "../x", "1-abc", "1-01jb0000000000000000000001"} {
		if err := c.SaveDispatch(Dispatch{ID: id}); err == nil {
			t.Errorf("SaveDispatch(%q): want an error", id)
		}
	}
}

func TestStatusTerminalIsNeverOverwrittenByNonTerminal(t *testing.T) {
	c := NewCache(t.TempDir())
	if _, ok, err := c.LoadStatus(id1); ok || err != nil {
		t.Fatalf("no status yet: ok=%v err=%v", ok, err)
	}
	save := func(state string) {
		t.Helper()
		if err := c.SaveStatus(runner.JobStatus{ID: id1, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		s, ok, err := c.LoadStatus(id1)
		if !ok || err != nil {
			t.Fatalf("LoadStatus: ok=%v err=%v", ok, err)
		}
		return s.State
	}
	save("queued")
	save("running")
	if state() != "running" {
		t.Fatalf("state = %s, want running", state())
	}
	save("succeeded")
	save("running") // a late answer
	save("queued")
	if state() != "succeeded" {
		t.Fatalf("state = %s, a terminal state was overwritten", state())
	}
	if !c.IsTerminal(id1) {
		t.Error("IsTerminal = false for a succeeded job")
	}
	save("lost") // terminal over terminal is allowed
	if state() != "lost" {
		t.Fatalf("state = %s, want lost", state())
	}
}

func TestResolve(t *testing.T) {
	c := NewCache(t.TempDir())
	sha := strings.Repeat("a", 40)
	for _, id := range []string{id1, id2, id3} {
		if err := c.SaveDispatch(dispatch(id, "o/r", sha)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ in, want string }{
		{id1, id1},
		{"2-01JC", id3},
		{"01jc", id3}, // case-insensitive, ULID part
		{"01JB0000000000000000000002", id2},
		{"2-", id3},
	}
	for _, c2 := range cases {
		got, err := c.Resolve(c2.in)
		if err != nil || got.ID != c2.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", c2.in, got.ID, err, c2.want)
		}
	}
	if _, err := c.Resolve("9-"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(9-): err = %v, want ErrNotFound", err)
	}
	if _, err := c.Resolve("  "); err == nil {
		t.Error("Resolve(blank): want an error")
	}
}

func TestResolveAmbiguous(t *testing.T) {
	c := NewCache(t.TempDir())
	for _, id := range []string{id1, id2} {
		if err := c.SaveDispatch(dispatch(id, "o/r", "s")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Resolve("01JB")
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want *AmbiguousError", err)
	}
	if len(amb.Candidates) != 2 || amb.Candidates[0] != id1 || amb.Candidates[1] != id2 {
		t.Fatalf("candidates = %v", amb.Candidates)
	}
	if !strings.Contains(err.Error(), id1) || !strings.Contains(err.Error(), id2) {
		t.Fatalf("message %q should list the candidates", err)
	}
}

func TestListIgnoresStrayDirectories(t *testing.T) {
	c := NewCache(t.TempDir())
	if all, err := c.List(); err != nil || len(all) != 0 {
		t.Fatalf("empty cache: %v, %v", all, err)
	}
	if err := c.SaveDispatch(dispatch(id1, "o/r", "s")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.Dir(), "jobs", "not-a-job"), 0o700); err != nil {
		t.Fatal(err)
	}
	all, err := c.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("List = %v, %v", all, err)
	}
}

func TestFindActive(t *testing.T) {
	c := NewCache(t.TempDir())
	sha, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if _, err := c.FindActive("o/r", sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty cache: err = %v, want ErrNotFound", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.SaveDispatch(dispatch(id1, "o/r", sha)))
	must(c.SaveDispatch(dispatch(id2, "o/r", other)))
	must(c.SaveDispatch(dispatch(id3, "x/y", sha)))

	got, err := c.FindActive("o/r", sha)
	if err != nil || got.ID != id1 {
		t.Fatalf("FindActive = %q, %v; want %q (no status counts as active)", got.ID, err, id1)
	}
	must(c.SaveStatus(runner.JobStatus{ID: id1, State: "running"}))
	if got, err = c.FindActive("o/r", sha); err != nil || got.ID != id1 {
		t.Fatalf("running job: %q, %v", got.ID, err)
	}
	must(c.SaveStatus(runner.JobStatus{ID: id1, State: "failed"}))
	if _, err = c.FindActive("o/r", sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished job: err = %v, want ErrNotFound", err)
	}

	last, err := c.FindLatest("o/r", sha)
	if err != nil || last.ID != id1 {
		t.Fatalf("FindLatest = %q, %v", last.ID, err)
	}
	if _, err = c.FindLatest("o/r", strings.Repeat("c", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindLatest unknown sha: err = %v", err)
	}

	// Two active jobs for the same commit are ambiguous.
	second := "1-01JB0000000000000000000009"
	must(c.SaveDispatch(dispatch(second, "o/r", sha)))
	must(c.SaveDispatch(dispatch("1-01JB0000000000000000000008", "o/r", sha)))
	var amb *AmbiguousError
	if _, err = c.FindActive("o/r", sha); !errors.As(err, &amb) || len(amb.Candidates) != 2 {
		t.Fatalf("two active jobs: err = %v, want *AmbiguousError with 2 candidates", err)
	}
}

func TestDefaultCacheDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	userDir := func() (string, error) { return filepath.Join("home", "cache"), nil }
	failing := func() (string, error) { return "", errors.New("no home") }

	if d, err := DefaultCacheDir(env(map[string]string{"GATEHAUL_CACHE_DIR": "/custom"}), failing); err != nil || d != "/custom" {
		t.Errorf("override: %q, %v", d, err)
	}
	if d, err := DefaultCacheDir(env(nil), userDir); err != nil || d != filepath.Join("home", "cache", "gatehaul") {
		t.Errorf("default: %q, %v", d, err)
	}
	if _, err := DefaultCacheDir(env(nil), failing); err == nil {
		t.Error("no home: want an error")
	}
}

func TestPatchRoundTripIsAtomicAndBinarySafe(t *testing.T) {
	c := NewCache(t.TempDir())
	if _, ok, err := c.LoadPatch(id1); ok || err != nil {
		t.Fatalf("no patch yet: ok=%v err=%v", ok, err)
	}
	want := []byte("diff --git a/f b/f\n\x00\x01binary\r\n")
	if err := c.SavePatch(id1, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.LoadPatch(id1)
	if err != nil || !ok || string(got) != string(want) {
		t.Fatalf("LoadPatch = %q, %v, %v", got, ok, err)
	}
	// Saving again replaces the file and leaves no temporary file behind.
	if err := c.SavePatch(id1, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.LoadPatch(id1); string(got) != "second" {
		t.Fatalf("second save: %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(c.Dir(), "jobs", id1))
	if err != nil || len(entries) != 1 || entries[0].Name() != "patch" {
		t.Fatalf("job dir = %v, %v; want only patch", entries, err)
	}
	if err := c.SavePatch("../x", want); err == nil {
		t.Error("SavePatch with a bad id: want an error")
	}
	if _, ok, err := c.LoadPatch("../x"); ok || err != nil {
		t.Errorf("LoadPatch with a bad id: ok=%v err=%v", ok, err)
	}
}

func TestAppliedRoundTrip(t *testing.T) {
	c := NewCache(t.TempDir())
	if _, ok, err := c.LoadApplied(id1); ok || err != nil {
		t.Fatalf("not applied yet: ok=%v err=%v", ok, err)
	}
	want := Applied{SHA256: strings.Repeat("c", 64), Files: []string{"a.go", "b/c.txt"}, AppliedAt: "2026-10-29T12:00:00Z"}
	if err := c.SaveApplied(id1, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.LoadApplied(id1)
	if err != nil || !ok {
		t.Fatalf("LoadApplied: ok=%v err=%v", ok, err)
	}
	if got.Schema != CacheSchema || got.SHA256 != want.SHA256 || got.AppliedAt != want.AppliedAt || strings.Join(got.Files, ",") != "a.go,b/c.txt" {
		t.Fatalf("round trip: %+v", got)
	}
	if err := c.SaveApplied(id2, Applied{SHA256: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.LoadApplied(id2); got.Files == nil {
		t.Error("a nil file list must be stored as an empty list")
	}
	if err := c.SaveApplied("nope", want); err == nil {
		t.Error("SaveApplied with a bad id: want an error")
	}
}

func TestDispatchKeepsTheTreeAndTheSensitiveOptIn(t *testing.T) {
	c := NewCache(t.TempDir())
	want := dispatch(id1, "o/r", strings.Repeat("a", 40))
	want.Kind = runner.KindGen
	want.Top = "/work/repo"
	want.AllowSensitive = true
	if err := c.SaveDispatch(want); err != nil {
		t.Fatal(err)
	}
	got, err := c.LoadDispatch(id1)
	if err != nil || got.Top != "/work/repo" || !got.AllowSensitive {
		t.Fatalf("dispatch %+v, %v", got, err)
	}
	if dir := filepath.Join(c.Dir(), "jobs", id1); c.JobDir(id1) != dir {
		t.Fatalf("JobDir = %q, want %q", c.JobDir(id1), dir)
	}
}

func TestFindLatestGen(t *testing.T) {
	c := NewCache(t.TempDir())
	sha := strings.Repeat("a", 40)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	gen := func(id, repo, sha string) {
		d := dispatch(id, repo, sha)
		d.Kind = runner.KindGen
		must(c.SaveDispatch(d))
	}
	if _, err := c.FindLatestGen("o/r", sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty cache: err = %v", err)
	}
	gen(id1, "o/r", sha)
	must(c.SaveStatus(runner.JobStatus{ID: id1, State: "succeeded"}))
	must(c.SaveDispatch(dispatch(id2, "o/r", sha))) // a run job: never chosen
	must(c.SaveStatus(runner.JobStatus{ID: id2, State: "succeeded"}))
	gen("1-01JB0000000000000000000004", "o/r", sha) // no status yet
	gen("1-01JB0000000000000000000005", "o/r", sha)
	must(c.SaveStatus(runner.JobStatus{ID: "1-01JB0000000000000000000005", State: "failed"}))
	gen("1-01JB0000000000000000000006", "x/y", sha) // another repository
	must(c.SaveStatus(runner.JobStatus{ID: "1-01JB0000000000000000000006", State: "succeeded"}))

	// Whatever its state: the newest gen job of the repository and commit.
	failed := "1-01JB0000000000000000000005"
	got, err := c.FindLatestGen("o/r", sha)
	if err != nil || got.ID != failed {
		t.Fatalf("FindLatestGen = %q, %v; want the latest gen job %q", got.ID, err, failed)
	}
	later := "1-01JB0000000000000000000007"
	gen(later, "o/r", sha) // no status yet: still the latest
	if got, err = c.FindLatestGen("o/r", sha); err != nil || got.ID != later {
		t.Fatalf("FindLatestGen = %q, %v; want the later job %q", got.ID, err, later)
	}
	if _, err = c.FindLatestGen("o/r", strings.Repeat("b", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other commit: err = %v", err)
	}
}
