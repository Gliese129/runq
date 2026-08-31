package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/project"
)

// Codex r3: the six import-graph corners that regex-based discovery
// could never hold together — statement type, source package context,
// entry mode, and the real sys.path roots. All resolved by running the
// walk INSIDE the probe with CPython's own machinery.

func r3Project(t *testing.T, files map[string]string, cmd string) *project.Config {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: cmd}
}

func r3Imports(t *testing.T, proj *project.Config, cmd string) CheckResult {
	t.Helper()
	pf := DefaultPreflight()
	var report Report
	for _, c := range pf.probeChecks(context.Background(), proj, cmd, &report) {
		if c.Name == "imports" {
			return c
		}
	}
	t.Fatal("no imports result")
	return CheckResult{}
}

// Case 1 (the browser repro): eval/eval.py imports its SIBLING helper —
// resolvable via the script dir — and helper carries a broken dep.
func TestR3SiblingImportInSubdir(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"eval/eval.py":   "import helper\n",
		"eval/helper.py": "import r3_missing_dep_zz\n",
	}, interp+" eval/eval.py")
	c := r3Imports(t, proj, interp+" eval/eval.py")
	if c.Status != "failed" || !strings.Contains(c.Detail, "r3_missing_dep_zz") {
		t.Fatalf("sibling helper's broken dep missed: %+v", c)
	}
}

// Case 2+3: explicit relative imports resolve through the package
// context; a broken dep two relative hops away is still caught.
func TestR3RelativeImports(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"pkg/__init__.py":     "",
		"pkg/sub/__init__.py": "",
		"pkg/sub/eval.py":     "from .helper import x\nfrom ..shared import y\n",
		"pkg/sub/helper.py":   "x = 1\n",
		"pkg/shared.py":       "import r3_missing_shared_zz\ny = 2\n",
	}, interp+" -m pkg.sub.eval")
	c := r3Imports(t, proj, interp+" -m pkg.sub.eval")
	if c.Status != "failed" || !strings.Contains(c.Detail, "r3_missing_shared_zz") {
		t.Fatalf("..shared's broken dep missed: %+v", c)
	}
	if strings.Contains(c.Detail, "helper") {
		t.Fatalf(".helper wrongly failed: %+v", c)
	}

	// And a relative import in a SCRIPT-mode entry is python's own
	// ImportError — reported as such.
	proj2 := r3Project(t, map[string]string{
		"train.py": "from .helper import x\n",
	}, interp+" train.py")
	c2 := r3Imports(t, proj2, interp+" train.py")
	if c2.Status != "failed" || !strings.Contains(c2.Detail, "relative import") {
		t.Fatalf("script-mode relative import not flagged: %+v", c2)
	}
}

// Case 4: `python -m eval.eval` entry — module mode, cwd on sys.path.
func TestR3ModuleEntry(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"eval/__init__.py": "",
		"eval/eval.py":     "import r3_missing_m_zz\n",
	}, interp+" -m eval.eval")
	c := r3Imports(t, proj, interp+" -m eval.eval")
	if c.Status != "failed" || !strings.Contains(c.Detail, "r3_missing_m_zz") {
		t.Fatalf("-m entry not walked: %+v", c)
	}
}

// Case 5: `from eval import helper` — helper is a SUBMODULE, not an
// attribute; it must be found and scanned.
func TestR3FromPackageImportSubmodule(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"train.py":         "from eval import helper\n",
		"eval/__init__.py": "",
		"eval/helper.py":   "import r3_missing_sub_zz\n",
	}, interp+" train.py")
	c := r3Imports(t, proj, interp+" train.py")
	if c.Status != "failed" || !strings.Contains(c.Detail, "r3_missing_sub_zz") {
		t.Fatalf("from-import submodule not scanned: %+v", c)
	}
}

// Case 6a: namespace package (no __init__.py) — PathFinder resolves it.
func TestR3NamespacePackage(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"train.py":     "from ns.helper import x\n",
		"ns/helper.py": "import r3_missing_ns_zz\nx = 1\n",
	}, interp+" train.py")
	c := r3Imports(t, proj, interp+" train.py")
	if c.Status != "failed" || !strings.Contains(c.Detail, "r3_missing_ns_zz") {
		t.Fatalf("namespace package not walked: %+v", c)
	}
}

// Case 6b: external `import a.b` verifies the FULL dotted path — a
// bogus submodule of a real package is at least a warning, never ok.
func TestR3ExternalDottedVerified(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"train.py": "import json.definitely_missing_zz\n",
	}, interp+" train.py")
	c := r3Imports(t, proj, interp+" train.py")
	// r4 ruling: `import a.b` IMPORTS the module a.b — a chain miss is a
	// guaranteed ModuleNotFoundError, external or not. Hard fail.
	if c.Status != "failed" || !strings.Contains(c.Detail, "ModuleNotFoundError") {
		t.Fatalf("external bogus submodule must hard-fail: %+v", c)
	}
}

// Syntax errors are findings — the task would die on exactly this.
func TestR3SyntaxErrorIsFinding(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"train.py": "def broken(:\n",
	}, interp+" train.py")
	c := r3Imports(t, proj, interp+" train.py")
	if c.Status != "failed" || !strings.Contains(c.Detail, "SyntaxError") {
		t.Fatalf("syntax error not surfaced: %+v", c)
	}
}

// try/except-guarded imports are intentionally conditional — never probed.
func TestR3GuardedImportsSkipped(t *testing.T) {
	interp := probePython(t)
	proj := r3Project(t, map[string]string{
		"train.py": "try:\n    import optional_dep_zz\nexcept ImportError:\n    optional_dep_zz = None\nimport json\n",
	}, interp+" train.py")
	c := r3Imports(t, proj, interp+" train.py")
	if c.Status != "passed" {
		t.Fatalf("guarded import wrongly probed: %+v", c)
	}
}
