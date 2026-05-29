package service

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
// ./internal/service/...` discovers it. Codex: please flesh out the
// bodies.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// ----------------------------------------------------------------------
// helpers (codex can use these for the bodies above)
// ----------------------------------------------------------------------

// withWritableDir creates a tmp dir, returns its path, and registers
// cleanup. Codex: use this in writability tests.
func withWritableDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return d
}

// withPythonScript writes a tiny Python file under workingDir and
// returns its path (used to drive extractImports tests).
func withPythonScript(t *testing.T, workingDir, name, source string) string {
	t.Helper()
	p := filepath.Join(workingDir, name)
	if err := os.WriteFile(p, []byte(source), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// withFakePythonOnPath puts a tiny bash stub named ``python`` (and
// ``python3``) at the front of $PATH for the test's lifetime. The stub
// reads its argv and behaves according to ``behavior`` — typically
// "ok" (exit 0) or "missing" (exit 1 with a ModuleNotFoundError-style
// message). Use this in checkImports / checkPipCheck tests to keep
// the suite hermetic.
//
// behavior strings:
//
//	"ok"      — succeed on every import probe
//	"missing" — fail with "ModuleNotFoundError: No module named 'X'"
//	"pipbad"  — succeed on import probes, fail pip check with one
//	            conflict line
func withFakePythonOnPath(t *testing.T, behavior string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/usr/bin/env bash
case "$behavior" in
  ok) exit 0 ;;
  missing)
    # the actual import target is the last arg; mimic CPython's
    # error format so checkImports's lastLine() picks it up.
    target=${@: -1}
    target=${target#import }
    >&2 echo "ModuleNotFoundError: No module named '${target}'"
    exit 1 ;;
  pipbad)
    if [[ "$*" == *"pip check"* ]]; then
      echo "tensorflow 2.5 has requirement numpy~=1.19, but you have numpy 1.24."
      exit 1
    fi
    exit 0 ;;
esac
exit 0
`
	stub := filepath.Join(bin, "python")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake python: %v", err)
	}
	if err := os.Link(stub, filepath.Join(bin, "python3")); err != nil {
		_ = os.WriteFile(filepath.Join(bin, "python3"), []byte(script), 0o755)
	}
	t.Setenv("behavior", behavior)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// withContext returns a context with the given timeout for subprocess-
// driven tests. Used to keep import / pip check probes from hanging
// the suite on slow CI.
func withContext(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), timeout)
}

// requireFinding asserts that ``findings`` contains at least one entry
// whose Detail field substring-matches ``want``. Codex: this is the
// idiomatic assertion shape for the table-driven tests above.
func requireFinding(t *testing.T, findings []PreflightFinding, want string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Detail, want) {
			return
		}
	}
	t.Fatalf("no finding contained %q; got %#v", want, findings)
}
