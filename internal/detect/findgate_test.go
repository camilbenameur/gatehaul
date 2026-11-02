package detect

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

func TestChanges(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git", "-C", "/w", "status"}, ghx.Stdout(" M main.go\r\n?? grep.exe.stackdump\nR  a.go -> b.go\n?? notes/\nA  new.go\n"), nil)
	got, err := Git{Runner: f, Dir: "/w"}.Changes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main.go", "a.go -> b.go", "new.go"}; !reflect.DeepEqual(got.Modified, want) {
		t.Fatalf("Modified = %q, want %q", got.Modified, want)
	}
	if want := []string{"grep.exe.stackdump", "notes/"}; !reflect.DeepEqual(got.Untracked, want) {
		t.Fatalf("Untracked = %q, want %q", got.Untracked, want)
	}
}

func TestChangesNotARepo(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"git", "-C", "/w", "status"}, ghx.Exit(128, "fatal: not a git repository"), nil)
	if _, err := (Git{Runner: f, Dir: "/w"}).Changes(context.Background()); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("err = %v, want ErrNotRepo", err)
	}
}

func TestFindGate(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	cases := []struct {
		name   string
		files  fstest.MapFS
		dir    string
		source string
		ok     bool
	}{
		{"root wins", fstest.MapFS{"go.mod": file("m"), "sub/Cargo.toml": file("")}, "", "go.mod", true},
		{"monorepo", fstest.MapFS{"README.md": file(""), "src/back-end/src/go.mod": file("m")}, "src/back-end/src", "go.mod", true},
		{"shallowest wins", fstest.MapFS{"a/b/c/go.mod": file("m"), "z/Cargo.toml": file("")}, "z", "Cargo.toml", true},
		{"lexical order at one depth", fstest.MapFS{"b/go.mod": file("m"), "a/Cargo.toml": file("")}, "a", "Cargo.toml", true},
		{"too deep", fstest.MapFS{"a/b/c/d/go.mod": file("m")}, "", "", false},
		{"skipped directories", fstest.MapFS{
			"node_modules/x/package.json": file(`{"scripts":{"test":"jest"}}`),
			"vendor/go.mod":               file("m"),
			".git/go.mod":                 file("m"),
			".hidden/go.mod":              file("m"),
			"dist/pyproject.toml":         file(""),
		}, "", "", false},
		{"package.json without a test script", fstest.MapFS{"web/package.json": file(`{"scripts":{"build":"x"}}`)}, "", "", false},
		{"nothing", fstest.MapFS{"README.md": file("")}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, dir, ok := FindGate(c.files, 3)
			if ok != c.ok || dir != c.dir || g.Source != c.source {
				t.Fatalf("FindGate = %+v, %q, %v; want source %q, dir %q, ok %v", g, dir, ok, c.source, c.dir, c.ok)
			}
		})
	}
}
