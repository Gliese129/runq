// Package runqenv builds the RUNQ_* environment variables that form the SDK
// contract. It is the single source of truth for that contract, shared by every
// backend so daemon and HPC never drift.
//
// Base() returns only the variables both backends share. Backend-specific keys
// are layered on top by the caller:
//   - daemon adds RUNQ_SOCKET_PATH (so the SDK can reach the daemon)
//   - HPC adds RUNQ_NO_DAEMON=1 and sets no socket
//
// Path variables are derived from the workspace layout helpers, so the on-disk
// file contract (params.json / metrics.jsonl / checkpoints/) lives in exactly
// one place (internal/workspace).
package runqenv

import (
	"os"
	"strconv"

	"github.com/gliese129/runq/internal/workspace"
)

// Identity is the task's stable identity plus its workspace directory.
type Identity struct {
	TaskID  string
	JobID   string
	Project string
	TaskDir string // per-task workspace; when empty, path vars are omitted
}

// Safety carries the disk self-freeze parameters the SDK reads to size its
// pre-checkpoint headroom check. Always emitted so the SDK never needs fallback
// defaults — a missing value would be a backend bug.
type Safety struct {
	FactorPercent int
	ExtraGB       int
}

// Base returns the shared RUNQ_* variables. It does NOT set RUNQ_SOCKET_PATH or
// RUNQ_NO_DAEMON. RUNQ_WANDB_CONFIG_FILE is only set when the file exists, so the
// SDK can treat its presence as a binary "wandb configured" signal.
func Base(id Identity, safety Safety) map[string]string {
	env := make(map[string]string, 10)
	env["RUNQ_TASK_ID"] = id.TaskID
	env["RUNQ_JOB_ID"] = id.JobID
	env["RUNQ_PROJECT_NAME"] = id.Project

	if id.TaskDir != "" {
		env["RUNQ_TASK_DIR"] = id.TaskDir
		env["RUNQ_PARAMS_FILE"] = workspace.ParamsPath(id.TaskDir)
		env["RUNQ_METRICS_FILE"] = workspace.MetricsPath(id.TaskDir)
		env["RUNQ_CHECKPOINT_DIR"] = workspace.CheckpointsDir(id.TaskDir)

		wandbCfg := workspace.WandbConfigPath(id.TaskDir)
		if _, err := os.Stat(wandbCfg); err == nil {
			env["RUNQ_WANDB_CONFIG_FILE"] = wandbCfg
		}
	}

	env["RUNQ_SAFETY_FACTOR_PERCENT"] = strconv.Itoa(safety.FactorPercent)
	env["RUNQ_SAFETY_EXTRA_GB"] = strconv.Itoa(safety.ExtraGB)
	return env
}
