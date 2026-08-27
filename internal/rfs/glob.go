package rfs

import (
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Glob resolves a path pattern against an FS on the OWNING side — the same
// doctrine as owning-side grep and activity decimation (RQ-44): results
// travel, files don't. A browser walking a deep tree through single-directory
// listings would pay one SFTP round trip per directory; this pays one call.
//
// Dialect (RQ2-3):
//
//   - ?   [abc]   — within one path segment (path.Match semantics)
//     **            — spans segments, matching zero or more of them
//
// A hidden entry (leading ".") matches only when the pattern segment itself
// starts with "." — the usual glob courtesy, so `*` never drags in .git.
//
// Bounds are mandatory, not optional: patterns are user-typed and a `**` at
// a home directory would otherwise walk a cluster filesystem forever.
// Symlinked directories ARE followed (a `~/fast -> /scratch/...` checkpoint
// dir is routine on HPC); maxVisits and maxDepth are what make a symlink
// loop terminate.
const (
	globMaxVisits = 20000
	globMaxDepth  = 24
)

// GlobMatch is one resolved entry. Size is 0 for entries whose metadata
// could not be read — a match is reported on the strength of its name.
type GlobMatch struct {
	Path  string
	IsDir bool
	Size  int64
}

// Glob walks root and returns entries matching the (root-relative) pattern,
// sorted by path. truncated reports that limit was reached or the traversal
// budget ran out — the caller must surface it rather than presenting a
// partial list as complete.
//
// An absolute pattern is accepted and overrides root; that keeps a
// hand-pasted "/gs/fs/.../ckpt-*.pt" working without the caller having to
// split it.
func Glob(fsys FS, root, pattern string, limit int) (matches []GlobMatch, truncated bool, err error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, false, nil
	}
	if path.IsAbs(pattern) {
		root = "/"
		pattern = strings.TrimPrefix(pattern, "/")
	}
	if root == "" {
		root = "."
	}
	root = path.Clean(root)

	segs := make([]string, 0, 8)
	for _, s := range strings.Split(path.Clean(pattern), "/") {
		if s == "" || s == "." {
			continue
		}
		// Collapse consecutive ** so `a/**/**/b` can't multiply the walk.
		if s == "**" && len(segs) > 0 && segs[len(segs)-1] == "**" {
			continue
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return nil, false, nil
	}

	w := &globWalker{fsys: fsys, limit: limit, seen: map[string]bool{}}
	w.walk(root, segs, 0)
	sort.Slice(w.out, func(i, j int) bool { return w.out[i].Path < w.out[j].Path })
	return w.out, w.truncated, nil
}

type globWalker struct {
	fsys      FS
	limit     int
	visits    int
	truncated bool
	seen      map[string]bool
	out       []GlobMatch
}

// matchSegment applies path.Match plus the hidden-entry courtesy.
func matchSegment(seg, name string) bool {
	if strings.HasPrefix(name, ".") && !strings.HasPrefix(seg, ".") {
		return false
	}
	ok, err := path.Match(seg, name)
	return err == nil && ok
}

func (w *globWalker) emit(p string, e fs.DirEntry, isDir bool) {
	if w.seen[p] {
		return
	}
	if len(w.out) >= w.limit {
		w.truncated = true
		return
	}
	w.seen[p] = true
	var size int64
	if info, err := e.Info(); err == nil {
		size = info.Size()
	}
	w.out = append(w.out, GlobMatch{Path: p, IsDir: isDir, Size: size})
}

// isDir resolves an entry's directory-ness, following symlinks the way
// fs/list does (DirEntry reports the LINK's type, so a symlinked directory
// would otherwise read as an unenterable file).
func (w *globWalker) isDir(parent string, e fs.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	info, err := e.Info()
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := w.fsys.Stat(path.Join(parent, e.Name()))
	return err == nil && target.IsDir()
}

func (w *globWalker) budgetLeft() bool {
	if w.visits >= globMaxVisits || len(w.out) >= w.limit {
		w.truncated = true
		return false
	}
	return true
}

// walk matches segs against the contents of dir. depth counts directory
// levels descended, bounding symlink cycles.
func (w *globWalker) walk(dir string, segs []string, depth int) {
	if len(segs) == 0 || depth > globMaxDepth || !w.budgetLeft() {
		return
	}
	seg, rest := segs[0], segs[1:]

	if seg == "**" {
		// Zero segments: try the remainder right here. `**` as the LAST
		// segment means "everything below", handled by the descent below
		// emitting every entry it sees.
		if len(rest) > 0 {
			w.walk(dir, rest, depth)
		}
	}

	entries, err := w.fsys.ReadDir(dir)
	if err != nil {
		// Unreadable directory (permissions, vanished mid-walk): skip it.
		// A pattern spanning a cluster filesystem will hit these routinely
		// and must not fail the whole resolution.
		return
	}
	w.visits += len(entries)

	for _, e := range entries {
		if !w.budgetLeft() {
			return
		}
		full := path.Join(dir, e.Name())
		isDir := w.isDir(dir, e)

		if seg == "**" {
			// `**` carries the hidden-entry courtesy too, and it matters
			// most here: without it a `**` at a project root walks .git.
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if len(rest) == 0 {
				w.emit(full, e, isDir) // trailing ** — everything below
			}
			if isDir {
				w.walk(full, segs, depth+1) // ** stays active
			}
			continue
		}
		if !matchSegment(seg, e.Name()) {
			continue
		}
		if len(rest) == 0 {
			w.emit(full, e, isDir)
		} else if isDir {
			w.walk(full, rest, depth+1)
		}
	}
}
