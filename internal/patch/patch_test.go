package patch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

// These tests use real git, as every CI system has it. Hostile trees that
// cannot exist on a Windows file system (symlinks, case collisions) are built
// in a scratch index only, and never in a working tree.

func runGit(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	full := []string{
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"-c", "core.ignorecase=false",
		"-c", "core.hooksPath=",
		"-c", "commit.gpgsign=false",
		"-c", "user.name=t",
		"-c", "user.email=t@example.invalid",
	}
	if dir != "" {
		full = append([]string{"-C", dir}, full...)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	// Windows may have written CRLF if the user's settings say so.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func sum256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fixture is a repository with one base commit. It is also the laptop's
// working tree.
type fixture struct {
	t    *testing.T
	repo string
	base string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", nil, "", "init", "-q", repo)
	writeFile(t, repo, "a.txt", "one\ntwo\nthree\nfour\nfive\n")
	writeFile(t, repo, "gone.txt", "bye\n")
	writeFile(t, repo, "README.md", "# readme\n")
	writeFile(t, repo, "data", "a plain file\n")
	writeFile(t, repo, "pkg/a.txt", "a file in a directory\n")
	runGit(t, repo, nil, "", "add", "-A")
	runGit(t, repo, nil, "", "commit", "-q", "-m", "base")
	base := strings.TrimSpace(runGit(t, repo, nil, "", "rev-parse", "HEAD"))
	return &fixture{t: t, repo: repo, base: base}
}

// edit makes changes in a second clone and returns the patch that the runner
// would produce for them.
func (f *fixture) edit(change func(work string)) []byte {
	f.t.Helper()
	work := filepath.Join(f.t.TempDir(), "work")
	runGit(f.t, "", nil, "", "clone", "-q", f.repo, work)
	change(work)
	runGit(f.t, work, nil, "", "add", "-A", "-N")
	diff := runGit(f.t, work, nil, "",
		"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD")
	return []byte(diff)
}

type cacheEntry struct {
	path    string
	mode    string
	content string
	blob    string
}

// indexPatch builds a patch from entries put in a scratch index, without a
// working tree. blob, when set, is an existing object used instead of content.
func (f *fixture) indexPatch(entries ...cacheEntry) []byte {
	f.t.Helper()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(f.t.TempDir(), "index")}
	runGit(f.t, f.repo, env, "", "read-tree", f.base)
	for _, e := range entries {
		blob := e.blob
		if blob == "" {
			blob = strings.TrimSpace(runGit(f.t, f.repo, nil, e.content, "hash-object", "-w", "--stdin"))
		}
		runGit(f.t, f.repo, env, "", "update-index", "--add", "--cacheinfo", e.mode+","+blob+","+e.path)
	}
	diff := runGit(f.t, f.repo, env, "", "diff", "--cached", "--binary", "--full-index", "--no-renames", "HEAD")
	return []byte(diff)
}

func (f *fixture) blobOf(path string) string {
	f.t.Helper()
	return strings.TrimSpace(runGit(f.t, f.repo, nil, "", "rev-parse", f.base+":"+path))
}

func (f *fixture) check(patch []byte, o Options) Report {
	f.t.Helper()
	rep, err := Check(context.Background(), ghx.ExecRunner{}, f.repo, f.base, patch, sum256(patch), o)
	if err != nil {
		f.t.Fatalf("Check: %v", err)
	}
	return rep
}

func (f *fixture) mustBeOK(patch []byte, o Options) Report {
	f.t.Helper()
	rep := f.check(patch, o)
	if !rep.OK() {
		f.t.Fatalf("want an OK report, got problems %+v", rep.Problems)
	}
	return rep
}

// handPatch is a patch for a new empty file, written by hand.
func handPatch(path string) []byte {
	return []byte("diff --git a/" + path + " b/" + path + "\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000..e69de29bb2d1d6434b8b29ae775ad8c2e48c5391\n")
}

func requireProblem(t *testing.T, rep Report, path string, reasons ...string) {
	t.Helper()
	for _, p := range rep.Problems {
		if p.Path == path && slices.Contains(reasons, p.Reason) {
			return
		}
	}
	t.Fatalf("no problem %v for path %q in %+v", reasons, path, rep.Problems)
}

// requireReason accepts a problem on any path, for patches that git refuses
// before the tree exists.
func requireReason(t *testing.T, rep Report, reasons ...string) {
	t.Helper()
	if rep.OK() {
		t.Fatalf("want a refused patch, got %+v", rep)
	}
	for _, p := range rep.Problems {
		if slices.Contains(reasons, p.Reason) {
			return
		}
	}
	t.Fatalf("no problem among %v in %+v", reasons, rep.Problems)
}

func findFile(t *testing.T, rep Report, path string) File {
	t.Helper()
	for _, f := range rep.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in %+v", path, rep.Files)
	return File{}
}

func TestOKEditNewFileAndDeletion(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
		writeFile(t, work, "dir/new.txt", "hello\n")
		if err := os.Remove(filepath.Join(work, "gone.txt")); err != nil {
			t.Fatal(err)
		}
	})
	rep := f.mustBeOK(patch, Options{})
	if rep.SHA256 != sum256(patch) {
		t.Errorf("SHA256 = %q, want the digest of the patch", rep.SHA256)
	}
	if len(rep.Files) != 3 || len(rep.Sensitive) != 0 {
		t.Fatalf("files = %+v, sensitive = %v", rep.Files, rep.Sensitive)
	}
	a := findFile(t, rep, "a.txt")
	if a.Status != "modified" || a.OldMode != "100644" || a.NewMode != "100644" || a.Binary || a.Size != int64(len("one\nTWO\nthree\nfour\nfive\n")) {
		t.Errorf("a.txt = %+v", a)
	}
	n := findFile(t, rep, "dir/new.txt")
	if n.Status != "added" || n.OldMode != "" || n.NewMode != "100644" || n.Size != 6 {
		t.Errorf("dir/new.txt = %+v", n)
	}
	g := findFile(t, rep, "gone.txt")
	if g.Status != "deleted" || g.OldMode != "100644" || g.NewMode != "" || g.Size != 0 {
		t.Errorf("gone.txt = %+v", g)
	}
}

func TestCheckLeavesTheRepositoryAlone(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
		writeFile(t, work, "new.txt", "new\n")
	})
	before := runGit(t, f.repo, nil, "", "ls-files", "-s") + runGit(t, f.repo, nil, "", "status", "--porcelain")
	f.mustBeOK(patch, Options{})
	after := runGit(t, f.repo, nil, "", "ls-files", "-s") + runGit(t, f.repo, nil, "", "status", "--porcelain")
	if before != after {
		t.Fatalf("the index or the work tree changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := readFile(t, f.repo, "a.txt"); got != "one\ntwo\nthree\nfour\nfive\n" {
		t.Fatalf("a.txt was modified: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt must not exist, stat error: %v", err)
	}
}

func TestCheckFromASubdirectory(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "new.txt", "new\n") })
	rep, err := Check(context.Background(), ghx.ExecRunner{}, filepath.Join(f.repo, "pkg"), f.base, patch, sum256(patch), Options{})
	if err != nil || !rep.OK() || len(rep.Files) != 1 || rep.Files[0].Path != "new.txt" {
		t.Fatalf("Check from a subdirectory = %+v, %v", rep, err)
	}
}

func TestEmptyPatch(t *testing.T) {
	f := newFixture(t)
	rep := f.mustBeOK(nil, Options{})
	if len(rep.Files) != 0 || len(rep.Sensitive) != 0 || rep.SHA256 != sum256(nil) {
		t.Fatalf("report = %+v", rep)
	}
}

func TestExecutableBitChange(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "a.txt", mode: "100755", blob: f.blobOf("a.txt")})
	rep := f.mustBeOK(patch, Options{})
	a := findFile(t, rep, "a.txt")
	if a.Status != "mode_changed" || a.OldMode != "100644" || a.NewMode != "100755" {
		t.Fatalf("a.txt = %+v", a)
	}
	if a.Size != int64(len("one\ntwo\nthree\nfour\nfive\n")) {
		t.Errorf("Size = %d, want the size of the blob", a.Size)
	}
}

func TestNewExecutableFile(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "tool", mode: "100755", content: "#!/bin/sh\n"})
	rep := f.mustBeOK(patch, Options{})
	if x := findFile(t, rep, "tool"); x.Status != "added" || x.NewMode != "100755" {
		t.Fatalf("tool = %+v", x)
	}
}

func TestModeOnlyChangeIgnoresTheFileSizeLimit(t *testing.T) {
	f := newFixture(t)
	// a.txt is 25 bytes, over the limit, but a mode change adds no blob.
	patch := f.indexPatch(cacheEntry{path: "a.txt", mode: "100755", blob: f.blobOf("a.txt")})
	rep := f.mustBeOK(patch, Options{MaxFileBytes: 5, MaxTotalBytes: 5})
	a := findFile(t, rep, "a.txt")
	if a.Status != "mode_changed" || a.Size != int64(len("one\ntwo\nthree\nfour\nfive\n")) {
		t.Fatalf("a.txt = %+v", a)
	}
	// A new blob of the same size is still refused.
	patch = f.indexPatch(cacheEntry{path: "new.txt", mode: "100644", content: "one\ntwo\nthree\nfour\nfive\n"})
	requireProblem(t, f.check(patch, Options{MaxFileBytes: 5}), "new.txt", ReasonTooLarge)
}

func TestShortNameIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{".github/WORKFL~1/x.yml", "GATEHA~1.TOM"} {
		patch := f.indexPatch(cacheEntry{path: path, mode: "100644", content: "on: push\n"})
		requireProblem(t, f.check(patch, Options{}), path, ReasonShortName)
	}
}

func TestCaseCollisionBelowSeveralDirectories(t *testing.T) {
	f := newFixture(t)
	// Both are new: the collision is between "Sub" and "sub", two levels down.
	patch := f.indexPatch(
		cacheEntry{path: "deep/Sub/x.txt", mode: "100644", content: "x\n"},
		cacheEntry{path: "deep/sub/y.txt", mode: "100644", content: "y\n"},
		cacheEntry{path: "deep/other/z.txt", mode: "100644", content: "z\n"},
	)
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "deep/Sub/x.txt", ReasonCaseCollision)
	requireProblem(t, rep, "deep/sub/y.txt", ReasonCaseCollision)
	for _, p := range rep.Problems {
		if p.Path == "deep/other/z.txt" {
			t.Errorf("deep/other/z.txt must not be a problem: %+v", p)
		}
	}

	// A new name next to an existing one, in a directory that exists.
	patch = f.indexPatch(cacheEntry{path: "pkg/A.TXT", mode: "100644", content: "x\n"})
	requireProblem(t, f.check(patch, Options{}), "pkg/A.TXT", ReasonCaseCollision)
}

func TestBinaryDeltaBombIsRefusedBeforeGitRuns(t *testing.T) {
	patch := []byte(deltaPatch(t, "a.txt", testHuge))
	fake := &ghx.Fake{}
	rep, err := Check(context.Background(), fake, "/w", "HEAD", patch, sum256(patch), Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	requireProblem(t, rep, "a.txt", ReasonTooLarge)
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("git must not run on such a patch, calls: %v", calls)
	}
	if len(rep.Files) != 0 {
		t.Errorf("no file can be reported, got %+v", rep.Files)
	}

	// The same patch against a real repository.
	f := newFixture(t)
	requireProblem(t, f.check(patch, Options{}), "a.txt", ReasonTooLarge)
	if got := readFile(t, f.repo, "a.txt"); got != "one\ntwo\nthree\nfour\nfive\n" {
		t.Errorf("a.txt = %q", got)
	}
}

func TestBinaryPatchesFromGitStillPass(t *testing.T) {
	f := newFixture(t)
	var blob []byte
	for i := 0; i < 125; i++ {
		sum := sha256.Sum256([]byte{byte(i)})
		blob = append(blob, sum[:]...)
	}
	blob[0] = 0
	writeFile(t, f.repo, "blob.bin", string(blob))
	runGit(t, f.repo, nil, "", "add", "-A")
	runGit(t, f.repo, nil, "", "commit", "-q", "-m", "binary")
	f.base = strings.TrimSpace(runGit(t, f.repo, nil, "", "rev-parse", "HEAD"))

	patch := f.edit(func(work string) {
		changed := slices.Clone(blob)
		changed[2000] ^= 0xff
		writeFile(t, work, "blob.bin", string(changed))
		writeFile(t, work, "new.bin", "\x00\x01\x02 a new binary file")
	})
	if !strings.Contains(string(patch), "\ndelta ") || !strings.Contains(string(patch), "\nliteral ") {
		t.Fatalf("want a delta and a literal in the patch:\n%s", patch)
	}
	rep := f.mustBeOK(patch, Options{})
	if b := findFile(t, rep, "blob.bin"); !b.Binary || b.Status != "modified" || b.Size != int64(len(blob)) {
		t.Errorf("blob.bin = %+v", b)
	}
	if n := findFile(t, rep, "new.bin"); !n.Binary || n.Status != "added" {
		t.Errorf("new.bin = %+v", n)
	}

	// The delta builds 4000 bytes: over a limit of 100, refused from its header.
	rep = f.check(patch, Options{MaxFileBytes: 100})
	requireProblem(t, rep, "blob.bin", ReasonTooLarge)
}

func TestParentDirectoryPath(t *testing.T) {
	f := newFixture(t)
	rep := f.check(handPatch("../../.bashrc"), Options{})
	requireReason(t, rep, ReasonUnparsable, ReasonParentDir)
}

func TestAbsolutePath(t *testing.T) {
	f := newFixture(t)
	rep := f.check(handPatch("/etc/passwd"), Options{})
	requireReason(t, rep, ReasonUnparsable, ReasonAbsolute)
}

func TestGarbageIsUnparsable(t *testing.T) {
	f := newFixture(t)
	rep := f.check([]byte("this is not a patch\n"), Options{})
	requireProblem(t, rep, "", ReasonUnparsable)
}

func TestSymlink(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "link", mode: "120000", content: "../../etc/passwd"})
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "link", ReasonSymlink)
}

func TestSubmodule(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "sub", mode: "160000", blob: f.base})
	rep := f.check(patch, Options{})
	// Git may refuse a submodule patch before the tree exists.
	requireReason(t, rep, ReasonSubmodule, ReasonUnparsable)
}

func TestDotGitPaths(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{".git/hooks/x", ".GIT/config"} {
		rep := f.check(handPatch(path), Options{})
		// Git itself refuses these names in an index, which is fine.
		requireReason(t, rep, ReasonDotGit, ReasonUnparsable)
	}
}

func TestCaseCollision(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "readme.md", mode: "100644", content: "another\n"})
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "readme.md", ReasonCaseCollision)
}

func TestCaseCollisionInADirectoryName(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(cacheEntry{path: "PKG/b.txt", mode: "100644", content: "x\n"})
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "PKG/b.txt", ReasonCaseCollision)
}

func TestFileBecomesDirectory(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		if err := os.Remove(filepath.Join(work, "data")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, work, "data/x.txt", "now in a directory\n")
	})
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "data/x.txt", ReasonFileDirConflict)
	requireProblem(t, rep, "data", ReasonFileDirConflict)
}

func TestLFSPointer(t *testing.T) {
	f := newFixture(t)
	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12345\n"
	patch := f.edit(func(work string) { writeFile(t, work, "model.bin", pointer) })
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "model.bin", ReasonLFSPointer)
}

func TestEmptyNewFile(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "empty.txt", "") })
	rep := f.mustBeOK(patch, Options{})
	e := findFile(t, rep, "empty.txt")
	if e.Status != "added" || e.Size != 0 || e.Binary {
		t.Fatalf("empty.txt = %+v", e)
	}
}

func TestBinaryFile(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "image.dat", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x01\x02")
		writeFile(t, work, "text.txt", "just text\n")
	})
	rep := f.mustBeOK(patch, Options{})
	if b := findFile(t, rep, "image.dat"); !b.Binary || b.Status != "added" || b.Size == 0 {
		t.Errorf("image.dat = %+v", b)
	}
	if x := findFile(t, rep, "text.txt"); x.Binary {
		t.Errorf("text.txt = %+v", x)
	}
}

func TestFileOverTheSizeLimit(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "big.txt", strings.Repeat("x", 100)+"\n")
		writeFile(t, work, "small.txt", "ok\n")
	})
	rep := f.check(patch, Options{MaxFileBytes: 10})
	requireProblem(t, rep, "big.txt", ReasonTooLarge)
	for _, p := range rep.Problems {
		if p.Path == "small.txt" {
			t.Errorf("small.txt must not be a problem: %+v", p)
		}
	}
}

func TestTotalSizeLimit(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "one.txt", strings.Repeat("x", 40)+"\n")
		writeFile(t, work, "two.txt", strings.Repeat("y", 40)+"\n")
	})
	rep := f.check(patch, Options{MaxTotalBytes: 50})
	requireProblem(t, rep, "", ReasonTooLarge)
}

func TestTooManyFiles(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		for _, name := range []string{"1.txt", "2.txt", "3.txt"} {
			writeFile(t, work, name, name+"\n")
		}
	})
	rep := f.check(patch, Options{MaxFiles: 2})
	requireProblem(t, rep, "", ReasonTooManyFiles)
}

func TestEveryProblemIsListedAndSorted(t *testing.T) {
	f := newFixture(t)
	patch := f.indexPatch(
		cacheEntry{path: "link", mode: "120000", content: "x"},
		cacheEntry{path: "readme.md", mode: "100644", content: "x\n"},
		cacheEntry{path: "gatehaul.toml", mode: "100644", content: "x\n"},
	)
	rep := f.check(patch, Options{})
	requireProblem(t, rep, "link", ReasonSymlink)
	requireProblem(t, rep, "readme.md", ReasonCaseCollision)
	requireProblem(t, rep, "gatehaul.toml", ReasonSensitive)
	paths := make([]string, len(rep.Problems))
	for i, p := range rep.Problems {
		paths[i] = p.Path
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("problems are not sorted by path: %v", paths)
	}
}

func TestSensitiveRefusedUnlessAllowed(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "gatehaul.toml", "x = 1\n") })

	rep := f.check(patch, Options{})
	requireProblem(t, rep, "gatehaul.toml", ReasonSensitive)
	if !slices.Equal(rep.Sensitive, []string{"gatehaul.toml"}) {
		t.Errorf("Sensitive = %v", rep.Sensitive)
	}

	rep = f.mustBeOK(patch, Options{AllowSensitive: true})
	if !slices.Equal(rep.Sensitive, []string{"gatehaul.toml"}) || !findFile(t, rep, "gatehaul.toml").Sensitive {
		t.Errorf("an allowed sensitive path must still be listed: %+v", rep)
	}
}

func TestSensitiveWorkflowIsRefused(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, ".github/workflows/ci.yml", "on: push\n") })
	rep := f.check(patch, Options{})
	requireProblem(t, rep, ".github/workflows/ci.yml", ReasonSensitive)
}

func TestSensitiveScriptIsAppliedButListed(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "scripts/x.sh", "#!/bin/sh\necho hi\n") })
	rep := f.mustBeOK(patch, Options{})
	if !slices.Equal(rep.Sensitive, []string{"scripts/x.sh"}) || !findFile(t, rep, "scripts/x.sh").Sensitive {
		t.Fatalf("report = %+v", rep)
	}
}

func TestSHAMismatchIsAnError(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "new.txt", "new\n") })
	_, err := Check(context.Background(), ghx.ExecRunner{}, f.repo, f.base, patch, strings.Repeat("0", 64), Options{})
	if !errors.Is(err, ErrSHAMismatch) {
		t.Fatalf("err = %v, want ErrSHAMismatch", err)
	}
	// An upper-case digest is the same digest.
	if _, err := Check(context.Background(), ghx.ExecRunner{}, f.repo, f.base, patch, strings.ToUpper(sum256(patch)), Options{}); err != nil {
		t.Fatalf("upper-case digest: %v", err)
	}
}

func TestUnknownBaseIsAnError(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "new.txt", "new\n") })
	_, err := Check(context.Background(), ghx.ExecRunner{}, f.repo, strings.Repeat("1", 40), patch, sum256(patch), Options{})
	if err == nil {
		t.Fatal("an unknown base commit must be an error")
	}
	if _, err := Check(context.Background(), ghx.ExecRunner{}, f.repo, "--upload-pack=x", patch, sum256(patch), Options{}); err == nil {
		t.Fatal("a base that looks like an option must be an error")
	}
}

// plainRunner is a Runner that cannot set an environment.
type plainRunner struct{ r ghx.Runner }

func (p plainRunner) Run(ctx context.Context, argv []string, stdin io.Reader, timeout time.Duration) (ghx.Result, error) {
	return p.r.Run(ctx, argv, stdin, timeout)
}

func TestCheckNeedsAnEnvRunner(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "new.txt", "new\n") })
	_, err := Check(context.Background(), plainRunner{ghx.ExecRunner{}}, f.repo, f.base, patch, sum256(patch), Options{})
	if err == nil {
		t.Fatal("Check must refuse a runner without environment support")
	}
	// An empty patch needs no git, so it passes with any runner.
	if rep, err := Check(context.Background(), plainRunner{ghx.ExecRunner{}}, f.repo, f.base, nil, sum256(nil), Options{}); err != nil || !rep.OK() {
		t.Fatalf("empty patch: %+v, %v", rep, err)
	}
}

func TestCheckPassesTheScratchIndexAndNoHooks(t *testing.T) {
	fake := (&ghx.Fake{}).
		On([]string{"git", "-C", "/w", "-c", "core.hooksPath=", "-c", "core.quotepath=false", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.ignorecase=false", "rev-parse", "--show-toplevel"}, ghx.Stdout("/w\n"), nil).
		On([]string{"git", "-C", "/w", "-c", "core.hooksPath=", "-c", "core.quotepath=false", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.ignorecase=false", "rev-parse", "--path-format=absolute"}, ghx.Stdout("/w/.git/objects\n"), nil).
		On([]string{"git"}, ghx.Exit(128, "fatal: stop here"), nil)
	patch := []byte("anything")
	_, err := Check(context.Background(), fake, "/w", "HEAD", patch, sum256(patch), Options{})
	if err == nil {
		t.Fatal("want the scripted failure")
	}
	calls := fake.EnvCalls()
	if len(calls) == 0 {
		t.Fatal("git must be called through RunEnv")
	}
	for _, c := range calls {
		if len(c.Env) == 0 || !strings.HasPrefix(c.Env[0], "GIT_INDEX_FILE=") {
			t.Errorf("env of %v = %v, want GIT_INDEX_FILE first", c.Argv, c.Env)
		}
		if !slices.Contains(c.Argv, "core.hooksPath=") || !slices.Contains(c.Argv, "core.autocrlf=false") {
			t.Errorf("argv %v must disable hooks and autocrlf", c.Argv)
		}
	}
}

func TestCheckLeavesNoObjectInTheRepository(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "new.txt", "a blob that the repository does not have yet\n")
		writeFile(t, work, "gatehaul.toml", "refused\n")
	})
	before := runGit(t, f.repo, nil, "", "count-objects", "-v")
	rep := f.check(patch, Options{})
	if rep.OK() {
		t.Fatal("gatehaul.toml must be refused without AllowSensitive")
	}
	if after := runGit(t, f.repo, nil, "", "count-objects", "-v"); after != before {
		t.Errorf("Check wrote objects into the repository:\nbefore %s\nafter %s", before, after)
	}
}

func TestApplyChangesTheWorkingTreeOnly(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) {
		writeFile(t, work, "a.txt", "one\nTWO\nthree\nfour\nfive\n")
		writeFile(t, work, "dir/new.txt", "hello\n")
		if err := os.Remove(filepath.Join(work, "gone.txt")); err != nil {
			t.Fatal(err)
		}
	})
	f.mustBeOK(patch, Options{})
	if err := Apply(context.Background(), ghx.ExecRunner{}, f.repo, patch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, f.repo, "a.txt"); got != "one\nTWO\nthree\nfour\nfive\n" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readFile(t, f.repo, "dir/new.txt"); got != "hello\n" {
		t.Errorf("dir/new.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt must be deleted, stat error: %v", err)
	}
	if staged := runGit(t, f.repo, nil, "", "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Errorf("Apply must not touch the index, staged: %q", staged)
	}
}

func TestApplyFromASubdirectory(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "top.txt", "at the top\n") })
	if err := Apply(context.Background(), ghx.ExecRunner{}, filepath.Join(f.repo, "pkg"), patch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, f.repo, "top.txt"); got != "at the top\n" {
		t.Errorf("top.txt = %q", got)
	}
}

func TestApplyReportsALocalConflict(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "a.txt", "one\nTWO\nthree\nfour\nfive\n") })
	f.mustBeOK(patch, Options{})
	writeFile(t, f.repo, "a.txt", "one\nlocal edit\nthree\nfour\nfive\n")

	err := Apply(context.Background(), ghx.ExecRunner{}, f.repo, patch)
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *ApplyError", err)
	}
	if !slices.Contains(ae.Paths, "a.txt") {
		t.Errorf("Paths = %v, want a.txt", ae.Paths)
	}
	if got := readFile(t, f.repo, "a.txt"); got != "one\nlocal edit\nthree\nfour\nfive\n" {
		t.Errorf("the local edit must be left alone, a.txt = %q", got)
	}
}

func TestApplyReportsAnExistingFile(t *testing.T) {
	f := newFixture(t)
	patch := f.edit(func(work string) { writeFile(t, work, "new.txt", "from the job\n") })
	writeFile(t, f.repo, "new.txt", "already here\n")

	err := Apply(context.Background(), ghx.ExecRunner{}, f.repo, patch)
	var ae *ApplyError
	if !errors.As(err, &ae) || !slices.Contains(ae.Paths, "new.txt") {
		t.Fatalf("err = %v, want an *ApplyError naming new.txt", err)
	}
}
