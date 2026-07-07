package backend

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
)

// View types shared by HTTP responses and CLI --json output.
// Keep this file free of business logic — only struct definitions.

type JobSummary struct {
	ID        string         `json:"id"`
	Project   string         `json:"project"`
	Note      string         `json:"note"`
	Status    string         `json:"status"`
	Target    string         `json:"target"`
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
	// LogPath is the filesystem path to the task's log file. Omitted from
	// JSON list responses; populated by GetTask for log-streaming endpoints.
	LogPath string `json:"log_path,omitempty"`
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
	// Target is the compute target these GPUs belong to — the aggregated
	// dashboard panel (local ∪ remote) groups by it. Stamped by
	// MultiBackend during aggregation.
	Target string `json:"target,omitempty"`
}

type ActionResponse struct {
	OK bool `json:"ok"`
}

// ConfigResponse is the v1 bootstrap summary (spec §4): the frontend pulls
// it once at startup — paths, default target, and each target's
// type/capabilities. NOT the management view (/targets has full
// TargetConfig fields). mode is gone from the wire (D5/D9).
type ConfigResponse struct {
	DataPath      string          `json:"data_path"`
	ConfigPath    string          `json:"config_path"`
	DefaultTarget string          `json:"default_target"`
	Targets       []TargetSummary `json:"targets"`
}

// TargetSummary is one target's bootstrap entry (spec §4): identity +
// capability bits, no scheduler template details. Type is inferred from
// config fields (no ssh/scheduler → local), matching state_model 1:1.
type TargetSummary struct {
	Name         string       `json:"name"`
	Type         string       `json:"type"`      // "local" | "remote"
	Scheduler    string       `json:"scheduler"` // slurm | pbs | ... | runq | ""
	Capabilities Capabilities `json:"capabilities"`
	// Cache TTLs (spec §3) land with the cache layer (L4).
}

// TargetHealth is one row of GET /health's targets[] (spec §5.1, D6):
// PASSIVE reachability — filled from the most recent sync/interaction
// outcome, never an active probe.
type TargetHealth struct {
	Name        string `json:"name"`
	Reachable   bool   `json:"reachable"`
	LastError   string `json:"last_error,omitempty"`
	LastChecked int64  `json:"last_checked"` // unix; 0 = no contact yet
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
	Orphan   bool   `json:"orphan"`   // tasks MARKED orphaned (rfs.FS detection, see remote.DetectOrphans)
	Archived bool   `json:"archived"` // tasks belonging to archived jobs
	JobID    string `json:"job_id"`   // specific job
	TaskID   string `json:"task_id"`  // specific task
	// TaskIDs selects an exact set of tasks — the execute phase of the
	// interactive clean flow (user multi-selected from the preview).
	TaskIDs []string `json:"task_ids,omitempty"`
	// Target scopes the clean to a single compute target. Empty = all targets.
	Target string `json:"target,omitempty"`
	// Time filter — optional when other selectors are present.
	OlderThan *time.Time `json:"older_than,omitempty"` // only tasks finished before this time
	// Partial cleanup: only delete checkpoints/, keep DB + other artifacts.
	CkptOnly bool `json:"ckpt_only"`
	// DryRun: preview what would be cleaned without deleting.
	DryRun bool `json:"dry_run"`
}

// CleanResult reports what Clean did (or would do in dry-run mode).
type CleanResult struct {
	Tasks      int                `json:"tasks"`
	Jobs       int                `json:"jobs"`
	FreedBytes int64              `json:"freed_bytes"`
	Preview    []CleanPreviewItem `json:"preview,omitempty"` // populated only in dry-run
}

// TaskListOptions filters the flat task table (spec §5.5). Zero values
// mean "no filter"; Limit 0 means unpaginated.
type TaskListOptions struct {
	JobID  string
	Status string
	Target string
	Limit  int
	Offset int
}

// LogMatch is one grep hit from JobLogSearch: which task's log, where, and
// the matching line (owning-side grep — results travel, files don't).
type LogMatch struct {
	TaskID string `json:"task_id"`
	Line   int    `json:"line"`
	Text   string `json:"text"`
}

// LogPage is one page of a task log: byte-anchored, line-quantified. The
// log-paging vocabulary is owned by the logfile package (single domain,
// single spelling); this alias only re-exports it for Backend signatures.
type LogPage = logfile.Page

// LogFollower is a pull-based LogPage iterator over a growing log — the
// return type of TaskLogFollow. Content arrives as pages (stripped lines +
// byte anchors), never raw bytes, so consumers inherit the positional read
// path's clamping/stripping instead of re-implementing it.
// *logfile.Follower satisfies this natively — no adapter.
//
// Next blocks until new data is available, then returns one page and
// advances. It returns promptly with ctx.Err() on cancellation. After a
// rotation the next page's Offset is 0 (view reset signal). Next never
// returns an empty page: it waits instead.
//
// Close releases the underlying file handle and must be idempotent; a
// blocked Next must not deadlock Close.
type LogFollower interface {
	Next(ctx context.Context) (*LogPage, error)
	Close() error
}

// QueueEntry is one row of the squeue output (runq preset): a non-terminal
// task's id and status, in runq's own status vocabulary (which doubles as
// remote.ParseSignal's canonical vocabulary — no SignalMap needed).
type QueueEntry struct {
	ID     string `json:"id"`
	Status string `json:"status"`
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
	FinishedAt *int64      `json:"finished_at,omitempty"`
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

// ErrorResponse is the v1 error envelope (spec §2): Error is for humans,
// Code is the stable machine enum — CLI/WebUI branch ONLY on Code.
type ErrorResponse struct {
	Error             string `json:"error"`
	Code              string `json:"code"`
	Details           string `json:"details,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// v1 error codes (spec §2) × HTTP status. Keep in sync with the protocol
// spec — additions must be reflected there first (spec-first).
const (
	CodeBadRequest        = "bad_request"        // 400
	CodeNotFound          = "not_found"          // 404
	CodeInvalidState      = "invalid_state"      // 409: action illegal in current state
	CodeNotSupported      = "not_supported"      // 409: target lacks the capability
	CodeNotImplemented    = "not_implemented"    // 501: spec-first stub, pending implementation
	CodeTargetUnreachable = "target_unreachable" // 502
	CodeMinInterval       = "min_interval"       // 429: refresh blocked by the 5min floor
	CodeInternal          = "internal"           // 500
)

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
