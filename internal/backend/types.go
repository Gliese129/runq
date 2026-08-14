package backend

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/preflight"
)

// View types shared by HTTP responses and CLI --json output.
// Keep this file free of business logic — only struct definitions.

// PreviewResult is what PreviewSubmit returns: the human-readable dry-run
// text plus the STRUCTURED preflight report (RQ-76 ②). The structure is
// what lets the WebUI act on findings — e.g. one-click insertion of an
// HF pre-download command into the project's setup command — instead of
// re-parsing the text blob.
type PreviewResult struct {
	Preview   string           `json:"preview"`
	Preflight preflight.Report `json:"preflight"`
}

type JobSummary struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Note    string `json:"note"`
	// Status: pending / running / paused while live; done / failed /
	// partial / killed once terminal (see store.TerminalJobStatus).
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
	// RQ-74: outcome-unknown submissions awaiting reconcile. Rendered as its
	// own segment so a job with unknown tasks never looks silently done.
	Unknown int `json:"unknown,omitempty"`
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
	// DataDir is the job's workspace directory (RQ2-1 §F: the Data tab's
	// fs/list root). ABSOLUTE path with TARGET semantics — it only means
	// something on the filesystem of job.target; pair it with that target
	// when browsing. Derived from the recorded task dirs (submit-time
	// fact, not re-derived config); archive never moves directories, so
	// it stays valid for archived jobs. Omitted when no task recorded a
	// dir (pre-L2C jobs, zero-task jobs) — the Data tab shows its empty
	// state, never an error.
	DataDir string `json:"data_dir,omitempty"`
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
	// RQ-74: verbatim failure evidence for pre-run failures (submit
	// rejection stderr + exit code + rendered command; local spawn errors).
	// Present on both lanes; empty once a task reached the run phase.
	FailureDetail string `json:"failure_detail,omitempty"`
	// Phase 2D: scheduler-native state token (e.g. "CONFIGURING") and
	// queue/partition name. Only populated in poll-model backends.
	NativeState string `json:"native_state,omitempty"`
	Queue       string `json:"queue,omitempty"`
	// LogPath is the filesystem path to the task's log file. Omitted from
	// JSON list responses; populated by GetTask for log-streaming endpoints.
	LogPath string `json:"log_path,omitempty"`
	// RQ2-1 §G: execution facts for the task page's Execution KV —
	// detail-only (populated by GetTask, never in list responses; row-level
	// consumers don't need them and lists stay lean). Straight DTO
	// exposure of store columns; secrets travel via .env/prelude, never
	// the command text.
	Command    string `json:"command,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
	TaskDir    string `json:"task_dir,omitempty"`
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
	// ConfigGeneration: config.yaml's semantic content hash (RQ-75) at the
	// time of this response — freshly computed, not the boot snapshot's.
	ConfigGeneration string `json:"config_generation,omitempty"`
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

// RefreshReceipt is the D22 refresh response: the caller ALWAYS learns
// whether the refresh actually happened and, if not, why and when to
// retry. refreshed_at is the persisted photo timestamp (sync_state), not
// response time.
type RefreshReceipt struct {
	RefreshedAt       int64  `json:"refreshed_at"` // unix; 0 = never synced
	Refreshed         bool   `json:"refreshed"`
	Reason            string `json:"reason,omitempty"` // min_interval | timeout | <sync error>
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
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

// ActivityPoint is one activity.tsv row: cumulative bytes/lines at ts
// (the sidecar appends one row per 60s tick). Lines is nil for legacy
// 2-column files — the frontend curve and log seek both need real line
// counts, and bytes cannot honestly stand in for them (the ratio
// depends on log line width).
type ActivityPoint struct {
	TS    int64  `json:"ts"`
	Bytes int64  `json:"bytes"`
	Lines *int64 `json:"lines"`
}

// TaskActivity is one task's decimated activity series.
type TaskActivity struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	// BucketMin: minutes per point after owning-side decimation (1 =
	// raw 60s rows). Stride sampling of CUMULATIVE columns is lossless
	// coarsening — the delta between kept rows equals the sum of the
	// dropped intervals' deltas exactly, so a burst cannot hide in a
	// dropped row. (Arbitrary value series lack this property; that is
	// why task metrics need the pyramid and activity does not.)
	BucketMin int             `json:"bucket_minutes"`
	Points    []ActivityPoint `json:"points"`
}

// JobActivity is the /jobs/{id}/activity response: every task's series
// plus the job's wall-clock window. JobEnd is nil while the job is
// live — the frontend draws the axis to now.
type JobActivity struct {
	Tasks    []TaskActivity `json:"tasks"`
	JobStart int64          `json:"job_start"`
	JobEnd   *int64         `json:"job_end,omitempty"`
}

// ── /jobs/{id}/results wire (RQ2-1 §A: columnar, record-index dimension) ──
//
// Every record from runq.record shares ONE dimension: its index in the
// sorted sequence. All columns are equally long; axes co-occurrence is
// encoded by the shared index. The backend sorts by (identity, task,
// primary x) so identity groups AND per-task runs are contiguous ranges,
// handed to the consumer as slice indices — no client-side scanning.

// JobResults is the GET /jobs/{id}/results response.
type JobResults struct {
	// Source describes the record contract, a constant — NOT a file path
	// (zero path inference; see ResultSource).
	Source string `json:"source"`
	// Parsed = records returned; Skipped = records dropped by the ingest
	// cap (Σ file_ingest.dropped_count over the job's tasks — known loss);
	// Truncated = Skipped > 0. Malformed lines are warn-logged at ingest
	// and not counted here.
	Parsed    int   `json:"parsed"`
	Skipped   int64 `json:"skipped"`
	Truncated bool  `json:"truncated"`
	// UpdatedAt is the newest record's ts (0 when empty).
	UpdatedAt int64        `json:"updated_at"`
	N         int          `json:"n"`
	Schema    ResultSchema `json:"schema"`
	Cols      ResultCols   `json:"cols"`
}

// ResultSchema is the "smart parse" product: key classification, group and
// task ranges, vocab dictionaries. All ranges index into Cols.
type ResultSchema struct {
	// Groups are identity runs (model value, task_id fallback) — the
	// series dimension, contiguous by construction.
	Groups []ResultRange `json:"groups"`
	// Tasks are contiguous per-task runs. NOT necessarily one entry per
	// task: a task that recorded several models is split across identity
	// groups and contributes one entry per run.
	Tasks []ResultRange         `json:"tasks"`
	Axes  map[string]ResultAxis `json:"axes"`
	// XAxes are the x-candidate axis names in first-appearance order
	// (first = primary, the sort key). Kept as an array because the Axes
	// map carries no order.
	XAxes   []string `json:"x_axes"`
	Metrics []string `json:"metrics"`
}

// ResultRange is one contiguous [Offset, Offset+Count) slice of the record
// sequence. Key carries the identity value for groups; ID the task id for
// task runs (exactly one of the two is set per usage).
type ResultRange struct {
	Key    string `json:"key,omitempty"`
	ID     string `json:"id,omitempty"`
	Offset int    `json:"offset"`
	Count  int    `json:"count"`
}

// ResultAxis classifies one axis key.
type ResultAxis struct {
	Type string `json:"type"` // "num" | "str" | "bool"
	Role string `json:"role"` // "identity" | "x" | "label"
	// Vocab dictionary-encodes str axes: the column holds indices into it.
	Vocab []string `json:"vocab,omitempty"`
	// Nulled counts values nulled by type conflict (mixed-type axis:
	// majority type wins, minority values → null) or non-scalar values.
	// The warning travels WITH the data instead of hiding in a log.
	Nulled int `json:"nulled,omitempty"`
}

// ResultCols holds the equally-long columns. Axis columns carry float64 /
// bool / vocab index (int) / nil per the axis type; metric columns are
// numbers with nil holes for records that didn't report the metric.
type ResultCols struct {
	TS      []int64               `json:"ts"`
	Axes    map[string][]any      `json:"axes"`
	Metrics map[string][]*float64 `json:"metrics"`
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

// CleanAction DESCRIBES what deletion will touch — it is not a user
// choice. Deletion is all-or-nothing by design (task dir + DB record,
// irreversible); partial cleanup was a knob nobody used and everyone
// feared — users who want surgery cd into the dir themselves.
type CleanAction string

const (
	CleanActionAll    CleanAction = "all"     // delete DB record + all files
	CleanActionDBOnly CleanAction = "db_only" // delete DB record (no files on disk)
)

type CleanPreviewItem struct {
	TaskID     string      `json:"task_id"`
	JobID      string      `json:"job_id"`
	Project    string      `json:"project"`
	Status     string      `json:"status"`
	FinishedAt *int64      `json:"finished_at,omitempty"`
	TaskDir    string      `json:"task_dir,omitempty"`
	Reason     string      `json:"reason"` // why selected: orphan / archived / job / task / older-than
	Action     CleanAction `json:"action"` // what will be cleaned
	Orphan     bool        `json:"orphan"` // true if task dir is missing from disk

	// Size preview — from the LEDGER, not the filesystem: checkpoint stats
	// come from the checkpoints table, metrics size from the ingest mark.
	// Zero SSH round trips at selection time; the FS is only touched after
	// the user confirms.
	CkptFiles    int   `json:"ckpt_files"`
	CkptBytes    int64 `json:"ckpt_bytes"`
	MetricsBytes int64 `json:"metrics_bytes"`
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
	// CurrentGeneration accompanies CodeGenerationConflict (RQ-75): the
	// file's generation as it exists NOW, so the client can re-read, show a
	// diff against the user's pending edit, and retry with a fresh If-Match.
	CurrentGeneration string `json:"current_generation,omitempty"`
}

// v1 error codes (spec §2) × HTTP status. Keep in sync with the protocol
// spec — additions must be reflected there first (spec-first).
const (
	CodeBadRequest        = "bad_request"        // 400
	CodeNotFound          = "not_found"          // 404
	CodeInvalidState      = "invalid_state"      // 409: action illegal in current state
	CodeNotSupported      = "not_supported"      // 409: target lacks the capability
	CodeNotImplemented    = "not_implemented"    // 501: spec-first stub, pending implementation
	CodeForbidden         = "forbidden"          // 403: refused by the remote-CLI forward guard (RQ-45)
	CodeTargetUnreachable = "target_unreachable" // 502
	CodeMinInterval       = "min_interval"       // 429: refresh blocked by the 5min floor
	CodeInternal          = "internal"           // 500
	// 409: If-Match generation mismatch — the config file changed since the
	// client read it (RQ-75). Response carries current_generation.
	CodeGenerationConflict = "generation_conflict"
	// 409: submit reached a RETIRED lane generation (best-effort gate —
	// routing already sends new work to the active lane; this catches a
	// straggler holding the old pointer). Retry immediately: it lands on
	// the active lane. A request that slips past the gate still runs
	// correctly under the retired lane's config snapshot.
	CodeLaneRetired = "lane_retired"
	// 409: rerun of a task whose target config CHANGED since submission
	// (RQ-75) — needs explicit confirmation (WebUI dialog / CLI y/N or -y).
	CodeGenerationChanged = "generation_changed"
)

// TargetGenerationView is one retired/retiring target generation for the
// CLI/WebUI archive display (RQ-75): same-name generations render as
// sub-rows of their active target; generations of REMOVED targets go to
// the collapsed archived section.
type TargetGenerationView struct {
	Target     string `json:"target"`
	Generation string `json:"generation"`
	Reason     string `json:"reason"` // changed | removed
	RetiredAt  int64  `json:"retired_at"`
	DoneAt     *int64 `json:"done_at,omitempty"` // nil = still retiring
	Unfinished int    `json:"unfinished"`        // tasks it still tracks
}

// ErrLaneRetired is the intake gate of a retired lane generation (round
// 7): routing already sends new work to the active lane, so hitting this
// means the caller held a stale pointer — an immediate retry succeeds.
var ErrLaneRetired = errors.New("lane generation retired")

// GenerationChangedError refuses an UNCONFIRMED rerun of a task whose
// target config changed since it was submitted (RQ-75): the rerun will use
// the NEW config, and the human should know before it does. Confirmed
// retries restamp the task to the active generation and proceed.
type GenerationChangedError struct {
	TaskGeneration   string
	ActiveGeneration string
}

func (e *GenerationChangedError) Error() string {
	return "target config changed since this task was submitted — rerun will use the NEW config (confirm to proceed)"
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
	// Style "flag" = argparse store_true switch: bare `--name` when true,
	// omitted when false (never `--name=false`).
	Style string `json:"style,omitempty"`
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
