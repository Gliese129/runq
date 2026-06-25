package backend

import (
	"encoding/json"
	"time"

	"github.com/gliese129/runq/internal/job"
)

// View types shared by HTTP responses and CLI --json output.
// Keep this file free of business logic — only struct definitions.

type JobSummary struct {
	ID        string         `json:"id"`
	Project   string         `json:"project"`
	Note      string         `json:"note"`
	Status    string         `json:"status"`
	Archived  bool           `json:"archived"`
	CreatedAt int64          `json:"created_at"`
	Tasks     TaskCountGroup `json:"tasks"`
	ETASec    *int64         `json:"eta_seconds,omitempty"`
	// RefreshedAt: last reconcile from external sources (poll-model backends
	// only). Omitted in daemon mode — push state is always current.
	RefreshedAt *int64 `json:"refreshed_at,omitempty"`
}

type TaskCountGroup struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type WandbInfo struct {
	Entity  string `json:"entity,omitempty"`
	Project string `json:"project,omitempty"`
	BaseURL string `json:"base_url"`
}

type JobDetail struct {
	Job        JobSummary      `json:"job"`
	Tasks      []TaskView      `json:"tasks"`
	MetricKeys []string        `json:"metric_keys"`
	Wandb      *WandbInfo      `json:"wandb,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"` // raw JobConfig (re-run as template)
}

type TaskView struct {
	ID          string             `json:"id"`
	Status      string             `json:"status"`
	Params      map[string]any     `json:"params"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	CurrentStep *int               `json:"current_step,omitempty"`
	StartedAt   *int64             `json:"started_at,omitempty"`
	FinishedAt  *int64             `json:"finished_at,omitempty"`
	ElapsedSec  *float64           `json:"elapsed_seconds,omitempty"`
	ExitCode    *int               `json:"exit_code,omitempty"`
	RetryCount  int                `json:"retry_count"`
	MaxRetry    int                `json:"max_retry"`
	GPUs        string             `json:"gpus,omitempty"`
	WandbRunID  string             `json:"wandb_run_id,omitempty"`
	// HPC-specific; zero-valued in daemon mode. Consumers check
	// Capabilities.StateModel == "poll" before rendering.
	ExternalID   string `json:"external_id,omitempty"`
	StatusSource string `json:"status_source,omitempty"`
	// Phase 2D: scheduler-native state token (e.g. "CONFIGURING") and
	// queue/partition name. Only populated in poll-model backends.
	NativeState string `json:"native_state,omitempty"`
	Queue       string `json:"queue,omitempty"`
}

type MetricPoint struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Step  *int    `json:"step,omitempty"`
	TS    int64   `json:"ts"`
}

type CompareRow struct {
	TaskID   string         `json:"task_id"`
	Status   string         `json:"status,omitempty"`
	Params   map[string]any `json:"params"`
	Best     float64        `json:"best"`
	HasValue bool           `json:"has_value"`
	Rank     int            `json:"rank"`
}

type GPUSlot struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	MemTotalMB  int    `json:"mem_total_mb"`
	MemUsedMB   int    `json:"mem_used_mb"`
	UtilPercent int    `json:"util_percent"`
	TaskID      string `json:"task_id,omitempty"`
	JobID       string `json:"job_id,omitempty"`
}

type ActionResponse struct {
	OK bool `json:"ok"`
}

type ConfigResponse struct {
	Mode         string       `json:"mode"`
	DataPath     string       `json:"data_path"`
	ConfigPath   string       `json:"config_path"`
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities is each backend's self-description, in three dimensions
// (design philosophy #2: capabilities are declared facts, not inferences
// from mode). The server forwards it verbatim; the GUI consumes all of it
// (unsupported concepts are not rendered), the CLI mostly ignores it and
// only relays it under --json.
type Capabilities struct {
	// Feature bits — "does this concept exist here at all".
	GPUMap      bool `json:"gpu_map"`
	PauseResume bool `json:"pause_resume"`
	LiveLog     bool `json:"live_log"`
	Retry       bool `json:"retry"`
	// State model — "push": a resident process owns the truth, data is
	// always current. "poll": best-effort projection, only advances on
	// refresh; consumers must surface staleness (refreshed_at).
	StateModel string `json:"state_model"`
	// Action semantics — kill is forwarded to an external scheduler (e.g.
	// qdel/scancel) rather than signalling a local process. The task is
	// marked "killed" locally as soon as the cancel command itself succeeds;
	// it is never marked killed unless that command returned success (the
	// kill never lies). UIs may show a transient local "cancelling" state for
	// the duration of the request — it clears on the next reconcile, which
	// usually already shows "killed".
	KillAsync bool `json:"kill_async"`
	// SubmitPreview — the backend can render exactly what WOULD be
	// submitted (run.sh + submit command) with zero side effects.
	SubmitPreview bool `json:"submit_preview"`
	// P6 features: log activity heatmap and cross-task log search.
	ActivityHeatmap bool `json:"activity_heatmap"`
	LogSearch       bool `json:"log_search"`
}

// CleanOptions controls which tasks the clean command targets.
// At least one selector must be set; --older-than acts as an additional filter
// when combined with other selectors.
type CleanOptions struct {
	// Selectors — at least one must be true/non-empty.
	Orphan   bool   // detect tasks whose taskDir is missing (on-demand os.Stat)
	Archived bool   // tasks belonging to archived jobs
	JobID    string // specific job
	TaskID   string // specific task
	// Time filter — optional when other selectors are present.
	OlderThan *time.Time // only tasks finished before this time
	// Partial cleanup: only delete checkpoints/, keep DB + other artifacts.
	CkptOnly bool
	// DryRun: preview what would be cleaned without deleting.
	DryRun bool
}

// CleanResult reports what Clean did (or would do in dry-run mode).
type CleanResult struct {
	Tasks      int                `json:"tasks"`
	Jobs       int                `json:"jobs"`
	FreedBytes int64              `json:"freed_bytes"`
	Preview    []CleanPreviewItem `json:"preview,omitempty"` // populated only in dry-run
}

// CleanAction describes what kind of cleanup will happen for a task.
type CleanAction string

const (
	CleanActionAll    CleanAction = "all"     // delete DB record + all files
	CleanActionDBOnly CleanAction = "db_only" // delete DB record (no files on disk)
	CleanActionCkpt   CleanAction = "ckpt"    // delete checkpoints/ only
	CleanActionCkptDB CleanAction = "no_ckpt" // ckpt-only requested but no checkpoints/ dir
)

type CleanPreviewItem struct {
	TaskID     string      `json:"task_id"`
	JobID      string      `json:"job_id"`
	Status     string      `json:"status"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	TaskDir    string      `json:"task_dir,omitempty"`
	Reason     string      `json:"reason"` // why selected: orphan / archived / job / task / older-than
	Action     CleanAction `json:"action"` // what will be cleaned
	Orphan     bool        `json:"orphan"` // true if task dir is missing from disk
}

// DryRunResult is what DryRun returns: expanded tasks plus best-effort
// preview info (command rendering and prospective workspace root).
// SampleCommand and WorkspaceRoot may be empty when the project has no
// command template or the config cannot be loaded.
type DryRunResult struct {
	Tasks         []job.TaskParams `json:"tasks"`
	SampleCommand string           `json:"sample_command,omitempty"`
	WorkspaceRoot string           `json:"workspace_root,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// Project summary for sidebar.

type ProjectSummary struct {
	Name     string `json:"name"`
	WorkDir  string `json:"work_dir,omitempty"`
	JobCount int    `json:"job_count"`
	Archived bool   `json:"archived"`
}

// Filesystem types for init GUI.

type FSEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type ParseScriptRequest struct {
	Path string `json:"path"`
}

type ParseResult struct {
	Args []ScriptArg `json:"args"`
	Env  string      `json:"detected_env,omitempty"`
	Cmd  string      `json:"suggested_command"`
}

type ScriptArg struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	Default *string `json:"default,omitempty"`
}

// ── Thaw types ──

// ThawResponse is the structured result of a thaw operation.
// Thawed lists task IDs that were successfully resumed.
// Blocked entries carry the mount, free/threshold bytes, and co-tenants
// so the user knows whose checkpoints to clean up.
type ThawResponse struct {
	Thawed  []string                 `json:"thawed"`
	Blocked map[string]BlockedDetail `json:"blocked,omitempty"`
}

// BlockedDetail enriches a blocked-task entry with per-mount info.
type BlockedDetail struct {
	Mount     string      `json:"mount"`
	FreeBytes int64       `json:"free_bytes"` // -1 if disk.Usage failed
	Threshold int64       `json:"threshold"`  // per-task NeededBytes
	DiskUsers []MountMate `json:"disk_users,omitempty"`
}

// MountMate describes another running task sharing the same mount as a
// blocked task — sorted by total ckpt bytes desc so the disk-hog stands
// out.
type MountMate struct {
	TaskID          string `json:"task_id"`
	User            string `json:"user"`
	JobID           string `json:"job_id"`
	LatestCkptBytes int64  `json:"latest_ckpt_bytes"`
	TotalCkptBytes  int64  `json:"total_ckpt_bytes"`
}
