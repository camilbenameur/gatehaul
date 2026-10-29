package patch

import (
	"context"
	"slices"
	"strings"
)

// lsTreeArgBytes bounds the pathspecs of one "git ls-tree" call, well below
// the limit of a Windows command line.
const lsTreeArgBytes = 8000

// listingPlan returns the directories whose entries are needed to find the
// names that differ only by case from an added path or from one of its
// directories. Element i holds the directories with i+1 components, sorted
// and without duplicates. The top directory is always needed and not listed.
func listingPlan(added []string) [][]string {
	seen := make(map[string]bool)
	var levels [][]string
	for _, p := range added {
		ps := prefixes(p)
		for i, dir := range ps[:len(ps)-1] {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			for len(levels) <= i {
				levels = append(levels, nil)
			}
			levels[i] = append(levels[i], dir)
		}
	}
	for _, dirs := range levels {
		slices.Sort(dirs)
	}
	return levels
}

// siblings returns the entries (files and directories, not what is below
// them) of the top directory and of every directory on the way to an added
// path, as full paths of the tree. Together they hold every name that an
// added path, or one of its directories, could collide with. That is what
// caseCollisions needs, without reading a whole tree that may not fit in the
// output that the runner keeps.
func siblings(ctx context.Context, g gitCmd, tree string, added []string) ([]string, error) {
	all, err := listDirs(ctx, g, tree, nil)
	if err != nil {
		return nil, err
	}
	// Directories of one depth never contain each other, so they can share a
	// call: "ls-tree" would otherwise look inside a directory that another
	// pathspec reaches into, and leave out its entry.
	for _, dirs := range listingPlan(added) {
		for len(dirs) > 0 {
			n, size := 0, 0
			for n < len(dirs) && (n == 0 || size+len(dirs[n]) <= lsTreeArgBytes) {
				size += len(dirs[n]) + 16
				n++
			}
			part, err := listDirs(ctx, g, tree, dirs[:n])
			if err != nil {
				return nil, err
			}
			all = append(all, part...)
			dirs = dirs[n:]
		}
	}
	return all, nil
}

// listDirs lists the entries of the given directories of a tree, or of the
// top directory when there are none. A directory that the tree does not have
// lists nothing.
func listDirs(ctx context.Context, g gitCmd, tree string, dirs []string) ([]string, error) {
	args := []string{"ls-tree", "-z", "--name-only", tree}
	if len(dirs) > 0 {
		args = append(args, "--")
		for _, d := range dirs {
			args = append(args, ":(literal)"+d+"/")
		}
	}
	out, err := g.outAll(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
