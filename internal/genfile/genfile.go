// Package genfile implements generation'd files (RQ-75): file-backed
// documents whose identity is a SEMANTIC content hash, with compare-and-swap
// writes. It is the file layer under runq's config-coherence machinery —
// config.yaml today, workspace project configs next (RQ-78).
//
// Two hashes, two jobs:
//
//   - ByteHash (sha256 of raw bytes) is a cheap "did the file move at all"
//     wake-up filter — pollers compare it without parsing.
//   - Generation (sha256 of the CANONICAL parsed form) is the document's
//     identity. Reformatting, comments, and key order do not change it, so
//     they never trigger conflicts, lane rebuilds, or job read-only flips.
//
// The canonical form is computed from the GENERIC yaml tree, not a typed
// struct: a runq upgrade that adds struct fields must not change the
// generation of an untouched file (typed marshalling would fold new zero
// values into the hash and flip every stored generation on upgrade).
//
// Writes are optimistic-concurrency (industry shape: ETag/If-Match,
// Kubernetes resourceVersion): the caller states which generation it
// believes it is replacing; a mismatch returns *ConflictError instead of
// silently clobbering someone else's edit. The check and the atomic
// replace (tmp + rename) happen under an advisory flock so two local
// writers cannot interleave.
package genfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"gopkg.in/yaml.v3"
)

// Doc is one loaded snapshot of a generation'd file.
type Doc struct {
	Path  string
	Bytes []byte
	// ByteHash is sha256 over the raw bytes — the cheap change filter.
	ByteHash string
	// Generation is the semantic identity (sha256 of the canonical parsed
	// form). Empty when ParseErr != nil.
	Generation string
	// ParseErr is set when the bytes are not valid yaml. The document
	// still has a ByteHash; consumers keep serving their last good
	// generation and surface this error verbatim (self-report).
	ParseErr error
}

// ConflictError reports a compare-and-swap failure: the file's current
// generation is not the one the writer based its edit on.
type ConflictError struct {
	Path string
	// Current is the file's present generation ("" when the file is
	// missing or currently unparseable).
	Current string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("generation conflict on %s: file has changed since it was read (current generation %q)", e.Path, e.Current)
}

// Load reads path and computes both hashes. A missing file returns
// (nil, fs.ErrNotExist-wrapped error) — callers decide what absence means.
// Unparseable yaml is NOT an error at this layer: the Doc carries ParseErr
// and an empty Generation.
func Load(path string) (*Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return FromBytes(path, raw), nil
}

// FromBytes builds a Doc from in-memory content (the write path hashes its
// candidate bytes the same way the read path does).
func FromBytes(path string, raw []byte) *Doc {
	d := &Doc{Path: path, Bytes: raw, ByteHash: hashBytes(raw)}
	gen, err := SemanticHash(raw)
	if err != nil {
		d.ParseErr = err
		return d
	}
	d.Generation = gen
	return d
}

// SemanticHash computes the generation of a yaml document: parse to the
// generic tree, normalize (string keys, sorted), serialize canonically
// (JSON — object keys sorted by encoding/json), sha256.
func SemanticHash(raw []byte) (string, error) {
	// An empty / whitespace-only / comment-only file parses to nil — a
	// legitimate document with a stable generation of its own.
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return "", err
	}
	canon, err := json.Marshal(normalize(tree))
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	return hashBytes(canon), nil
}

// normalize converts a yaml generic tree into a json.Marshal-friendly,
// deterministic shape: all map keys become strings (yaml.v2 legacy trees
// use interface{} keys; yaml.v3 mostly emits string keys already), nested
// values recurse. encoding/json sorts map keys, which provides the
// deterministic ordering.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	default:
		return v
	}
}

// Save writes newBytes to path with optimistic concurrency:
//
//   - ifMatch == "" bypasses the check (legacy/CLI writers that never read
//     a generation; also first-write of a new file).
//   - otherwise the file's CURRENT semantic generation must equal ifMatch,
//     or Save returns *ConflictError carrying the current generation.
//
// defaultMode is the permission for a NEWLY created file (0 → 0600); an
// existing file always keeps its own permission bits — a deliberately
// tightened config.yaml stays tight, a repo-normal project yaml stays 0644.
//
// The compare and the replace run under an advisory flock (path + ".lock"),
// and the replace itself is atomic (same-dir temp file + rename), so a
// concurrent reader never sees a torn file and a concurrent writer loses
// the race loudly instead of silently.
func Save(path string, newBytes []byte, ifMatch string, defaultMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	unlock, err := lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer unlock()

	if ifMatch != "" {
		current := ""
		switch cur, lerr := Load(path); {
		case lerr == nil:
			current = cur.Generation // "" if currently unparseable
		case errors.Is(lerr, os.ErrNotExist):
			// missing file: current stays "" — an ifMatch computed from a
			// previously-read file will mismatch, which is the honest call.
		default:
			return lerr
		}
		if current != ifMatch {
			return &ConflictError{Path: path, Current: current}
		}
	}

	// Preserve the existing file's permission bits (config files may be
	// deliberately tightened, e.g. 0600 for config.yaml — RQ-45); new
	// files take the caller's default.
	mode := defaultMode
	if mode == 0 {
		mode = 0o600
	}
	if info, serr := os.Stat(path); serr == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(newBytes); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// lock takes an advisory exclusive flock on lockPath, returning the unlock
// function. The lock file itself is left in place (removing it would race
// other lockers — the git index.lock lesson, inverted).
func lock(lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// SemanticEqual reports whether two yaml documents are SEMANTICALLY
// identical, in the same two layers as the generation mechanism: layer 1
// is a cheap byte comparison (the common case — nothing changed), layer 2
// parses and compares semantic hashes, so representation differences that
// cannot move a generation (key order, comments, formatting) can never count as a
// difference; nil-vs-empty container unification happens one layer up, at
// the TYPED marshal (omitempty) — see config.TargetConfig.SemanticEquals. Unparseable input compares
// unequal — callers treat "can't prove equal" as changed.
func SemanticEqual(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	ha, ea := SemanticHash(a)
	hb, eb := SemanticHash(b)
	return ea == nil && eb == nil && ha == hb
}

// SortedKeys is a small helper for consumers that render diffs of generic
// trees (the field-level conflict dialog): deterministic key order.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
