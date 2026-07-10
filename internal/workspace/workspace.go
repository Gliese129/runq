package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// TaskDir returns the task workspace path under an already-selected root.
// Root selection is a backend concern; this helper only joins mechanically.
func TaskDir(root, taskID string) string {
	return path.Join(root, taskID)
}

func ParamsPath(dir string) string {
	return path.Join(dir, "params.json")
}

func MetricsPath(dir string) string {
	return path.Join(dir, "metrics.jsonl")
}

// PyramidPath is the multi-resolution metrics index built next to
// metrics.jsonl by `runq metrics-index build` (pyramid.go, this package).
func PyramidPath(dir string) string {
	return path.Join(dir, "metrics.pyr")
}

func CheckpointsDir(dir string) string {
	return path.Join(dir, "checkpoints")
}

func WandbConfigPath(dir string) string {
	return path.Join(dir, "wandb_config.json")
}

func NotePath(dir string) string {
	return path.Join(dir, "note.txt")
}

func ActivityPath(dir string) string {
	return path.Join(dir, "activity.tsv")
}

// TaskMetaPath returns the path to task.meta, a JSON file holding derived
// post-mortem metadata (future fields like TotalLines, ErrorCount, etc.).
func TaskMetaPath(dir string) string {
	return path.Join(dir, "task.meta")
}

// TaskMeta is the JSON schema for task.meta. Add new fields here as needed;
// existing files without the field will unmarshal to their zero value.
// Activity data (ts/bytes/lines) is now recorded directly in activity.tsv
// by the sidecar; this struct is reserved for other post-mortem metadata.
type TaskMeta struct {
	// Future fields: TotalLines, ErrorCount, TimeRange, etc.
}

// ReadTaskMeta reads and parses task.meta. Returns a zero-value TaskMeta
// (not an error) if the file does not exist.
func ReadTaskMeta(dir string) (TaskMeta, error) {
	data, err := os.ReadFile(TaskMetaPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return TaskMeta{}, nil
		}
		return TaskMeta{}, fmt.Errorf("read task.meta: %w", err)
	}
	var meta TaskMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return TaskMeta{}, fmt.Errorf("parse task.meta: %w", err)
	}
	return meta, nil
}

// WriteTaskMeta atomically writes task.meta. It merges: reads existing meta,
// applies the update function, then writes back. This way multiple callers
// can add different fields without overwriting each other.
func WriteTaskMeta(dir string, update func(*TaskMeta)) error {
	meta, err := ReadTaskMeta(dir)
	if err != nil {
		// If parse fails, start fresh
		meta = TaskMeta{}
	}
	update(&meta)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task.meta: %w", err)
	}
	return utils.AtomicWriteFile(TaskMetaPath(dir), data, 0o644)
}

// Write creates <dir>/checkpoints/, writes params.json, and (when wandb is
// configured at project level) writes wandb_config.json. When note is non-empty
// it also writes note.txt. All writes are atomic via utils.AtomicWriteFile so
// the Python SDK never observes a half-written file.
func Write(dir string, params job.TaskParams, wandb *project.WandbConfig, note string) error {
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

	if note != "" {
		if err := utils.AtomicWriteFile(NotePath(dir), []byte(note), 0o644); err != nil {
			return fmt.Errorf("write note.txt: %w", err)
		}
	}
	return nil
}

// WriteFS is Write through an explicit filesystem — the remote-target path:
// task workspaces live on the TARGET's filesystem (rfs.SSHFS), not the
// client's. nil falls back to the local, atomic-write implementation.
//
// Remote note: sftp has no atomic rename-write. Ordering makes this safe
// anyway — everything here lands BEFORE the task is submitted, so the SDK on
// the compute node can never observe a half-written file.
func WriteFS(fsys rfs.FS, dir string, params job.TaskParams, wandb *project.WandbConfig, note string) error {
	if fsys == nil {
		return Write(dir, params, wandb, note)
	}
	if err := fsys.MkdirAll(CheckpointsDir(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	paramsJSON, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	if err := fsys.WriteFile(ParamsPath(dir), paramsJSON, 0o644); err != nil {
		return fmt.Errorf("write params.json: %w", err)
	}

	if wandb != nil {
		wandbJSON, err := json.MarshalIndent(wandb, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal wandb config: %w", err)
		}
		if err := fsys.WriteFile(WandbConfigPath(dir), wandbJSON, 0o644); err != nil {
			return fmt.Errorf("write wandb_config.json: %w", err)
		}
	}

	if note != "" {
		if err := fsys.WriteFile(NotePath(dir), []byte(note), 0o644); err != nil {
			return fmt.Errorf("write note.txt: %w", err)
		}
	}
	return nil
}

// JobDirName names a job's workspace directory: "<sanitized-note>-<jobID>"
// so an `ls` of .runq/ reads like an experiment log, falling back to the
// bare id when there is no note. The DB always stores full paths — this
// name is decoration for humans, never re-derived from the id.
func JobDirName(note, jobID string) string {
	s := sanitizeDirComponent(note)
	if s == "" {
		return jobID
	}
	return s + "-" + jobID
}

// sanitizeDirComponent keeps [A-Za-z0-9._-], maps the rest to '-',
// collapses runs, trims separators, caps at 40 chars (paths must stay
// pleasant to type and safe on every shared FS).
func sanitizeDirComponent(s string) string {
	var b []rune
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	out := string(b)
	for len(out) > 0 && (out[0] == '-' || out[0] == '.') {
		out = out[1:]
	}
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-.")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-.")
	}
	return out
}
