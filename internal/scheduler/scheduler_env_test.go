package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTaskEnvRunqOverridesUser confirms the core invariant: a user who
// (intentionally or accidentally) sets RUNQ_TASK_ID in project.yaml's
// environment block must be overridden by the daemon's value. Otherwise the
// SDK reads a wrong task identity and the entire RUNQ_TASK_DIR/PARAMS/METRICS
// chain breaks.
func TestBuildTaskEnvRunqOverridesUser(t *testing.T) {
	s := &Scheduler{socketPath: "/tmp/runq.sock"}
	task := &Task{
		ID:          "abc12345",
		JobID:       "def67890",
		ProjectName: "proj",
		TaskDir:     "/tmp/work/.runq/abc12345",
		Env: map[string]string{
			"USER_VAR":     "user-value",
			"RUNQ_TASK_ID": "user-tried-to-override",
		},
	}

	env := s.buildTaskEnv(task)

	if env["RUNQ_TASK_ID"] != "abc12345" {
		t.Errorf("RUNQ_TASK_ID = %q, want %q (daemon must win over user)",
			env["RUNQ_TASK_ID"], "abc12345")
	}
	if env["USER_VAR"] != "user-value" {
		t.Errorf("USER_VAR clobbered: got %q, want %q", env["USER_VAR"], "user-value")
	}
}

// TestBuildTaskEnvFullSet verifies every RUNQ_* env we promise to the SDK is
// present, with paths joined from TaskDir.
func TestBuildTaskEnvFullSet(t *testing.T) {
	taskDir := t.TempDir()
	// Pretend wandb is configured: drop the file so the env injection trips.
	if err := os.WriteFile(filepath.Join(taskDir, "wandb_config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write wandb_config.json: %v", err)
	}

	s := &Scheduler{socketPath: "/tmp/sock"}
	task := &Task{
		ID:          "t1",
		JobID:       "j1",
		ProjectName: "proj",
		TaskDir:     taskDir,
		Env:         nil,
	}
	env := s.buildTaskEnv(task)

	expected := map[string]string{
		"RUNQ_TASK_ID":           "t1",
		"RUNQ_JOB_ID":            "j1",
		"RUNQ_PROJECT_NAME":      "proj",
		"RUNQ_TASK_DIR":          taskDir,
		"RUNQ_PARAMS_FILE":       filepath.Join(taskDir, "params.json"),
		"RUNQ_METRICS_FILE":      filepath.Join(taskDir, "metrics.jsonl"),
		"RUNQ_CHECKPOINT_DIR":    filepath.Join(taskDir, "checkpoints"),
		"RUNQ_SOCKET_PATH":       "/tmp/sock",
		"RUNQ_WANDB_CONFIG_FILE": filepath.Join(taskDir, "wandb_config.json"),
	}
	for k, want := range expected {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
}

// TestBuildTaskEnvNoWandbNoEnv guarantees that the absence of wandb_config.json
// means the SDK sees no RUNQ_WANDB_CONFIG_FILE env — its binary signal for
// "wandb is not configured for this task".
func TestBuildTaskEnvNoWandbNoEnv(t *testing.T) {
	taskDir := t.TempDir() // empty dir, no wandb_config.json
	s := &Scheduler{socketPath: ""}
	task := &Task{ID: "t1", JobID: "j1", TaskDir: taskDir}

	env := s.buildTaskEnv(task)
	if _, ok := env["RUNQ_WANDB_CONFIG_FILE"]; ok {
		t.Errorf("RUNQ_WANDB_CONFIG_FILE should be absent when file does not exist, got %q",
			env["RUNQ_WANDB_CONFIG_FILE"])
	}
}

// TestBuildTaskEnvEmptyTaskDir confirms the fallback path: tasks without a
// TaskDir (e.g. legacy DB rows from before L2-C) still get RUNQ_TASK_ID and
// don't blow up with empty-string paths.
func TestBuildTaskEnvEmptyTaskDir(t *testing.T) {
	s := &Scheduler{}
	task := &Task{ID: "t1", JobID: "j1", TaskDir: ""}
	env := s.buildTaskEnv(task)

	if env["RUNQ_TASK_ID"] != "t1" {
		t.Errorf("RUNQ_TASK_ID = %q, want t1", env["RUNQ_TASK_ID"])
	}
	for _, k := range []string{"RUNQ_TASK_DIR", "RUNQ_PARAMS_FILE", "RUNQ_METRICS_FILE", "RUNQ_CHECKPOINT_DIR"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s should be absent for empty TaskDir, got %q", k, v)
		}
	}
}

// TestBuildTaskEnvEmptySocketPath — if daemon constructed Scheduler with an
// empty socketPath (test setups, future detached mode), don't inject an empty
// RUNQ_SOCKET_PATH that the SDK might mistake for a real path.
func TestBuildTaskEnvEmptySocketPath(t *testing.T) {
	s := &Scheduler{socketPath: ""}
	task := &Task{ID: "t1", JobID: "j1", TaskDir: t.TempDir()}
	env := s.buildTaskEnv(task)
	if _, ok := env["RUNQ_SOCKET_PATH"]; ok {
		t.Errorf("RUNQ_SOCKET_PATH should be absent when socketPath is empty")
	}
}

// TestBuildTaskEnvKeyCoverage — sanity guard against drift; if the list of
// RUNQ_* keys changes, update this test and the stage1 doc together.
func TestBuildTaskEnvKeyCoverage(t *testing.T) {
	taskDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(taskDir, "wandb_config.json"), []byte("{}"), 0o644)
	s := &Scheduler{socketPath: "/sock"}
	env := s.buildTaskEnv(&Task{ID: "t", JobID: "j", ProjectName: "p", TaskDir: taskDir})

	wantKeys := []string{
		"RUNQ_TASK_ID", "RUNQ_JOB_ID", "RUNQ_PROJECT_NAME",
		"RUNQ_TASK_DIR", "RUNQ_PARAMS_FILE", "RUNQ_METRICS_FILE",
		"RUNQ_CHECKPOINT_DIR", "RUNQ_SOCKET_PATH", "RUNQ_WANDB_CONFIG_FILE",
	}
	var missing []string
	for _, k := range wantKeys {
		if _, ok := env[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing RUNQ_* keys: %s", strings.Join(missing, ", "))
	}
}
