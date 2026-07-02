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

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/project"
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
	dir := t.TempDir()
	script := `import argparse
import json
import math
import os
from pathlib import Path
from typing import Any

Row = dict[str, Any]
`
	if err := os.WriteFile(dir+"/train.py", []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	path, modules, err := (Preflight{}).extractImports("python train.py", dir)
	if err != nil {
		t.Fatalf("extractImports: %v", err)
	}
	if !strings.HasSuffix(path, "/train.py") {
		t.Fatalf("path = %q, want train.py", path)
	}
	want := []string{"argparse", "json", "math", "os", "pathlib", "typing"}
	if !reflect.DeepEqual(modules, want) {
		t.Fatalf("modules = %#v, want %#v", modules, want)
	}
	for _, mod := range modules {
		if strings.Contains(mod, "\n") {
			t.Fatalf("module name crossed line boundary: %q", mod)
		}
	}
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

// Three-state report: skips are honest non-failures, never blockers.
func TestThreeStateReport(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi"}

	// Skip flag → single skipped entry, OK.
	pf := DefaultPreflight()
	pf.Skip = true
	rep := pf.RunPreflight(context.Background(), proj, "echo hi")
	if !rep.OK() || len(rep.Results) != 1 || rep.Results[0].Status != "skipped" {
		t.Fatalf("skip: %+v", rep.Results)
	}

	// DisableLocal → imports/pip skipped with config reason; still OK.
	pf = DefaultPreflight()
	pf.DisableLocal = true
	pf.Scope = "on this login node"
	rep = pf.RunPreflight(context.Background(), proj, "echo hi")
	statuses := map[string]string{}
	for _, c := range rep.Results {
		statuses[c.Name] = c.Status
	}
	if statuses["imports"] != "skipped" || statuses["pip_check"] != "skipped" {
		t.Errorf("disable_local: %+v", rep.Results)
	}
	if statuses["writable"] != "passed" || statuses["paths"] != "passed" {
		t.Errorf("free tier should still run: %+v", rep.Results)
	}
	// gpus_per_task=0 → gpu skipped with its reason.
	if statuses["gpu"] != "skipped" {
		t.Errorf("gpu should skip when none requested: %+v", rep.Results)
	}
	if !rep.OK() {
		t.Error("skips must never block")
	}
}

// Regression: absolute-path extraction must respect token boundaries —
// relative paths and HF model ids must never be mangled into bogus
// absolute paths (real false positives from an HPC eval command).
func TestCheckPathArgsTokenBoundaries(t *testing.T) {
	cmd := `bash scripts/qsub/evaluate_lighteval.sh --model-name tokyotech-llm/Qwen3-Swallow-8B-RL-v0.2 --repo-path .`
	if f := (Preflight{}).checkPathArgs(cmd); len(f) != 0 {
		t.Fatalf("relative paths / model ids flagged: %+v", f)
	}

	// Genuine absolute paths are still checked: token-initial, after = and quotes.
	bad := (Preflight{}).checkPathArgs(`run --out=/definitely-missing-zz/x '/also-missing-zz/y' /missing-zz/z`)
	if len(bad) != 3 {
		t.Fatalf("want 3 findings, got %+v", bad)
	}
}
