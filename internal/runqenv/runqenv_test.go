package runqenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBaseSharedKeys(t *testing.T) {
	dir := t.TempDir()
	env := Base(
		Identity{TaskID: "t1", JobID: "j1", Project: "p1", TaskDir: dir},
		Safety{FactorPercent: 10, ExtraGB: 5},
	)

	want := map[string]string{
		"RUNQ_TASK_ID":               "t1",
		"RUNQ_JOB_ID":                "j1",
		"RUNQ_PROJECT_NAME":          "p1",
		"RUNQ_TASK_DIR":              dir,
		"RUNQ_PARAMS_FILE":           filepath.Join(dir, "params.json"),
		"RUNQ_METRICS_FILE":          filepath.Join(dir, "metrics.jsonl"),
		"RUNQ_CHECKPOINT_DIR":        filepath.Join(dir, "checkpoints"),
		"RUNQ_SAFETY_FACTOR_PERCENT": "10",
		"RUNQ_SAFETY_EXTRA_GB":       "5",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}

	// Backend-specific keys must NOT be set by Base.
	for _, k := range []string{"RUNQ_SOCKET_PATH", "RUNQ_NO_DAEMON"} {
		if _, ok := env[k]; ok {
			t.Errorf("Base must not set %q (backend layers it on)", k)
		}
	}
}

func TestBaseWandbOnlyWhenFileExists(t *testing.T) {
	dir := t.TempDir()

	// No wandb file → key absent.
	if _, ok := Base(Identity{TaskDir: dir}, Safety{})["RUNQ_WANDB_CONFIG_FILE"]; ok {
		t.Fatal("RUNQ_WANDB_CONFIG_FILE should be absent when the file does not exist")
	}

	// Create it → key present and points at it.
	wandb := filepath.Join(dir, "wandb_config.json")
	if err := os.WriteFile(wandb, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Base(Identity{TaskDir: dir}, Safety{})["RUNQ_WANDB_CONFIG_FILE"]; got != wandb {
		t.Fatalf("RUNQ_WANDB_CONFIG_FILE = %q, want %q", got, wandb)
	}
}

func TestBaseNoTaskDirOmitsPaths(t *testing.T) {
	env := Base(Identity{TaskID: "t", JobID: "j", Project: "p"}, Safety{})
	for _, k := range []string{"RUNQ_TASK_DIR", "RUNQ_PARAMS_FILE", "RUNQ_METRICS_FILE", "RUNQ_CHECKPOINT_DIR"} {
		if _, ok := env[k]; ok {
			t.Errorf("path var %q should be omitted when TaskDir is empty", k)
		}
	}
	// Identity + safety still present.
	if env["RUNQ_TASK_ID"] != "t" || env["RUNQ_SAFETY_EXTRA_GB"] != "0" {
		t.Errorf("identity/safety vars missing: %#v", env)
	}
}
