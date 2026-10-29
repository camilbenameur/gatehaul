package patch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/ghx"
)

// ApplyError means git could not apply the patch to the working tree, most
// often because a file was edited locally. Paths lists the files that git
// names (it can be empty); Detail is the start of git's own message, on one
// line.
type ApplyError struct {
	Paths  []string
	Detail string
}

func (e *ApplyError) Error() string {
	switch {
	case len(e.Paths) > 0:
		return "the patch does not apply to the working tree: " + strings.Join(e.Paths, ", ")
	case e.Detail != "":
		return "the patch does not apply to the working tree: " + e.Detail
	}
	return "the patch does not apply to the working tree"
}

// Apply runs "git apply --check" and then "git apply", both with --binary, on
// the working tree that contains dir. It never touches the index, and the
// user's own settings for line endings stay in force. Call it only after
// Check returned OK for the same data. A patch that does not fit the local
// files gives an *ApplyError; any other error means git could not run.
//
// Apply cannot undo a write that fails half way: ApplySafely can.
func Apply(ctx context.Context, r ghx.Runner, dir string, data []byte) error {
	top, err := topLevel(ctx, r, dir, true)
	if err != nil {
		return err
	}
	g := gitCmd{r: r, dir: top, worktree: true}
	for _, args := range [][]string{
		{"apply", "--check", "--binary", "--whitespace=nowarn", "-"},
		{"apply", "--binary", "--whitespace=nowarn", "-"},
	} {
		res, err := g.run(ctx, bytes.NewReader(data), args...)
		if err != nil {
			return err
		}
		if !res.OK() {
			return &ApplyError{Paths: conflictPaths(res.Stderr), Detail: oneLine(res.Stderr)}
		}
	}
	return nil
}

// applyTimeout bounds the write phase of ApplySafely, which no cancellation of
// the caller's context can cut short.
const applyTimeout = 5 * time.Minute

// Names of the files that ApplySafely keeps in its state directory.
const (
	applyingMarker = "applying"
	preimageDir    = "preimage"
	manifestName   = "manifest.json"
)

// ErrAlreadyApplied means the patch does not apply to the working tree but its
// reverse does: the files already hold the patch.
var ErrAlreadyApplied = errors.New("patch: the working tree already holds the patch")

// ErrUnrecovered means an earlier apply was interrupted and Recover has not
// restored its files yet, so nothing may be applied on top of them.
var ErrUnrecovered = errors.New("patch: an earlier apply was interrupted and its files are not restored yet (call Recover first)")

// manifest describes the files saved before the real apply: the work tree they
// belong to, whether each existed, and the directories the apply may create.
type manifest struct {
	Top     string         `json:"top"`
	Files   []manifestFile `json:"files"`
	NewDirs []string       `json:"new_dirs,omitempty"`
}

type manifestFile struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Mode    fs.FileMode `json:"mode,omitempty"`
}

// applyGit runs git in the work tree top for ApplySafely: with LC_ALL=C when
// the runner can set it, so that the messages of git parse in any locale.
func applyGit(r ghx.Runner, top string) gitCmd {
	g := gitCmd{r: r, dir: top, worktree: true}
	if _, ok := r.(ghx.EnvRunner); ok {
		g.env = []string{"LC_ALL=C"}
	}
	return g
}

// ApplySafely is Apply with a way back. It checks that the patch fits (a
// patch that does not is an *ApplyError, or ErrAlreadyApplied when its reverse
// fits), saves the files listed in files into stateDir/preimage, writes the
// marker stateDir/applying and only then runs "git apply". The write cannot be
// interrupted by ctx. When it fails, the saved files are put back and the
// files it created are removed, so that the tree is as it was. A process that
// dies in between leaves the marker, and Recover restores the files.
//
// files are the slash-separated paths below the work tree that the patch
// changes (the paths of Check's report). stateDir belongs to one job, and the
// caller must serialise the calls that share it. The tree is not touched when
// an error comes before the write phase.
func ApplySafely(ctx context.Context, r ghx.Runner, dir string, data []byte, files []string, stateDir string) error {
	if stateDir == "" {
		return errors.New("patch: no state directory")
	}
	top, err := topLevel(ctx, r, dir, true)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(stateDir, applyingMarker)); err == nil {
		return ErrUnrecovered
	}
	g := applyGit(r, top)
	res, err := g.run(ctx, bytes.NewReader(data), "apply", "--check", "--binary", "--whitespace=nowarn", "-")
	if err != nil {
		return err
	}
	if !res.OK() {
		fwd := &ApplyError{Paths: conflictPaths(res.Stderr), Detail: oneLine(res.Stderr)}
		rev, err := g.run(ctx, bytes.NewReader(data), "apply", "--reverse", "--check", "--binary", "--whitespace=nowarn", "-")
		if err != nil {
			return err
		}
		if rev.OK() {
			return ErrAlreadyApplied
		}
		return fwd
	}

	if err := saveFiles(filepath.Clean(top), stateDir, files); err != nil {
		_ = os.RemoveAll(filepath.Join(stateDir, preimageDir))
		return fmt.Errorf("patch: cannot save the files before applying: %w", err)
	}
	// From here on, a failure puts the files back. There is no defer for it: a
	// process that dies must leave the marker for Recover.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	res, err = g.run(actx, bytes.NewReader(data), "apply", "--binary", "--whitespace=nowarn", "-")
	switch {
	case err != nil:
		return rollback(stateDir, fmt.Errorf("patch: git apply: %w", err))
	case !res.OK():
		return rollback(stateDir, fmt.Errorf("patch: git apply failed after its check passed: %s", oneLine(res.Stderr)))
	}
	if err := clearMarker(stateDir); err != nil {
		return rollback(stateDir, fmt.Errorf("patch: cannot clear the apply marker: %w", err))
	}
	return nil
}

// rollback restores the saved files after cause, and returns cause (with the
// reason the restore failed, when it did).
func rollback(stateDir string, cause error) error {
	if _, err := Recover(stateDir); err != nil {
		return fmt.Errorf("%w; the files could not be restored: %v", cause, err)
	}
	return cause
}

// Recover puts back the files of an apply that did not end: it looks for the
// marker in stateDir and, when there is one, restores the saved files, removes
// the files and directories that the apply created, and clears the marker. It
// returns the paths it changed, or nil when there was nothing to recover.
func Recover(stateDir string) ([]string, error) {
	_, err := os.Lstat(filepath.Join(stateDir, applyingMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(stateDir, preimageDir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("patch: cannot read the saved files of the interrupted apply: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("patch: the saved files of the interrupted apply are damaged: %w", err)
	}
	changed, err := restoreFiles(m, stateDir)
	if err != nil {
		return nil, err
	}
	if err := clearMarker(stateDir); err != nil {
		return nil, err
	}
	return changed, nil
}

// clearMarker removes the marker, then the saved files.
func clearMarker(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, applyingMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.RemoveAll(filepath.Join(stateDir, preimageDir))
	return nil
}

// resolve returns the place of the slash-separated path p below top, or an
// error when p could lead out of the work tree or into .git.
func resolve(top, p string) (string, error) {
	if !fs.ValidPath(p) || p == "." || strings.ContainsAny(p, `\:`) {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("unsafe path %q", p)
		}
	}
	return filepath.Join(top, filepath.FromSlash(p)), nil
}

func savedPath(stateDir string, i int) string {
	return filepath.Join(stateDir, preimageDir, "files", strconv.Itoa(i))
}

// saveFiles copies the files that exist, writes the manifest and, last, the
// marker: a marker therefore always comes with a complete manifest. top must
// be clean.
func saveFiles(top, stateDir string, files []string) error {
	pre := filepath.Join(stateDir, preimageDir)
	if err := os.RemoveAll(pre); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(pre, "files"), 0o700); err != nil {
		return err
	}
	paths := slices.Clone(files)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	m := manifest{Top: top, Files: []manifestFile{}}
	newDirs := map[string]bool{}
	for i, p := range paths {
		full, err := resolve(top, p)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(full)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			m.Files = append(m.Files, manifestFile{Path: p})
			// The directories above that do not exist yet may be created.
			for d := filepath.Dir(full); strings.HasPrefix(d, top+string(filepath.Separator)); d = filepath.Dir(d) {
				if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
					break
				}
				newDirs[d] = true
			}
		case err != nil:
			return err
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", p)
		default:
			b, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			if err := writeSynced(savedPath(stateDir, i), b, 0o600); err != nil {
				return err
			}
			m.Files = append(m.Files, manifestFile{Path: p, Existed: true, Mode: fi.Mode().Perm()})
		}
	}
	for d := range newDirs {
		m.NewDirs = append(m.NewDirs, d)
	}
	slices.Sort(m.NewDirs)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := writeSynced(filepath.Join(pre, manifestName), b, 0o600); err != nil {
		return err
	}
	return writeSynced(filepath.Join(stateDir, applyingMarker), []byte("applying\n"), 0o600)
}

// writeSynced writes b to path and flushes it to disk.
func writeSynced(path string, b []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// restoreFiles puts every saved file back (one that already holds its saved
// bytes and mode is left alone, so that its time stays), removes the files that
// did not exist, then the directories that did not exist and are empty. It
// returns the paths it changed.
func restoreFiles(m manifest, stateDir string) ([]string, error) {
	top := filepath.Clean(m.Top)
	if !filepath.IsAbs(top) {
		return nil, fmt.Errorf("patch: the saved files name no work tree (%q)", m.Top)
	}
	changed := []string{}
	for i, f := range m.Files {
		full, err := resolve(top, f.Path)
		if err != nil {
			return changed, err
		}
		if !f.Existed {
			err := os.Remove(full)
			switch {
			case err == nil:
				changed = append(changed, f.Path)
			case !errors.Is(err, fs.ErrNotExist):
				return changed, err
			}
			continue
		}
		saved, err := os.ReadFile(savedPath(stateDir, i))
		if err != nil {
			return changed, fmt.Errorf("patch: the saved copy of %s is missing: %w", f.Path, err)
		}
		if fi, err := os.Lstat(full); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == f.Mode {
			if cur, err := os.ReadFile(full); err == nil && bytes.Equal(cur, saved) {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o777); err != nil {
			return changed, err
		}
		if err := writeSynced(full, saved, f.Mode); err != nil {
			return changed, err
		}
		_ = os.Chmod(full, f.Mode)
		changed = append(changed, f.Path)
	}
	// The deepest directories first; one that is not empty stays.
	dirs := slices.Clone(m.NewDirs)
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	for _, d := range dirs {
		if strings.HasPrefix(filepath.Clean(d), top+string(filepath.Separator)) {
			_ = os.Remove(d)
		}
	}
	return changed, nil
}

// conflictSuffixes end the lines of "git apply" that name a file, as in
// "error: a.txt: patch does not apply".
var conflictSuffixes = []string{
	": patch does not apply",
	": already exists in working directory",
	": No such file or directory",
	": does not exist in working directory",
	": does not match index",
}

// conflictPaths extracts the paths that git apply names in its errors.
func conflictPaths(stderr []byte) []string {
	var paths []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, q := range paths {
			if q == p {
				return
			}
		}
		paths = append(paths, p)
	}
	for _, line := range strings.Split(string(stderr), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "error: ")
		if !ok {
			continue
		}
		if r, ok := strings.CutPrefix(rest, "patch failed: "); ok {
			// "patch failed: a.txt:12"
			if i := strings.LastIndex(r, ":"); i > 0 {
				add(r[:i])
			}
			continue
		}
		for _, suffix := range conflictSuffixes {
			if p, ok := strings.CutSuffix(rest, suffix); ok {
				add(p)
				break
			}
		}
	}
	return paths
}
