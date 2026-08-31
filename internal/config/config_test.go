package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRoot_NoDataPath(t *testing.T) {
	workDir := t.TempDir()
	cfg := &GlobalConfig{}

	root, err := ResolveRoot(cfg, workDir, "myproj")
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}
	want := filepath.Join(workDir, ".runq")
	if root != want {
		t.Errorf("root = %s, want %s", root, want)
	}
}

func TestResolveRoot_NilConfig(t *testing.T) {
	workDir := t.TempDir()
	root, err := ResolveRoot(nil, workDir, "myproj")
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}
	want := filepath.Join(workDir, ".runq")
	if root != want {
		t.Errorf("root = %s, want %s", root, want)
	}
}

func TestResolveRoot_WithDataPath_ReturnsPhysicalPath(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	cfg := &GlobalConfig{DataPath: dataDir}

	root, err := ResolveRoot(cfg, workDir, "myproj")
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}

	// Returned root is the PHYSICAL path, not the symlink.
	wantPhysical := filepath.Join(dataDir, "myproj")
	if root != wantPhysical {
		t.Errorf("root = %s, want %s (physical path)", root, wantPhysical)
	}

	// The physical directory should exist.
	if fi, err := os.Stat(wantPhysical); err != nil || !fi.IsDir() {
		t.Errorf("physical directory %s should exist and be a dir", wantPhysical)
	}

	// Convenience symlink should exist and point to the physical path.
	link := filepath.Join(workDir, ".runq")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != wantPhysical {
		t.Errorf("symlink target = %s, want %s", target, wantPhysical)
	}
}

func TestResolveRoot_SymlinkIdempotent(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	cfg := &GlobalConfig{DataPath: dataDir}

	// Call twice — second should be a no-op.
	if _, err := ResolveRoot(cfg, workDir, "proj"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := ResolveRoot(cfg, workDir, "proj"); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestResolveRoot_SymlinkRepoints_ButOldPathStillValid(t *testing.T) {
	workDir := t.TempDir()
	oldData := t.TempDir()
	newData := t.TempDir()

	// First call: returns old physical path.
	cfg1 := &GlobalConfig{DataPath: oldData}
	root1, err := ResolveRoot(cfg1, workDir, "proj")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	wantOld := filepath.Join(oldData, "proj")
	if root1 != wantOld {
		t.Errorf("first root = %s, want %s", root1, wantOld)
	}

	// Write a file to the old physical dir (simulates a running task).
	os.WriteFile(filepath.Join(wantOld, "sentinel"), []byte("old"), 0o644)

	// Second call: returns new physical path; symlink re-points.
	cfg2 := &GlobalConfig{DataPath: newData}
	root2, err := ResolveRoot(cfg2, workDir, "proj")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	wantNew := filepath.Join(newData, "proj")
	if root2 != wantNew {
		t.Errorf("second root = %s, want %s", root2, wantNew)
	}

	// Old physical dir is STILL accessible (old task rows survive).
	if _, err := os.Stat(filepath.Join(wantOld, "sentinel")); err != nil {
		t.Errorf("old physical dir should still be reachable: %v", err)
	}
}

func TestResolveRoot_RealDirBlocksSymlink(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()

	// Create a real directory where the symlink would go.
	realDir := filepath.Join(workDir, ".runq")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlink creation is best-effort: ResolveRoot succeeds and returns the
	// physical path even when the convenience symlink cannot be placed.
	cfg := &GlobalConfig{DataPath: dataDir}
	root, err := ResolveRoot(cfg, workDir, "proj")
	if err != nil {
		t.Fatalf("ResolveRoot should succeed (symlink is best-effort): %v", err)
	}
	want := filepath.Join(dataDir, "proj")
	if root != want {
		t.Errorf("root = %s, want %s", root, want)
	}
}

func TestValidateProjectName(t *testing.T) {
	bad := []string{"", ".", "..", "../x", "foo/bar", "a\\b", "/abs"}
	for _, name := range bad {
		if _, err := ResolveRoot(nil, "/tmp", name); err == nil {
			t.Errorf("expected error for project name %q, got nil", name)
		}
	}
	good := []string{"my-proj", "train_v2", "exp.2024"}
	for _, name := range good {
		// nil config → project_path mode, no mkdir needed.
		if _, err := ResolveRoot(nil, t.TempDir(), name); err != nil {
			t.Errorf("unexpected error for project name %q: %v", name, err)
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataPath != "" {
		t.Errorf("DataPath = %q, want empty", cfg.DataPath)
	}
	if got := cfg.ResolveTargets(); len(got) != 0 {
		t.Errorf("ResolveTargets() = %+v, want empty", got)
	}
	if got := cfg.ResolveDefaultTarget(); got != "" {
		t.Errorf("ResolveDefaultTarget() = %q, want empty", got)
	}
}

func TestLoad_WithDataPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("data_path: /scratch/runq\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataPath != "/scratch/runq" {
		t.Errorf("DataPath = %q, want /scratch/runq", cfg.DataPath)
	}
}

func TestLoad_StaleModeKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mode: hpc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// mode is dead (D9): a leftover key in an old file parses as an
	// ignored unknown — never an error, never behavior.
	if got := cfg.ResolveDefaultTarget(); got != "" {
		t.Errorf("default target = %q, want unconfigured", got)
	}
}

func TestResolveDefaultTarget(t *testing.T) {
	tests := []struct {
		name string
		cfg  GlobalConfig
		want string
	}{
		{name: "empty", cfg: GlobalConfig{}, want: ""},
		{name: "stale default without targets", cfg: GlobalConfig{DefaultTarget: "deleted"}, want: ""},
		{name: "first configured target", cfg: GlobalConfig{Targets: []TargetConfig{{Name: "a"}, {Name: "b"}}}, want: "a"},
		{name: "explicit configured target", cfg: GlobalConfig{DefaultTarget: "b", Targets: []TargetConfig{{Name: "a"}, {Name: "b"}}}, want: "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveDefaultTarget(); got != tt.want {
				t.Fatalf("ResolveDefaultTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoad_ArbitraryModeValueIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mode: cluster\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("stale mode key must be ignored, got error: %v", err)
	}
}

func TestSetKeyPreservesHPCSection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	src := []byte("data_path: /old\ntargets:\n  - name: my-lab\n    scheduler: slurm\nhpc:\n  submit_template: sbatch {{run_sh}}\n  submit_id_regex: \"([0-9]+)\"\n  kill_template: scancel {{ext_id}}\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetKey("default_target", "my-lab"); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultTarget != "my-lab" {
		t.Errorf("DefaultTarget = %q, want my-lab", cfg.DefaultTarget)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), "hpc:") || !strings.Contains(string(buf), "submit_template") {
		t.Fatalf("hpc section was not preserved:\n%s", string(buf))
	}
}

func TestSetKeyRejectsUnconfiguredDefaultTarget(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	if err := SetKey("default_target", "missing"); err == nil || !strings.Contains(err.Error(), "add the target first") {
		t.Fatalf("SetKey error = %v, want actionable missing-target error", err)
	}
}
