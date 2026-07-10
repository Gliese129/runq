package utils

import (
	"fmt"
	"path"
	"strings"
)

// SafeDeletePath is the RQ-45 guard on DESTRUCTIVE remote/local operations
// (rm -rf targets, RemoveAll). Reads and browsing are deliberately NOT
// guarded — exploring the filesystem is a feature; deleting it is not.
//
// The check is structural, not allowlist-based: task dirs come from the DB
// (task_dir was written at submit from workspace layout <root>/<job>/<task>),
// so a well-formed value is absolute, dot-free, and at least 3 components
// deep. Anything shallower ("/", "/home", "/home/user", "~") or containing
// traversal is a corrupted row or an injection — refuse loudly.
func SafeDeletePath(p string) error {
	if p == "" {
		return fmt.Errorf("delete path is empty")
	}
	if strings.HasPrefix(p, "~") {
		return fmt.Errorf("delete path %q: unexpanded ~", p)
	}
	if !path.IsAbs(p) {
		return fmt.Errorf("delete path %q is not absolute", p)
	}
	clean := path.Clean(p)
	if clean != p && clean+"/" != p {
		return fmt.Errorf("delete path %q is not clean (traversal or duplicate slashes)", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("delete path %q contains traversal", p)
	}
	// Depth: "/a/b/c" → at least 3 non-empty components.
	if len(strings.FieldsFunc(clean, func(r rune) bool { return r == '/' })) < 3 {
		return fmt.Errorf("delete path %q is too shallow to be a task artifact", p)
	}
	return nil
}
