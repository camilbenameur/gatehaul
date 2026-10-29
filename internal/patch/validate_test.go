package patch

import (
	"reflect"
	"slices"
	"testing"
)

func TestCheckPath(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{"src/main.go", nil},
		{"a/b.txt", nil},
		{"dir/.gitignore", nil},
		{".github/workflows/ci.yml", nil},
		{"../../.bashrc", []string{ReasonParentDir}},
		{"a/../b", []string{ReasonParentDir}},
		{"/etc/passwd", []string{ReasonAbsolute}},
		{"//server/share/x", []string{ReasonAbsolute, ReasonBadChar}},
		{"C:/Windows/x", []string{ReasonAbsolute, ReasonBadChar}},
		{"a\\b", []string{ReasonBadChar}},
		{"x/a:b", []string{ReasonBadChar}},
		{"a:b", []string{ReasonAbsolute, ReasonBadChar}},
		{"a\x00b", []string{ReasonBadChar}},
		{"a\nb", []string{ReasonBadChar}},
		{"a//b", []string{ReasonBadChar}},
		{"a/./b", []string{ReasonBadChar}},
		{"a/b/", []string{ReasonBadChar}},
		{"ci.yml.", []string{ReasonBadChar}},
		{"ci.yml ", []string{ReasonBadChar}},
		{"", []string{ReasonBadChar}},
		{".git/hooks/x", []string{ReasonDotGit}},
		{".GIT/config", []string{ReasonDotGit}},
		{"sub/.Git/config", []string{ReasonDotGit}},
		{"GIT~1/config", []string{ReasonDotGit, ReasonShortName}},
		{".git./config", []string{ReasonBadChar, ReasonDotGit}},
		{".git::$INDEX_ALLOCATION/x", []string{ReasonBadChar, ReasonDotGit}},
		{".g\u200cit/config", []string{ReasonDotGit}},
		{".github/WORKFL~1/x.yml", []string{ReasonShortName}},
		{"GATEHA~1.TOM", []string{ReasonShortName}},
		{"gatehaul.toml/../PROGRA~1", []string{ReasonParentDir, ReasonShortName}},
		{"a/ab~12.c", []string{ReasonShortName}},
		{"CLAUDE~1/settings.json", []string{ReasonShortName}},
		{"notes~1.backup", nil},
		{"verylongname~1", nil},
		{"a~b", nil},
		{"x~1.tar.gz", nil},
		{"~1", nil},
		{"a\xffb", []string{ReasonBadChar}},
	}
	for _, tt := range tests {
		got := checkPath(tt.path)
		slices.Sort(got)
		want := slices.Clone(tt.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("checkPath(%q) = %v, want %v", tt.path, got, want)
		}
	}
}

func TestSensitivity(t *testing.T) {
	tests := []struct {
		path      string
		sensitive bool
		refused   bool
	}{
		{"main.go", false, false},
		{"docs/guide.md", false, false},
		{"gatehaul.toml", true, true},
		{"GateHaul.TOML", true, true},
		{".github/workflows/ci.yml", true, true},
		{".GitHub/Workflows/ci.yml", true, true},
		{".github/CODEOWNERS", false, false},
		{".githooks/pre-commit", true, true},
		{".husky/pre-push", true, true},
		{".claude/settings.json", true, false},
		{".codex/config.toml", true, false},
		{"CLAUDE.md", true, false},
		{"sub/agents.md", true, false},
		{"Makefile", true, false},
		{"pkg/MAKEFILE", true, false},
		{"package.json", true, false},
		{"scripts/x.sh", true, false},
		{"scripts/X.SH", true, false},
		{".envrc", true, false},
		{".gitattributes", true, false},
		{".gitmodules", true, false},
		{"notes/package.json.bak", false, false},
		{"AGENT\u017f.md", true, false},
		{"sub/agent\u017f.MD", true, false},
		{"scripts/x.\u017fh", true, false},
		{"gatehaul\u200c.toml", true, true},
		{".github/work\u200dflows/ci.yml", true, true},
	}
	for _, tt := range tests {
		sensitive, refused := sensitivity(tt.path)
		if sensitive != tt.sensitive || refused != tt.refused {
			t.Errorf("sensitivity(%q) = %v, %v; want %v, %v", tt.path, sensitive, refused, tt.sensitive, tt.refused)
		}
	}
}

func TestListingPlan(t *testing.T) {
	got := listingPlan([]string{"top.txt", "a/b/c.txt", "a/d.txt", "z/y.txt", "a/b/e.txt"})
	want := [][]string{{"a", "z"}, {"a/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listingPlan = %v, want %v", got, want)
	}
	if got := listingPlan([]string{"top.txt"}); len(got) != 0 {
		t.Fatalf("a file at the top needs no other listing, got %v", got)
	}
}

func TestCaseCollisions(t *testing.T) {
	all := []string{"README.md", "readme.md", "src/A.go", "Src/b.go", "docs/x.md", "other.txt"}
	got := caseCollisions(all, []string{"readme.md", "Src/b.go", "other.txt"})
	want := []string{"readme.md", "Src/b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("caseCollisions = %v, want %v", got, want)
	}
	// The Kelvin sign folds to "k".
	if got := caseCollisions([]string{"k.txt", "\u212a.txt"}, []string{"\u212a.txt"}); len(got) != 1 {
		t.Fatalf("the Kelvin sign must collide with k, got %v", got)
	}
	if got := caseCollisions([]string{"a.txt", "b.txt"}, []string{"b.txt"}); len(got) != 0 {
		t.Fatalf("unexpected collision: %v", got)
	}
}

func TestDirFileConflicts(t *testing.T) {
	files := []File{
		{Path: "data", Status: StatusDeleted},
		{Path: "data/x.txt", Status: StatusAdded},
		{Path: "pkg/a.txt", Status: StatusDeleted},
		{Path: "pkg", Status: StatusAdded},
		{Path: "plain.txt", Status: StatusAdded},
		{Path: "other/b.txt", Status: StatusModified},
	}
	got := dirFileConflicts(files)
	want := []conflict{{path: "data/x.txt", other: "data"}, {path: "pkg", other: "pkg/a.txt"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dirFileConflicts = %v, want %v", got, want)
	}
}

func TestParseDiffTree(t *testing.T) {
	a := "1111111111111111111111111111111111111111"
	b := "2222222222222222222222222222222222222222"
	zero := "0000000000000000000000000000000000000000"
	out := ":000000 100644 " + zero + " " + a + " A\x00new file.txt\x00" +
		":100644 100755 " + a + " " + a + " M\x00bin/run\x00" +
		":100644 000000 " + b + " " + zero + " D\x00gone\x00"
	got, err := parseDiffTree([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].path != "new file.txt" || got[0].oldMode != modeNone || got[1].newMode != modeExec || got[2].path != "gone" {
		t.Fatalf("parseDiffTree = %+v", got)
	}
	for _, bad := range []string{
		"garbage\x00x\x00",
		":100644 100644 " + a + " " + b + " M\x00",
		":100644 100644 abc def M\x00x\x00",
		":100644 100644 " + a + " " + b + "\x00x\x00",
	} {
		if _, err := parseDiffTree([]byte(bad)); err == nil {
			t.Errorf("parseDiffTree(%q) must fail", bad)
		}
	}
	if got, err := parseDiffTree(nil); err != nil || len(got) != 0 {
		t.Fatalf("parseDiffTree(nil) = %v, %v", got, err)
	}
}

func TestConflictPaths(t *testing.T) {
	stderr := "error: patch failed: a.txt:1\n" +
		"error: a.txt: patch does not apply\n" +
		"error: new.txt: already exists in working directory\r\n" +
		"error: gone.txt: No such file or directory\n" +
		"error: while searching for:\n" +
		"warning: something\n"
	got := conflictPaths([]byte(stderr))
	want := []string{"a.txt", "new.txt", "gone.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conflictPaths = %v, want %v", got, want)
	}
	if got := conflictPaths([]byte("fatal: corrupt patch at line 3\n")); len(got) != 0 {
		t.Fatalf("unexpected paths: %v", got)
	}
}

func TestApplyErrorMessage(t *testing.T) {
	if got := (&ApplyError{Paths: []string{"a", "b"}}).Error(); got != "the patch does not apply to the working tree: a, b" {
		t.Errorf("with paths: %q", got)
	}
	if got := (&ApplyError{Detail: "corrupt patch"}).Error(); got != "the patch does not apply to the working tree: corrupt patch" {
		t.Errorf("with detail: %q", got)
	}
	if got := (&ApplyError{}).Error(); got != "the patch does not apply to the working tree" {
		t.Errorf("bare: %q", got)
	}
}

func TestOptionsDefaultsAndReportOK(t *testing.T) {
	o := Options{}.withDefaults()
	if o.MaxFiles != 2000 || o.MaxFileBytes != 16<<20 || o.MaxTotalBytes != 64<<20 {
		t.Fatalf("defaults = %+v", o)
	}
	o = Options{MaxFiles: 3, MaxFileBytes: 4, MaxTotalBytes: 5}.withDefaults()
	if o.MaxFiles != 3 || o.MaxFileBytes != 4 || o.MaxTotalBytes != 5 {
		t.Fatalf("explicit values changed: %+v", o)
	}
	if !(Report{}).OK() || (Report{Problems: []Problem{{Reason: ReasonSymlink}}}).OK() {
		t.Fatal("Report.OK must follow the problem list")
	}
}

func TestIsHexAndOneLine(t *testing.T) {
	if !isHex("0123456789abcdef0123456789abcdef01234567") || isHex("--upload-pack=x") || isHex("ABCDEF0123456789abcdef0123456789abcdef01") {
		t.Fatal("isHex must accept lower-case 40 or 64 digit ids only")
	}
	if got := oneLine([]byte("a\n\tb \x1b[31mred\x00")); got != "a b ?[31mred?" {
		t.Fatalf("oneLine = %q", got)
	}
}
