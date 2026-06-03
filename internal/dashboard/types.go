package dashboard

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
	Job        JobSummary `json:"job"`
	Tasks      []TaskView `json:"tasks"`
	MetricKeys []string   `json:"metric_keys"`
	Wandb      *WandbInfo `json:"wandb,omitempty"`
}

type TaskView struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Params      map[string]any `json:"params"`
	CurrentStep *int           `json:"current_step,omitempty"`
	StartedAt   *int64         `json:"started_at,omitempty"`
	FinishedAt  *int64         `json:"finished_at,omitempty"`
	ElapsedSec  *float64       `json:"elapsed_seconds,omitempty"`
	ExitCode    *int           `json:"exit_code,omitempty"`
	RetryCount  int            `json:"retry_count"`
	WandbRunID  string         `json:"wandb_run_id,omitempty"`
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
	Mode       string       `json:"mode"`
	DataPath   string       `json:"data_path"`
	ConfigPath string       `json:"config_path"`
	Features   FeatureFlags `json:"features"`
}

type FeatureFlags struct {
	GPUMap      bool `json:"gpu_map"`
	PauseResume bool `json:"pause_resume"`
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
