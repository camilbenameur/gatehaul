package patch

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reasons carried by Problem.Reason. They never change meaning once released.
const (
	ReasonSymlink         = "symlink"
	ReasonSubmodule       = "submodule"
	ReasonBadMode         = "bad_mode"
	ReasonDotGit          = "dot_git"
	ReasonParentDir       = "parent_dir"
	ReasonAbsolute        = "absolute"
	ReasonBadChar         = "bad_char"
	ReasonShortName       = "short_name"
	ReasonCaseCollision   = "case_collision"
	ReasonFileDirConflict = "file_dir_conflict"
	ReasonLFSPointer      = "lfs_pointer"
	ReasonTooLarge        = "too_large"
	ReasonTooManyFiles    = "too_many_files"
	ReasonSensitive       = "sensitive_refused"
	ReasonUnparsable      = "unparsable"
)

// checkPath returns the reasons why a path of the new tree must be refused.
// Git itself refuses most of these in an index, so this is a second line of
// defence that does not depend on the git version or its configuration.
func checkPath(p string) []string {
	if p == "" {
		return []string{ReasonBadChar}
	}
	var reasons []string
	add := func(r string) {
		if !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	// A name that is not UTF-8 cannot be passed to git as an argument on
	// Windows, or written to most file systems.
	if !utf8.ValidString(p) {
		add(ReasonBadChar)
	}
	abs := strings.HasPrefix(p, "/")
	if abs || hasDrivePrefix(p) {
		add(ReasonAbsolute)
	}
	for _, r := range p {
		if r == '\\' || r == ':' || r < 0x20 || r == 0x7f {
			add(ReasonBadChar)
		}
	}
	for i, c := range strings.Split(p, "/") {
		switch c {
		case "..":
			add(ReasonParentDir)
		case ".":
			add(ReasonBadChar)
		case "":
			if i > 0 || !abs {
				add(ReasonBadChar)
			}
		default:
			// Windows drops trailing dots and spaces, so "ci.yml." is "ci.yml".
			if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
				add(ReasonBadChar)
			}
			// Windows resolves an 8.3 alias to the long name, which git
			// does not know about when it checks the tree.
			if isShortName(c) {
				add(ReasonShortName)
			}
		}
		if isDotGit(c) {
			add(ReasonDotGit)
		}
	}
	return reasons
}

func hasDrivePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// stripIgnorable removes the characters that HFS+ ignores when it compares
// names: the zero-width joiners and marks, the bidirectional controls, the
// deprecated formatting characters and the byte order mark.
func stripIgnorable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return r
	}, s)
}

// isDotGit reports whether a path component names the git directory on any
// platform: ".git" in any case, the NTFS short name "git~1", with trailing
// dots or spaces, an NTFS stream suffix, or invisible characters that HFS+
// ignores.
func isDotGit(c string) bool {
	c = stripIgnorable(c)
	if i := strings.IndexByte(c, ':'); i >= 0 {
		c = c[:i]
	}
	c = strings.ToLower(strings.TrimRight(c, ". "))
	return c == ".git" || c == "git~1"
}

// isShortName reports whether a path component has the shape of an NTFS 8.3
// alias: one to six characters, "~", one to seven digits (eight characters in
// all), then optionally a dot and an extension of at most three characters,
// as in "WORKFL~1" or "GATEHA~1.TOM". Windows resolves such an alias to a
// long name, so a patch could write ".github/workflows/x.yml" through
// ".github/WORKFL~1/x.yml" without any check on the long name.
func isShortName(c string) bool {
	name, ext := c, ""
	if i := strings.LastIndexByte(c, '.'); i >= 0 {
		name, ext = c[:i], c[i+1:]
	}
	if utf8.RuneCountInString(ext) > 3 || strings.Contains(name, ".") {
		return false
	}
	i := strings.LastIndexByte(name, '~')
	if i < 1 {
		return false
	}
	base, digits := name[:i], name[i+1:]
	if digits == "" || len(digits) > 7 {
		return false
	}
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return false
		}
	}
	return utf8.RuneCountInString(base)+1+len(digits) <= 8
}

// normName is the form of a path that is compared to the fixed names of
// sensitivity: without the characters that HFS+ ignores, and with the case
// folded, so that a long s (U+017F) reads as "s" in "agents.md".
func normName(s string) string {
	return strings.ToLower(fold(stripIgnorable(s)))
}

// sensitivity classifies a path. A sensitive path is listed in the report; a
// refused one is also an error unless the caller allows sensitive paths.
// Matching is by component, at any depth, on the normalised name (normName).
func sensitivity(p string) (sensitive, refused bool) {
	parts := strings.Split(normName(p), "/")
	name := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	switch name {
	case "gatehaul.toml":
		return true, true
	case "claude.md", "agents.md", "makefile", "package.json", ".envrc", ".gitattributes", ".gitmodules":
		sensitive = true
	}
	if strings.HasSuffix(name, ".sh") {
		sensitive = true
	}
	for i, d := range dirs {
		switch d {
		case ".claude", ".codex":
			sensitive = true
		case ".githooks", ".husky":
			return true, true
		case ".github":
			if i+1 < len(dirs) && dirs[i+1] == "workflows" {
				return true, true
			}
		}
	}
	return sensitive, false
}

// foldRune maps a rune to the smallest rune of its case-folding orbit, so
// that "K" and the Kelvin sign compare equal.
func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}

func fold(s string) string { return strings.Map(foldRune, s) }

// prefixes returns every leading directory of p followed by p itself:
// "a/b/c" gives "a", "a/b", "a/b/c".
func prefixes(p string) []string {
	var out []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return append(out, p)
}

// caseCollisions returns the added paths that collide, on a case-insensitive
// file system, with another name of the new tree: the path itself or one of
// its directories folds to the same name as a different spelling. all is the
// whole new tree.
func caseCollisions(all, added []string) []string {
	spellings := make(map[string]map[string]struct{})
	for _, p := range all {
		for _, pre := range prefixes(p) {
			k := fold(pre)
			m := spellings[k]
			if m == nil {
				m = make(map[string]struct{})
				spellings[k] = m
			}
			m[pre] = struct{}{}
		}
	}
	var out []string
	for _, p := range added {
		for _, pre := range prefixes(p) {
			if len(spellings[fold(pre)]) > 1 {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

type conflict struct {
	path  string
	other string
}

// dirFileConflicts finds the paths that turn a file into a directory or the
// other way round: an added path below a deleted path, or an added path that
// has deleted paths below it. Applying those to a working tree that has local
// changes in the way is not safe, so they are refused.
func dirFileConflicts(files []File) []conflict {
	var deleted []string
	isDeleted := make(map[string]bool)
	for _, f := range files {
		if f.Status == StatusDeleted {
			deleted = append(deleted, f.Path)
			isDeleted[f.Path] = true
		}
	}
	var out []conflict
	for _, f := range files {
		if f.Status != StatusAdded {
			continue
		}
		ps := prefixes(f.Path)
		for _, pre := range ps[:len(ps)-1] {
			if isDeleted[pre] {
				out = append(out, conflict{path: f.Path, other: pre})
			}
		}
		for _, d := range deleted {
			if strings.HasPrefix(d, f.Path+"/") {
				out = append(out, conflict{path: f.Path, other: d})
			}
		}
	}
	return out
}
