package preflight

// Test spec for preflight.go — for Codex to flesh out.
//
// The functions under test are unit-testable as pure-ish funcs:
// checkWritable / checkPathArgs / extractImports / topLevelModule.
// checkImports and checkPipCheck shell out to `python` and need to be
// either skipped when python is unavailable, or driven by a fake
// python binary on $PATH (see the t.Setenv("PATH", ...) trick at the
// bottom of this file).
//
// The cases below are the spec; the file lives here so `go test
// ./internal/preflight/...` discovers it. Codex: please flesh out the
// bodies.

import "testing"

// ----------------------------------------------------------------------
// (1) checkWritable
// ----------------------------------------------------------------------
//
//	- Empty dir string → finding with "working_dir is empty".
//	- Dir that does not exist → finding with "does not exist".
//	- Path that is a file, not a dir → finding with "not a directory".
//	- Dir that exists + writable → 0 findings.
//	- Dir that exists but read-only (chmod 0555) → finding "not writable".
//	  (Skip on Windows.)
//
// The OS-permission test should restore mode in t.Cleanup so a CI
// failure doesn't leave un-deletable temp dirs behind.

func TestCheckWritable_Missing(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckWritable_File(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckWritable_OK(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckWritable_ReadOnly(t *testing.T) {
	t.Skip("TODO(codex)")
}

// ----------------------------------------------------------------------
// (2) checkPathArgs
// ----------------------------------------------------------------------
//
//	- Command with `--data /missing/path` (path doesn't exist) → finding.
//	- Command with `--data=/missing/path` (=-form) → finding.
//	- Command with `--data /tmp` (always-OK path) → 0 findings.
//	- Command with an existing absolute path → 0 findings.
//	- Multiple references to the same missing path → deduped to 1 finding.
//	- Allowlisted paths (/dev/null, /tmp) → 0 findings.
//	- Relative paths are ignored (../foo, configs/x.yaml).
//	- Trailing punctuation in tokens (`/abs/path.` from a comment-style
//	  end of line) → stripped before stat.

func TestCheckPathArgs_MissingPath(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckPathArgs_EqualsForm(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckPathArgs_AllowlistedDevNull(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckPathArgs_ExistingPathOK(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckPathArgs_RelativePathIgnored(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestCheckPathArgs_DuplicatesDeduped(t *testing.T) {
	t.Skip("TODO(codex)")
}

// ----------------------------------------------------------------------
// (3) extractImports + topLevelModule
// ----------------------------------------------------------------------
//
//	- `python script.py` with `import torch` and `from transformers
//	  import AutoModel` → modules = {"torch", "transformers"}; path is
//	  resolved against workingDir.
//	- Dotted name `import torch.nn.functional` → contributes "torch".
//	- `import a, b, c` → all three captured.
//	- `from a.b import x as y` → contributes "a".
//	- Indented imports inside functions / classes are NOT extracted
//	  (only top-level imports get checked).
//	- `python3 -u path/to/main.py` (extra flags) → still picks the .py.
//	- Absolute script path in command → used directly, not joined with
//	  workingDir.
//	- Missing script file → returns the path AND a non-nil error so the
//	  caller can record a clean finding.
//	- topLevelModule("a.b.c") → "a"; topLevelModule("a") → "a".

func TestExtractImports_TopLevelOnly(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestExtractImports_DottedToHead(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestExtractImports_MultipleOnOneLine(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestExtractImports_IgnoresIndented(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestExtractImports_AbsoluteScriptPath(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestExtractImports_MissingScriptErrors(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestTopLevelModule(t *testing.T) {
	if got := topLevelModule("torch.nn"); got != "torch" {
		t.Fatalf("torch.nn → %q, want torch", got)
	}
	if got := topLevelModule("os"); got != "os" {
		t.Fatalf("os → %q, want os", got)
	}
}

// ----------------------------------------------------------------------
// (4) RunPreflight orchestration
// ----------------------------------------------------------------------
//
//	- pf.Skip=true → always returns nil, even with broken project.
//	- Empty taskParams → no sample command; only writable + pip check
//	  paths exercised; should still work without panic.
//	- Multiple findings from different checks → all surface in the
//	  combined error message.
//	- Findings are formatted as "  - <kind>: <detail>" so the operator
//	  can read them quickly.

func TestRunPreflight_Skip(t *testing.T) {
	t.Skip("TODO(codex)")
}

func TestRunPreflight_AggregatesFindings(t *testing.T) {
	t.Skip("TODO(codex)")
}
