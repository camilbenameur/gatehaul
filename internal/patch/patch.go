// Package patch decides what may be applied to the working tree from the git
// patch that a remote job produced. The patch is hostile input: the sandbox
// that made it runs code written by an agent.
//
// The patch text is not inspected, except for the sizes that its binary
// hunks declare, which are read before git allocates them (binary.go). Check
// applies the patch to a temporary index that starts from the base commit,
// reads back the tree that git built, and validates the difference between
// the two trees (docs/SPEC.md §7). Whatever git refuses to put in a
// tree, such as a "../" path, is refused too.
package patch

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

const (
	defaultMaxFiles      = 2000
	defaultMaxFileBytes  = 16 << 20
	defaultMaxTotalBytes = 64 << 20
	binaryProbe          = 8000
	lfsPrefix            = "version https://git-lfs.github.com/spec/"
)

// File statuses.
const (
	StatusAdded       = "added"
	StatusModified    = "modified"
	StatusDeleted     = "deleted"
	StatusModeChanged = "mode_changed"
)

const (
	modeFile    = "100644"
	modeExec    = "100755"
	modeSymlink = "120000"
	modeGitlink = "160000"
	modeNone    = "000000"
)

// ErrSHAMismatch means the patch does not hash to the digest that the runner
// announced, so it was damaged or replaced on the way.
var ErrSHAMismatch = errors.New("patch: sha256 does not match the digest announced by the runner")

// Options sets the limits and the policy. The zero value is the default.
type Options struct {
	// AllowSensitive applies even the paths that are refused unless allowed.
	AllowSensitive bool
	// MaxFiles is the number of changed files; 0 means 2000.
	MaxFiles int
	// MaxFileBytes is the size of one new blob; 0 means 16 MiB.
	MaxFileBytes int64
	// MaxTotalBytes is the size of all new blobs together; 0 means 64 MiB.
	MaxTotalBytes int64
}

func (o Options) withDefaults() Options {
	if o.MaxFiles <= 0 {
		o.MaxFiles = defaultMaxFiles
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = defaultMaxFileBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = defaultMaxTotalBytes
	}
	return o
}

// File is one changed file of the patched tree. Status is "added",
// "modified", "deleted" or "mode_changed". OldMode is empty when the file is
// added and NewMode is empty when it is deleted. Size is the size of the new
// blob, 0 when deleted. Binary means a NUL byte in the first 8000 bytes.
type File struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	OldMode   string `json:"old_mode"`
	NewMode   string `json:"new_mode"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary"`
	Sensitive bool   `json:"sensitive"`
}

// Problem is one reason to refuse the patch. Reason is a stable snake_case
// code (the Reason constants). Path is empty when the problem is not about
// one file. Detail is free text for people; it never contains control
// characters. Path is the name as it appears in the patch and may contain
// anything that git allows in a name, so callers must escape it before
// printing it to a terminal.
type Problem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Report is the verdict on a patch.
type Report struct {
	SHA256    string    `json:"sha256"`
	Files     []File    `json:"files"`
	Sensitive []string  `json:"sensitive_paths"`
	Problems  []Problem `json:"problems"`
}

// OK reports whether the patch has no problem.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Check verifies that sha256(data) is wantSHA, then builds the patched tree in
// a temporary index and validates it. It never touches the working tree or the
// repository's own index; it does leave unreferenced objects in the object
// database, which git collects later. An error means Check could not run (git
// missing, I/O, wrong digest); a hostile patch is a Report with Problems. An
// empty patch is OK with no files.
//
// The runner must implement ghx.EnvRunner. dir may be any directory of the
// work tree and base any commit-ish that the laptop has.
func Check(ctx context.Context, r ghx.Runner, dir, base string, data []byte, wantSHA string, o Options) (Report, error) {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(wantSHA), got) {
		return Report{}, fmt.Errorf("%w (got %s, want %s)", ErrSHAMismatch, got, wantSHA)
	}
	rep := Report{SHA256: got, Files: []File{}, Sensitive: []string{}, Problems: []Problem{}}
	if len(data) == 0 {
		return rep, nil
	}
	if _, ok := r.(ghx.EnvRunner); !ok {
		return Report{}, errNoEnvRunner
	}
	if base == "" {
		return Report{}, errors.New("patch: no base commit")
	}
	o = o.withDefaults()

	// Git allocates what a binary hunk declares before any size limit could
	// be tested on the blobs it makes, so the declared sizes come first.
	if problems := scanBinary(data, o); len(problems) > 0 {
		rep.Problems = problems
		return rep, nil
	}

	top, err := topLevel(ctx, r, dir, false)
	if err != nil {
		return Report{}, err
	}
	tmp, err := os.MkdirTemp("", "gatehaul-patch-")
	if err != nil {
		return Report{}, fmt.Errorf("patch: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// The blobs and trees that "apply --cached" and "write-tree" create go to
	// a temporary object directory, with the repository's own objects only
	// read through the alternates: a refused patch leaves nothing behind.
	repoObjects, err := gitCmd{r: r, dir: top}.out(ctx, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return Report{}, err
	}
	objects := filepath.Join(tmp, "objects")
	for _, d := range []string{filepath.Join(objects, "pack"), filepath.Join(objects, "info")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Report{}, fmt.Errorf("patch: %w", err)
		}
	}
	g := gitCmd{
		r:   r,
		dir: top,
		env: []string{
			"GIT_INDEX_FILE=" + filepath.Join(tmp, "index"),
			"GIT_OBJECT_DIRECTORY=" + objects,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strings.TrimSpace(string(repoObjects)),
			"GIT_TERMINAL_PROMPT=0",
		},
	}
	// The base is resolved first, so that what follows only sees a hex id.
	out, err := g.out(ctx, nil, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return Report{}, err
	}
	baseSHA := strings.TrimSpace(string(out))
	if !isHex(baseSHA) {
		return Report{}, fmt.Errorf("patch: unexpected id %q for the base commit", oneLine([]byte(baseSHA)))
	}
	if _, err := g.out(ctx, nil, "read-tree", baseSHA); err != nil {
		return Report{}, err
	}

	res, err := g.run(ctx, bytes.NewReader(data), "apply", "--cached", "--binary", "--whitespace=nowarn", "-")
	if err != nil {
		return Report{}, fmt.Errorf("patch: git apply: %w", err)
	}
	if !res.OK() {
		rep.Problems = append(rep.Problems, Problem{Reason: ReasonUnparsable, Detail: oneLine(res.Stderr)})
		return rep, nil
	}
	out, err = g.out(ctx, nil, "write-tree")
	if err != nil {
		return Report{}, err
	}
	tree := strings.TrimSpace(string(out))
	if !isHex(tree) {
		return Report{}, fmt.Errorf("patch: unexpected tree id %q", oneLine([]byte(tree)))
	}
	out, err = g.outAll(ctx, nil, "diff-tree", "-r", "-z", "--raw", "--no-renames", "--full-index", "--no-abbrev", baseSHA, tree)
	if err != nil {
		return Report{}, err
	}
	entries, err := parseDiffTree(out)
	if err != nil {
		return Report{}, err
	}
	if err := validate(ctx, g, entries, tree, o, &rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}

func isHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// entry is one line of git diff-tree --raw.
type entry struct {
	path    string
	oldMode string
	newMode string
	oldSHA  string
	newSHA  string
}

// parseDiffTree reads the NUL-separated output of
// "git diff-tree -r -z --raw --no-renames --full-index": for each change,
// ":<old mode> <new mode> <old id> <new id> <status>" then the path.
func parseDiffTree(out []byte) ([]entry, error) {
	toks := strings.Split(string(out), "\x00")
	var entries []entry
	for i := 0; i < len(toks); {
		t := toks[i]
		if t == "" {
			i++
			continue
		}
		f := strings.Fields(strings.TrimPrefix(t, ":"))
		if t[0] != ':' || len(f) != 5 || i+1 >= len(toks) || toks[i+1] == "" {
			return nil, fmt.Errorf("patch: unexpected diff-tree output %q", oneLine([]byte(t)))
		}
		if len(f[2]) < 40 || len(f[3]) < 40 {
			return nil, fmt.Errorf("patch: diff-tree gave abbreviated ids in %q", oneLine([]byte(t)))
		}
		entries = append(entries, entry{
			path:    toks[i+1],
			oldMode: f[0],
			newMode: f[1],
			oldSHA:  f[2],
			newSHA:  f[3],
		})
		i += 2
	}
	return entries, nil
}

type collector struct {
	problems []Problem
}

func (c *collector) add(path, reason, detail string) {
	c.problems = append(c.problems, Problem{Path: path, Reason: reason, Detail: detail})
}

// sorted returns the problems ordered by path then reason, without
// duplicates.
func (c *collector) sorted() []Problem {
	ps := slices.Clone(c.problems)
	slices.SortFunc(ps, func(a, b Problem) int {
		if d := cmp.Compare(a.Path, b.Path); d != 0 {
			return d
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	ps = slices.CompactFunc(ps, func(a, b Problem) bool {
		return a.Path == b.Path && a.Reason == b.Reason
	})
	if ps == nil {
		ps = []Problem{}
	}
	return ps
}

func isRegular(mode string) bool { return mode == modeFile || mode == modeExec }

// validate fills rep from the changes between the base tree and the new tree.
// It records every problem it finds and returns an error only when git fails.
func validate(ctx context.Context, g gitCmd, entries []entry, tree string, o Options, rep *Report) error {
	var col collector
	if len(entries) > o.MaxFiles {
		col.add("", ReasonTooManyFiles, fmt.Sprintf("%d files changed, the limit is %d", len(entries), o.MaxFiles))
	}

	files := make([]File, len(entries))
	for i, e := range entries {
		f := File{Path: e.path, OldMode: e.oldMode, NewMode: e.newMode}
		if f.OldMode == modeNone {
			f.OldMode = ""
		}
		if f.NewMode == modeNone {
			f.NewMode = ""
		}
		switch {
		case f.OldMode == "":
			f.Status = StatusAdded
		case f.NewMode == "":
			f.Status = StatusDeleted
		case e.oldSHA == e.newSHA:
			f.Status = StatusModeChanged
		default:
			f.Status = StatusModified
		}

		for _, reason := range checkPath(e.path) {
			col.add(e.path, reason, "")
		}
		switch f.NewMode {
		case "", modeFile, modeExec:
			// A regular file, or a deletion.
		case modeSymlink:
			col.add(e.path, ReasonSymlink, "")
		case modeGitlink:
			col.add(e.path, ReasonSubmodule, "")
		default:
			col.add(e.path, ReasonBadMode, "mode "+oneLine([]byte(f.NewMode)))
		}
		if f.OldMode == modeGitlink && f.NewMode != modeGitlink {
			col.add(e.path, ReasonSubmodule, "replaces or removes a submodule")
		}

		if sensitive, refused := sensitivity(e.path); sensitive {
			f.Sensitive = true
			rep.Sensitive = append(rep.Sensitive, e.path)
			if refused && !o.AllowSensitive {
				col.add(e.path, ReasonSensitive, "")
			}
		}
		files[i] = f
	}

	// Reading blobs is the expensive part: skip it for a patch that is
	// refused anyway for its size.
	if len(entries) <= o.MaxFiles {
		if err := checkBlobs(ctx, g, entries, files, o, &col); err != nil {
			return err
		}
	}

	var added []string
	for _, f := range files {
		if f.Status == StatusAdded {
			added = append(added, f.Path)
		}
	}
	if len(added) > 0 {
		near, err := siblings(ctx, g, tree, added)
		if err != nil {
			return err
		}
		for _, p := range caseCollisions(near, added) {
			col.add(p, ReasonCaseCollision, "")
		}
	}
	for _, c := range dirFileConflicts(files) {
		col.add(c.path, ReasonFileDirConflict, "conflicts with "+oneLine([]byte(c.other)))
		col.add(c.other, ReasonFileDirConflict, "conflicts with "+oneLine([]byte(c.path)))
	}

	slices.Sort(rep.Sensitive)
	if rep.Sensitive == nil {
		rep.Sensitive = []string{}
	}
	rep.Files = files
	rep.Problems = col.sorted()
	return nil
}

type blobInfo struct {
	binary bool
	lfs    bool
}

// checkBlobs reads the size of every new blob and the start of the ones that
// are small enough, to fill Size and Binary and to find LFS pointers.
func checkBlobs(ctx context.Context, g gitCmd, entries []entry, files []File, o Options, col *collector) error {
	var shas []string
	seen := make(map[string]bool)
	for _, e := range entries {
		if isRegular(e.newMode) && !seen[e.newSHA] {
			seen[e.newSHA] = true
			shas = append(shas, e.newSHA)
		}
	}
	if len(shas) == 0 {
		return nil
	}
	sizes, err := blobSizes(ctx, g, shas)
	if err != nil {
		return err
	}

	var total int64
	probed := make(map[string]blobInfo)
	for i, e := range entries {
		if !isRegular(e.newMode) {
			continue
		}
		f := &files[i]
		size, ok := sizes[e.newSHA]
		if !ok {
			return fmt.Errorf("patch: git did not report the size of %s", e.newSHA)
		}
		f.Size = size
		// A mode change makes no new blob: it keeps the size of the old one,
		// which this patch does not add to the repository.
		hasNewBlob := f.Status != StatusModeChanged
		if hasNewBlob {
			total += size
		}
		if size > o.MaxFileBytes {
			if hasNewBlob {
				col.add(f.Path, ReasonTooLarge, fmt.Sprintf("%d bytes, the limit is %d", size, o.MaxFileBytes))
			}
			continue
		}
		if size == 0 {
			continue
		}
		info, ok := probed[e.newSHA]
		if !ok {
			if info, err = probeBlob(ctx, g, e.newSHA); err != nil {
				return err
			}
			probed[e.newSHA] = info
		}
		f.Binary = info.binary
		if info.lfs && hasNewBlob {
			col.add(f.Path, ReasonLFSPointer, "")
		}
	}
	if total > o.MaxTotalBytes {
		col.add("", ReasonTooLarge, fmt.Sprintf("%d bytes in new files, the limit is %d", total, o.MaxTotalBytes))
	}
	return nil
}

// blobSizes returns the size of each blob, with "git cat-file --batch-check".
func blobSizes(ctx context.Context, g gitCmd, shas []string) (map[string]int64, error) {
	out, err := g.outAll(ctx, strings.NewReader(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(shas))
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			return nil, fmt.Errorf("patch: unexpected cat-file answer %q", oneLine([]byte(line)))
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("patch: unexpected blob size in %q", oneLine([]byte(line)))
		}
		sizes[f[0]] = n
	}
	return sizes, nil
}

// probeBlob reads a blob (the runner keeps at most its first 8 MiB, which is
// more than enough) and looks at its start.
func probeBlob(ctx context.Context, g gitCmd, sha string) (blobInfo, error) {
	out, err := g.out(ctx, nil, "cat-file", "blob", sha)
	if err != nil {
		return blobInfo{}, err
	}
	head := out
	if len(head) > binaryProbe {
		head = head[:binaryProbe]
	}
	return blobInfo{
		binary: bytes.IndexByte(head, 0) >= 0,
		lfs:    bytes.HasPrefix(out, []byte(lfsPrefix)),
	}, nil
}
