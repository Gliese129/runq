package preflight

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/project"
	"github.com/gliese129/runq-lab/internal/rfs"
)

// Codex r2 #1a: `from pkg.helper import x` must verify the WHOLE dotted
// chain — pkg/__init__.py alone is not pkg.helper.
func TestDottedLocalImportLayout(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	writeF := func(name, content string) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF("train.py", "from pkg.helper import x\n")
	writeF("pkg/__init__.py", "")

	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " train.py"}
	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" train.py", &report)
	found := false
	for _, c := range results {
		if c.Name == "imports" {
			found = true
			if c.Status != "failed" || !strings.Contains(c.Detail, "pkg.helper") {
				t.Fatalf("missing chain link not caught: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("no imports result")
	}

	// With the chain complete, the leaf's own bad import IS scanned.
	writeF("pkg/helper.py", "import nested_r2_missing_zz\nx = 1\n")
	results = pf.probeChecks(context.Background(), proj, interp+" train.py", &report)
	for _, c := range results {
		if c.Name == "imports" {
			if c.Status != "failed" || !strings.Contains(c.Detail, "nested_r2_missing_zz") {
				t.Fatalf("leaf module's import not scanned: %+v", c)
			}
			return
		}
	}
	t.Fatal("no imports result after completing chain")
}

// User-aligned r2: the probe replicates the task's sys.path —
// `python scripts/train.py` puts the SCRIPT's dir at path[0], so a
// root-level `import rootmod` from a subdir script fails exactly like
// the real run would (project-root vs relative import styles).
func TestSysPathMirrorsScriptDir(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	writeF := func(name, content string) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF("rootmod.py", "v = 1\n")
	writeF("scripts/train.py", "import sibling\nimport rootmod\n")
	writeF("scripts/sibling.py", "w = 2\n")

	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " scripts/train.py"}
	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" scripts/train.py", &report)
	for _, c := range results {
		if c.Name == "imports" {
			// sibling resolves (path[0] = scripts/); rootmod must NOT —
			// and the message should say layout, not env.
			if c.Status != "failed" || !strings.Contains(c.Detail, "rootmod") || !strings.Contains(c.Detail, "layout") {
				t.Fatalf("script-dir sys.path semantics: %+v", c)
			}
			if strings.Contains(c.Detail, `"sibling"`) {
				t.Fatalf("sibling wrongly failed: %+v", c)
			}
			return
		}
	}
	t.Fatal("no imports result")
}

// Codex r2 #4: a python3-only host (no `python` alias) must still run
// contract checks for pure-shell entries — the probe picks the
// interpreter at runtime instead of hardcoding "python".
func TestPython3OnlyHost(t *testing.T) {
	real := probePython(t)
	realPath, err := exec.LookPath(real)
	if err != nil {
		t.Skip("no python")
	}
	bin := t.TempDir()
	link := func(name, target string) {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	// A PATH with ONLY python3 (never `python`) + the shell utilities the
	// probe chain needs.
	link("python3", realPath)
	for _, tool := range []string{"bash", "sh", "rm", "tr"} {
		if p, err := exec.LookPath(tool); err == nil {
			link(tool, p)
		}
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("python"); err == nil {
		t.Skip("python still on PATH; cannot simulate python3-only host")
	}

	dir := t.TempDir()
	wandbOn := true
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi",
		Preflight: &project.PreflightConfig{Wandb: &wandbOn}}

	pf := DefaultPreflight()
	pf.Env = map[string]string{"WANDB_API_KEY": "k"}
	var report Report
	results := pf.probeChecks(context.Background(), proj, "echo hi", &report)
	for _, c := range results {
		if c.Name == "wandb" {
			if c.Status != "passed" {
				t.Fatalf("wandb on python3-only host: %+v", c)
			}
			return
		}
	}
	t.Fatalf("no wandb result on python3-only host: %+v", results)
}

// The single remaining Go-side read is ctx-bounded (Codex r3): a slow
// transport read cannot hold preflight past its deadline — the caller
// returns at the deadline while the read finishes in the background.
func TestReadFileCtxDeadline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	slow := slowFS{FS: rfs.NewLocalFS(), delay: 250 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := readFileCtx(ctx, slow, filepath.Join(dir, "x"))
	if err == nil {
		t.Fatal("deadline not enforced")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("readFileCtx held past deadline: %s", elapsed)
	}
}

type slowFS struct {
	rfs.FS
	delay time.Duration
}

func (s slowFS) ReadFile(p string) ([]byte, error) {
	time.Sleep(s.delay)
	return s.FS.ReadFile(p)
}
