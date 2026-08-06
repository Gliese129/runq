package genfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The core promise (RQ-75, user-aligned): formatting, comments, and key
// order are NOT semantic — the generation must not move.
func TestSemanticHashIgnoresFormatting(t *testing.T) {
	base := []byte("default_target: tsubame\ntargets:\n  - name: tsubame\n    scheduler: pbs\n")
	variants := map[string][]byte{
		"comments":   []byte("# my cluster\ndefault_target: tsubame # prod\ntargets:\n  - name: tsubame\n    scheduler: pbs\n"),
		"key order":  []byte("targets:\n  - scheduler: pbs\n    name: tsubame\ndefault_target: tsubame\n"),
		"whitespace": []byte("default_target:   tsubame\n\n\ntargets:\n    - name: tsubame\n      scheduler: pbs\n"),
	}
	want, err := SemanticHash(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range variants {
		got, err := SemanticHash(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: generation moved on a non-semantic change", name)
		}
	}

	changed, _ := SemanticHash([]byte("default_target: abci\ntargets:\n  - name: tsubame\n    scheduler: pbs\n"))
	if changed == want {
		t.Error("a REAL value change must move the generation")
	}
}

func TestFromBytesParseError(t *testing.T) {
	d := FromBytes("x.yaml", []byte("a: [unclosed"))
	if d.ParseErr == nil {
		t.Fatal("want ParseErr for invalid yaml")
	}
	if d.Generation != "" {
		t.Error("invalid yaml must have no generation")
	}
	if d.ByteHash == "" {
		t.Error("ByteHash must still be computed")
	}
}

func TestSaveCAS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Unconditional first write.
	if err := Save(path, []byte("a: 1\n"), "", 0); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// CAS with the right generation succeeds.
	if err := Save(path, []byte("a: 2\n"), doc.Generation, 0); err != nil {
		t.Fatalf("matching ifMatch rejected: %v", err)
	}

	// CAS with the now-stale generation conflicts and reports current.
	err = Save(path, []byte("a: 3\n"), doc.Generation, 0)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want ConflictError, got %v", err)
	}
	cur, _ := Load(path)
	if conflict.Current != cur.Generation {
		t.Errorf("conflict.Current = %q, want file's current generation %q", conflict.Current, cur.Generation)
	}
	if string(cur.Bytes) != "a: 2\n" {
		t.Errorf("conflicting write must not touch the file, got %q", cur.Bytes)
	}

	// Reformatting on disk does NOT invalidate a caller's ifMatch: the
	// semantic generation is unchanged, so the save goes through.
	if err := os.WriteFile(path, []byte("# note\na:   2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur2, _ := Load(path)
	if err := Save(path, []byte("a: 4\n"), cur2.Generation, 0); err != nil {
		t.Fatalf("reformat-only drift must not conflict: %v", err)
	}
}

func TestSavePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte("a: 2\n"), "", 0); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 preserved", info.Mode().Perm())
	}
}

func TestSaveAgainstMissingFileWithIfMatchConflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.yaml")
	err := Save(path, []byte("a: 1\n"), "deadbeef", 0)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want ConflictError for ifMatch against missing file, got %v", err)
	}
	if conflict.Current != "" {
		t.Errorf("missing file current generation = %q, want empty", conflict.Current)
	}
}

// Round 10: the diff layer must use the SAME equivalence as the
// generation hash — representation-only differences never count.
func TestSemanticEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a: 1\nb: 2\n", "a: 1\nb: 2\n", true}, // byte-equal fast path
		{"a: 1\nb: 2\n", "b: 2\na: 1\n", true}, // key order
		// At the RAW document layer an extra `m: {}` key IS a difference;
		// nil-vs-{} unification happens at the TYPED marshal layer
		// (omitempty) — see config.TestTargetSemanticEquals.
		{"a: 1\n", "a: 1\nm: {}\n", false},
		{"a: 1\n", "a: 1\ns: []\n", false},
		{"a: 1\n", "# c\na: 1\n", true}, // comments
		{"a: 1\n", "a: 2\n", false},     // real change
		{"a: 1\n", ": : :\n", false},    // unparseable = not provably equal
	}
	for i, c := range cases {
		if got := SemanticEqual([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("case %d: SemanticEqual(%q, %q) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}
