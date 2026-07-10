// env.go — the RUNQ_* environment half of the SDK contract (formerly
// package runqenv; merged: the env contract and the file contract are two
// halves of one SDK surface, and this package is that surface).
//
// BaseEnv returns only the variables both backends share. Backend-specific
// keys are layered on top by the caller:
//   - daemon adds RUNQ_SOCKET_PATH (so the SDK can reach the daemon)
//   - HPC adds RUNQ_NO_DAEMON=1 and sets no socket
//
// Path variables come from the layout helpers in workspace.go, so the
// on-disk file contract lives in exactly one place.

package workspace

import (
	"os"
	"strconv"
	"strings"
)

// Identity is the task's stable identity plus its workspace directory.
type Identity struct {
	TaskID    string
	JobID     string
	Project   string
	TaskDir   string   // per-task workspace; when empty, path vars are omitted
	SweepKeys []string // sweep parameter key names (for WANDB_TAGS + WANDB_RUN_NAME)
	JobNote   string   // job note (for WANDB_RUN_GROUP)
}

// Safety carries the disk self-freeze parameters the SDK reads to size its
// pre-checkpoint headroom check. Always emitted so the SDK never needs fallback
// defaults — a missing value would be a backend bug.
type Safety struct {
	FactorPercent int
	ExtraGB       int
}

// BaseEnv returns the shared RUNQ_* variables. It does NOT set RUNQ_SOCKET_PATH or
// RUNQ_NO_DAEMON. RUNQ_WANDB_CONFIG_FILE is only set when the file exists, so the
// SDK can treat its presence as a binary "wandb configured" signal.
func BaseEnv(id Identity, safety Safety) map[string]string {
	env := make(map[string]string, 10)
	env["RUNQ_TASK_ID"] = id.TaskID
	env["RUNQ_JOB_ID"] = id.JobID
	env["RUNQ_PROJECT_NAME"] = id.Project

	if id.TaskDir != "" {
		env["RUNQ_TASK_DIR"] = id.TaskDir
		env["RUNQ_PARAMS_FILE"] = ParamsPath(id.TaskDir)
		env["RUNQ_METRICS_FILE"] = MetricsPath(id.TaskDir)
		env["RUNQ_CHECKPOINT_DIR"] = CheckpointsDir(id.TaskDir)

		wandbCfg := WandbConfigPath(id.TaskDir)
		if _, err := os.Stat(wandbCfg); err == nil {
			env["RUNQ_WANDB_CONFIG_FILE"] = wandbCfg
		}
	}

	env["RUNQ_SAFETY_FACTOR_PERCENT"] = strconv.Itoa(safety.FactorPercent)
	env["RUNQ_SAFETY_EXTRA_GB"] = strconv.Itoa(safety.ExtraGB)

	if len(id.SweepKeys) > 0 {
		env["RUNQ_SWEEP_KEYS"] = strings.Join(id.SweepKeys, ",")
		env["WANDB_TAGS"] = strings.Join(id.SweepKeys, ",")
	}
	if id.JobNote != "" {
		env["RUNQ_JOB_NOTE"] = id.JobNote
		env["WANDB_RUN_GROUP"] = id.JobNote
	}

	return env
}
