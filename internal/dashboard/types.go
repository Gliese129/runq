package dashboard

import "encoding/json"

// View types shared by HTTP responses and CLI --json output.
// Keep this file free of business logic — only struct definitions.

type JobSummary struct {
	ID        string         `json:"id"`
	Project   string         `json:"project"`
	Note      string         `json:"note"`
	Status    string         `json:"status"`
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
}

type MetricPoint struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Step  *int    `json:"step,omitempty"`
	TS    int64   `json:"ts"`
}

type CompareRow struct {
	TaskID string         `json:"task_id"`
	Params map[string]any `json:"params"`
	Best   float64        `json:"best"`
	Rank   int            `json:"rank"`
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
	// Action semantics — kill is asynchronous: the request is forwarded
	// (e.g. qdel) and the status only changes once a later poll confirms
	// it. UIs should show a transient local "cancelling" state.
	KillAsync bool `json:"kill_async"`
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
