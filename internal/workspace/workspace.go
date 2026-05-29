package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/utils"
)

// TaskDir returns the task workspace path under an already-selected root.
// Root selection is a backend concern; this helper only joins mechanically.
func TaskDir(root, taskID string) string {
	return filepath.Join(root, taskID)
}

func ParamsPath(dir string) string {
	return filepath.Join(dir, "params.json")
}

func MetricsPath(dir string) string {
	return filepath.Join(dir, "metrics.jsonl")
}

func CheckpointsDir(dir string) string {
	return filepath.Join(dir, "checkpoints")
}

func WandbConfigPath(dir string) string {
	return filepath.Join(dir, "wandb_config.json")
}

// Write creates <dir>/checkpoints/, writes params.json, and (when wandb is
// configured at project level) writes wandb_config.json. All writes are atomic
// via utils.AtomicWriteFile so the Python SDK never observes a half-written file.
func Write(dir string, params job.TaskParams, wandb *project.WandbConfig) error {
	if err := os.MkdirAll(CheckpointsDir(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	paramsJSON, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	if err := utils.AtomicWriteFile(ParamsPath(dir), paramsJSON, 0o644); err != nil {
		return fmt.Errorf("write params.json: %w", err)
	}

	if wandb != nil {
		wandbJSON, err := json.MarshalIndent(wandb, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal wandb config: %w", err)
		}
		if err := utils.AtomicWriteFile(WandbConfigPath(dir), wandbJSON, 0o644); err != nil {
			return fmt.Errorf("write wandb_config.json: %w", err)
		}
	}
	return nil
}
