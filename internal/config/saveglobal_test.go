package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/genfile"
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
	if buf, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); strings.Contains(string(buf), "targets:") {
		t.Fatalf("targets key still present after removing all targets:\n%s", buf)
	}
}
