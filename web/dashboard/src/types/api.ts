// Mirrors Go view types in internal/dashboard/types.go

export interface TaskCountGroup {
  total: number
  pending: number
  running: number
  completed: number
  failed: number
}

export interface JobSummary {
  archived: boolean
  id: string
  project: string
  note: string
  status: string
  created_at: number
  tasks: TaskCountGroup
  eta_seconds?: number
  /** last reconcile from external sources — poll-model backends only */
  refreshed_at?: number
}

/** Raw job config as submitted (note keeps its {{...}} template form). */
export interface RawJobConfig {
  project: string
  note?: string
  name?: string
  fixed_params?: Record<string, any>
  sweep?: Array<{ method: string; parameters: Record<string, { values: any[] } | any[]> }>
}

export interface TaskView {
  id: string
  status: string
  params: Record<string, any>
  metrics?: Record<string, number>
  current_step?: number
  started_at?: number
  finished_at?: number
  elapsed_seconds?: number
  exit_code?: number
  retry_count: number
  max_retry: number
  gpus?: string
  wandb_run_id?: string
  /** HPC-specific; empty in daemon mode. Check caps.state_model == 'poll'. */
  external_id?: string
  status_source?: string
  /** Raw scheduler state token before signal mapping (e.g. "COMPLETING", "R"). */
  native_state?: string
  /** Scheduler queue/partition name (e.g. "gpu-a100", "cpu-batch"). */
  queue?: string
}

/** Mirrors logfile.Page — byte-offset based log page. */
export interface LogPage {
  lines: string[]
  start_offset: number
  end_offset: number
  total_bytes: number
  total_lines: number // -1 when unknown
}

/** @deprecated Use LogPage. Kept for migration. */
export type TaskLogResponse = LogPage

export interface WandbInfo {
  entity?: string
  project?: string
  base_url: string
}

export interface JobDetail {
  job: JobSummary
  tasks: TaskView[]
  metric_keys: string[]
  wandb?: WandbInfo
  /** raw config as submitted — powers "re-run as template" */
  config?: RawJobConfig
}

export interface CompareRow {
  task_id: string
  status?: string
  params: Record<string, any>
  best: number
  /** false when the task has no metric value (distinguish 0 from absent) */
  has_value: boolean
  rank: number
}

export interface GPUSlot {
  index: number
  name: string
  mem_total_mb: number
  mem_used_mb: number
  util_percent: number
  task_id?: string
  job_id?: string
}

/**
 * Backend self-description in three dimensions (mirrors Go Capabilities).
 * Feature bits: concept exists here at all → render or remove (never disable).
 * state_model: "poll" → surface staleness (refreshed_at) + manual refresh.
 * kill_async: kill is forwarded to an external scheduler; the task is marked
 *   "killed" as soon as the cancel command succeeds. Show a local transient
 *   "cancelling" state for the request; it clears on the next reconcile.
 */
export interface Capabilities {
  gpu_map: boolean
  pause_resume: boolean
  live_log: boolean
  retry: boolean
  state_model: 'push' | 'poll'
  kill_async: boolean
  /** backend can render exactly what would be submitted (zero side effects) */
  submit_preview: boolean
  /** P6: log activity heatmap available */
  activity_heatmap: boolean
  /** P6: cross-task log search available */
  log_search: boolean
}

export interface ConfigResponse {
  mode: string
  data_path: string
  config_path: string
  capabilities: Capabilities
}

export interface ActionResponse {
  ok: boolean
}

export interface MessageResponse {
  message: string
}

export interface JobSubmitResponse {
  job_id: string
  total_tasks?: number
}

export interface JobConfigPayload {
  project: string
  note: string
  /** scheduler job name template override ({{name}} in submit_template) */
  name?: string
  fixed_params?: Record<string, any>
  sweep: Array<{
    method: string
    parameters: Record<string, { values: any[] }>
  }>
}

export interface FSEntry {
  name: string
  path: string
  is_dir: boolean
  size: number
}

export interface ParseResult {
  args: ScriptArg[]
  detected_env?: string
  suggested_command: string
}

export interface ScriptArg {
  name: string
  type: string
  default?: string
  choices?: string[]
  min?: number
  max?: number
}

export interface ProjectSummary {
  archived?: boolean
  name: string
  work_dir?: string
  job_count: number
}

export interface ProjectConfig {
  project_name: string
  working_dir: string
  command_template: string
  /** optional one-shot command before each submit (fixed params only) */
  setup_command?: string
  /** scheduler job name template ({{name}} in submit_template) */
  job_name?: string
  environment?: Record<string, string>
  defaults?: {
    gpus_per_task?: number
    max_retry?: number
    timeout?: string
  }
  resume?: {
    enabled: boolean
    extra_args?: string
  }
  python_env?: {
    type?: string
    path?: string
    name?: string
  }
  wandb?: {
    project: string
    entity?: string
    tags?: string[]
    mode?: string
  }
  params?: Array<{
    name: string
    type: string
    default?: string
    choices?: string[]
    min?: number
    max?: number
    /** user curation: appears in submit param table. absent = never curated */
    include?: boolean
    /** choices become a contract: out-of-list values fail at submit */
    strict?: boolean
    /** "scheduler" = consumed by submit_template only (never by the command) */
    scope?: string
  }>
}

export interface ProjectPayload {
  project_name: string
  working_dir: string
  command_template: string
  /** optional one-shot command before each submit (fixed params only) */
  setup_command?: string
  /** scheduler job name template ({{name}} in submit_template) */
  job_name?: string
  environment?: Record<string, string>
  defaults: {
    gpus_per_task: number
    max_retry: number
  }
  python_env?: {
    type: string
    path?: string
    name?: string
  }
  params?: Array<{
    name: string
    type: string
    default?: string
    choices?: string[]
    min?: number
    max?: number
    /** user curation: appears in submit param table. absent = never curated */
    include?: boolean
    /** choices become a contract: out-of-list values fail at submit */
    strict?: boolean
    /** "scheduler" = consumed by submit_template only (never by the command) */
    scope?: string
  }>
}

export interface WebhookConfig {
  url: string
  events: string[]
}

// ── P6: Activity + Search ──

export interface ActivityPoint {
  ts: number
  bytes: number
  lines: number
}

export interface TaskActivity {
  task_id: string
  status: string
  points: ActivityPoint[]
}

export interface JobActivityResponse {
  tasks: TaskActivity[]
  job_start?: number
  job_end?: number
}

export interface SearchMatch {
  task_id: string
  line_no: number
  offset: number
  line: string
}

export interface JobLogSearchResponse {
  matches: SearchMatch[]
  next_offset: number
  truncated: boolean
}
