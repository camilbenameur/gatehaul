package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

func sha(c string) string { return strings.Repeat(c, 40) }

func TestHostHas(t *testing.T) {
	refs := map[string]string{"main-aaaaaa": sha("a"), "dev-bbbbbb": sha("b")}
	if !HostHas(refs, sha("b")) || HostHas(refs, sha("c")) || HostHas(nil, sha("a")) {
		t.Fatal("HostHas gave a wrong answer")
	}
}

func TestBases(t *testing.T) {
	// k3 duplicates k1, k4 is not an ancestor, k5 is not a commit id (never
	// asked about), k6 is the commit being sent, k7 is empty.
	refs := map[string]string{
		"k1": sha("c"),
		"k2": sha("a"),
		"k3": sha("c"),
		"k4": sha("d"),
		"k5": "not-a-sha",
		"k6": sha("f"),
		"k7": "",
	}
	asked := map[string]int{}
	isAncestor := func(b string) bool {
		asked[b]++
		return b != sha("d")
	}
	got := Bases(refs, sha("f"), isAncestor)
	want := []string{sha("a"), sha("c")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bases = %v, want %v", got, want)
	}
	if asked[sha("c")] != 1 {
		t.Errorf("a duplicate base was checked %d times, want once", asked[sha("c")])
	}
	if len(asked) != 3 {
		t.Errorf("isAncestor was asked about %d commits, want 3 (a, c, d)", len(asked))
	}
	if got := Bases(nil, sha("f"), isAncestor); len(got) != 0 {
		t.Errorf("Bases(nil) = %v, want none", got)
	}
}

func TestIsAncestor(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/r", "merge-base", "--is-ancestor", sha("a")}, ghx.Result{}, nil).
		On([]string{"git", "-C", "/r", "merge-base", "--is-ancestor", sha("b")}, ghx.Exit(1, ""), nil).
		On([]string{"git", "-C", "/r", "merge-base", "--is-ancestor", sha("c")}, ghx.Exit(128, "bad object"), nil)
	ctx := context.Background()
	if !IsAncestor(ctx, f, "/r", sha("a"), sha("f")) {
		t.Error("exit 0 should be true")
	}
	if IsAncestor(ctx, f, "/r", sha("b"), sha("f")) || IsAncestor(ctx, f, "/r", sha("c"), sha("f")) {
		t.Error("exit 1 and exit 128 should be false")
	}
	if IsAncestor(ctx, f, "/r", sha("d"), sha("f")) {
		t.Error("an unscripted command (error) should be false")
	}
}

func TestCreateIncrementalAndCleanup(t *testing.T) {
	ref := RefPrefix + "1-ID"
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/r", "update-ref"}, ghx.Result{}, nil).
		On([]string{"git", "-C", "/r", "bundle", "create"}, ghx.Result{}, nil)
	err := Create(context.Background(), f, "/r", "1-ID", sha("f"), []string{sha("a"), sha("c")}, "/tmp/b")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"git", "-C", "/r", "update-ref", ref, sha("f")},
		{"git", "-C", "/r", "bundle", "create", "/tmp/b", ref, "^" + sha("a"), "^" + sha("c")},
		{"git", "-C", "/r", "update-ref", "-d", ref},
	}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
}

func TestCreateFullBundleHasNoPrerequisites(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git"}, ghx.Result{}, nil)
	if err := Create(context.Background(), f, "/r", "1-ID", sha("f"), nil, "/tmp/b"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls() {
		for _, a := range c {
			if strings.HasPrefix(a, "^") {
				t.Fatalf("a full bundle excludes nothing, but got %v", c)
			}
		}
	}
}

func TestCreateDeletesTheRefWhenTheBundleFails(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"git", "-C", "/r", "bundle"}, ghx.Exit(128, "fatal: Refusing to create empty bundle."), nil).
		On([]string{"git", "-C", "/r", "update-ref"}, ghx.Result{}, nil)
	err := Create(context.Background(), f, "/r", "1-ID", sha("f"), nil, "/tmp/b")
	if err == nil || !strings.Contains(err.Error(), "empty bundle") {
		t.Fatalf("err = %v, want the git message", err)
	}
	calls := f.Calls()
	last := calls[len(calls)-1]
	if !reflect.DeepEqual(last, []string{"git", "-C", "/r", "update-ref", "-d", RefPrefix + "1-ID"}) {
		t.Fatalf("the temporary ref was not deleted; last call %v", last)
	}
}

func TestCreateDeletesTheRefWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelRunner{cancel: cancel}
	err := Create(ctx, r, "/r", "1-ID", sha("f"), nil, "/tmp/b")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !r.deleted {
		t.Fatal("the temporary ref was not deleted after the cancellation")
	}
}

// cancelRunner cancels the context when git bundle starts. It refuses to
// delete the ref under a cancelled context, as a real command would.
type cancelRunner struct {
	cancel  context.CancelFunc
	deleted bool
}

func (c *cancelRunner) Run(ctx context.Context, argv []string, _ io.Reader, _ time.Duration) (ghx.Result, error) {
	switch {
	case slices.Contains(argv, "bundle"):
		c.cancel()
		return ghx.Result{}, fmt.Errorf("git: %w", ctx.Err())
	case slices.Contains(argv, "-d"):
		if err := ctx.Err(); err != nil {
			return ghx.Result{}, err
		}
		c.deleted = true
	}
	return ghx.Result{}, nil
}
