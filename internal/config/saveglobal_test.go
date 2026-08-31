package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/genfile"
)

const saveTestCfg = `# my precious comment
default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: sbatch {{run_sh}}
hpc:
  submit_template: legacy
`

func writeGlobalCfg(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveGlobalIfMatchFreshAndStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	writeGlobalCfg(t, dir, saveTestCfg)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Generation == "" {
		t.Fatal("Load did not compute a generation")
	}

	// Fresh If-Match: the write lands.
	cfg.DefaultTarget = "a"
	cfg.Targets[0].SubmitTemplate = "sbatch --new {{run_sh}}"
	if err := SaveGlobalIfMatch(cfg, cfg.Generation); err != nil {
		t.Fatalf("fresh If-Match save: %v", err)
	}
	after, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Targets[0].SubmitTemplate != "sbatch --new {{run_sh}}" {
		t.Fatalf("edit not persisted: %+v", after.Targets[0])
	}
	if after.Generation == cfg.Generation {
		t.Fatal("generation did not move after a semantic change")
	}

	// Stale If-Match (the generation we loaded FIRST): conflict with the
	// current generation attached.
	cfg2 := *after
	cfg2.DataPath = "/tmp/x"
	err = SaveGlobalIfMatch(&cfg2, cfg.Generation)
	var conflict *genfile.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale If-Match: got %v, want ConflictError", err)
	}
	if conflict.Current != after.Generation {
		t.Fatalf("conflict.Current = %q, want %q", conflict.Current, after.Generation)
	}
}

func TestSaveGlobalPreservesCommentsAndHPC(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	writeGlobalCfg(t, dir, saveTestCfg)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultTarget = "a"
	if err := SaveGlobal(cfg); err != nil {
		t.Fatal(err)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(buf)
	if !strings.Contains(text, "my precious comment") {
		t.Errorf("top-level comment lost:\n%s", text)
	}
	if !strings.Contains(text, "hpc:") || !strings.Contains(text, "legacy") {
		t.Errorf("unmanaged hpc section lost:\n%s", text)
	}
}

func TestSaveGlobalClearsScalarKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	writeGlobalCfg(t, dir, "data_path: /old\n"+saveTestCfg)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath != "/old" {
		t.Fatalf("precondition: data_path = %q", cfg.DataPath)
	}
	// Review fix #3: empty scalar = clear the key (callers start from
	// Load(), so empty can only mean absent-or-cleared).
	cfg.DataPath = ""
	if err := SaveGlobal(cfg); err != nil {
		t.Fatal(err)
	}

	after, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.DataPath != "" {
		t.Fatalf("data_path not cleared: %q", after.DataPath)
	}
	buf, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if strings.Contains(string(buf), "data_path:") {
		t.Fatalf("data_path key still on disk:\n%s", buf)
	}
	if !strings.Contains(string(buf), "default_target: a") {
		t.Fatalf("untouched scalar lost:\n%s", buf)
	}
}

func TestSaveGlobalDeleteLastTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	writeGlobalCfg(t, dir, saveTestCfg)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Targets = nil
	if err := SaveGlobal(cfg); err != nil {
		t.Fatal(err)
	}

	after, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Targets) != 0 {
		t.Fatalf("deleting the last target did not stick: %+v", after.Targets)
	}
	if after.ResolveDefaultTarget() != "" {
		t.Fatalf("effective default after deleting the last target = %q, want empty", after.ResolveDefaultTarget())
	}
	if buf, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); strings.Contains(string(buf), "targets:") {
		t.Fatalf("targets key still present after removing all targets:\n%s", buf)
	} else if strings.Contains(string(buf), "default_target:") {
		t.Fatalf("stale default_target still present after removing all targets:\n%s", buf)
	}
}

// Round 10: nil vs empty containers are the SAME target (identical
// generation) — the reconciler diff must agree.
func TestTargetSemanticEquals(t *testing.T) {
	a := TargetConfig{Name: "x", Scheduler: "slurm", SubmitTemplate: "s"}
	b := a
	b.SignalMap = map[string]string{}
	b.StatusParser = []string{}
	if !a.SemanticEquals(&b) {
		t.Fatal("nil vs empty map/slice judged as different targets")
	}
	if a.SemanticGeneration() != b.SemanticGeneration() {
		t.Fatal("generation moved on a representation-only difference")
	}
	c := a
	c.SubmitTemplate = "s2"
	if a.SemanticEquals(&c) {
		t.Fatal("real change judged equal")
	}
}
