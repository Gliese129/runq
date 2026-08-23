package rfs

import (
	"os"
	"path/filepath"
	"testing"
)

// tree builds a temp directory tree: each entry is a relative file path
// ("a/b.txt"); directories are created implicitly.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func globPaths(t *testing.T, root, pattern string, limit int) ([]string, bool) {
	t.Helper()
	matches, truncated, err := Glob(NewLocalFS(), root, pattern, limit)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		rel, err := filepath.Rel(root, m.Path)
		if err != nil {
			rel = m.Path
		}
		out[i] = filepath.ToSlash(rel)
	}
	return out, truncated
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGlobSingleDirectory(t *testing.T) {
	// The bread-and-butter case: one eval sweep over a checkpoint dir.
	root := tree(t,
		"runs/train-8b/checkpoints/ckpt-1000.pt",
		"runs/train-8b/checkpoints/ckpt-2000.pt",
		"runs/train-8b/checkpoints/latest.txt",
		"runs/train-8b/train.log",
	)
	got, truncated := globPaths(t, root, "runs/train-8b/checkpoints/ckpt-*.pt", 500)
	eq(t, got, "runs/train-8b/checkpoints/ckpt-1000.pt", "runs/train-8b/checkpoints/ckpt-2000.pt")
	if truncated {
		t.Error("truncated on a 2-match resolution")
	}
}

func TestGlobDoubleStarSpansSegments(t *testing.T) {
	root := tree(t,
		"data/a/shard.jsonl",
		"data/a/b/shard.jsonl",
		"data/a/b/c/shard.jsonl",
		"data/a/b/other.txt",
	)
	got, _ := globPaths(t, root, "data/**/shard.jsonl", 500)
	eq(t, got, "data/a/b/c/shard.jsonl", "data/a/b/shard.jsonl", "data/a/shard.jsonl")
}

func TestGlobDoubleStarMatchesZeroSegments(t *testing.T) {
	root := tree(t, "ckpt.pt", "sub/ckpt.pt")
	got, _ := globPaths(t, root, "**/ckpt.pt", 500)
	eq(t, got, "ckpt.pt", "sub/ckpt.pt")
}

func TestGlobTrailingDoubleStarTakesEverythingBelow(t *testing.T) {
	root := tree(t, "out/a.txt", "out/deep/b.txt")
	got, _ := globPaths(t, root, "out/**", 500)
	eq(t, got, "out/a.txt", "out/deep", "out/deep/b.txt")
}

func TestGlobSkipsHiddenUnlessAsked(t *testing.T) {
	root := tree(t, "data/.hidden.jsonl", "data/visible.jsonl")
	got, _ := globPaths(t, root, "data/*.jsonl", 500)
	eq(t, got, "data/visible.jsonl")

	got, _ = globPaths(t, root, "data/.*.jsonl", 500)
	eq(t, got, "data/.hidden.jsonl")
}

func TestGlobDoesNotDescendHiddenDirs(t *testing.T) {
	// A `**` from a project root must not drag in .git's thousands of files.
	root := tree(t, ".git/objects/deadbeef", "src/main.go")
	got, _ := globPaths(t, root, "**", 500)
	eq(t, got, "src", "src/main.go")
}

func TestGlobAbsolutePatternOverridesRoot(t *testing.T) {
	root := tree(t, "ckpt-1.pt")
	got, _, err := Glob(NewLocalFS(), "/nonexistent", filepath.Join(root, "ckpt-*.pt"), 500)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(got) != 1 || got[0].Path != filepath.Join(root, "ckpt-1.pt") {
		t.Fatalf("absolute pattern not honored: %+v", got)
	}
}

func TestGlobLimitTruncates(t *testing.T) {
	files := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		files = append(files, string(rune('a'+i))+".pt")
	}
	root := tree(t, files...)
	got, truncated := globPaths(t, root, "*.pt", 4)
	if len(got) != 4 || !truncated {
		t.Fatalf("limit not enforced: %d matches, truncated=%v", len(got), truncated)
	}
}

func TestGlobNoMatchIsEmptyNotError(t *testing.T) {
	// 0 matches is a legitimate answer here — the SUBMIT path is where an
	// empty selection has to be caught, not the resolver.
	root := tree(t, "a.txt")
	got, truncated := globPaths(t, root, "*.pt", 500)
	if len(got) != 0 || truncated {
		t.Fatalf("got %v truncated=%v", got, truncated)
	}
}

func TestGlobMissingRootIsEmptyNotError(t *testing.T) {
	_, _, err := Glob(NewLocalFS(), "/definitely/not/here", "*.pt", 500)
	if err != nil {
		t.Fatalf("unreadable root must not fail the resolution: %v", err)
	}
}

func TestGlobEmptyPattern(t *testing.T) {
	root := tree(t, "a.txt")
	got, _ := globPaths(t, root, "   ", 500)
	if len(got) != 0 {
		t.Fatalf("empty pattern matched %v", got)
	}
}

func TestGlobReportsDirsForFolderParams(t *testing.T) {
	root := tree(t, "runs/exp-a/ckpt.pt", "runs/exp-b/ckpt.pt")
	matches, _, err := Glob(NewLocalFS(), root, "runs/exp-*", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || !matches[0].IsDir || !matches[1].IsDir {
		t.Fatalf("folder matches not flagged as dirs: %+v", matches)
	}
}

func TestGlobSymlinkedDirIsFollowed(t *testing.T) {
	// `~/fast -> /scratch/...` is routine on HPC: a symlinked checkpoint
	// dir must resolve, and a self-referential link must still terminate.
	root := tree(t, "real/ckpt-1.pt")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "fast")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, _ := globPaths(t, root, "fast/ckpt-*.pt", 500)
	eq(t, got, "fast/ckpt-1.pt")

	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Must terminate (depth cap), not hang or blow the stack.
	if _, _, err := Glob(NewLocalFS(), root, "**/ckpt-1.pt", 500); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}
}
