// Mirrors Go view types in internal/backend/types.go (spec-first /api/v1).

/** Standard list wrapper for all v1 collection endpoints. */
export interface ListEnvelope<T> {
  items: T[]
  total?: number
  /** poll-model freshness metadata — surface staleness in the UI */
  refreshed_at?: number
  stale?: boolean
}

export interface TaskCountGroup {
  total: number
  pending: number
  running: number
  /** success tasks (the backend counts "success" as completed) */
  completed: number
  /** failed + killed tasks (the backend folds killed in here) */
  failed: number
  /** RQ-74: outcome-unknown submissions awaiting reconcile (absent when 0) */
  unknown?: number
}

export interface JobSummary {
  archived: boolean
  id: string
  project: string
  note: string
  /**
   * pending | running | paused while live; done | failed | partial | killed
   * once terminal (mirrors store.TerminalJobStatus — see statusGrammar for
   * the visual mapping)
   */
  status: string
  /** compute target this job runs on (multi-target model) */
  target: string
  created_at: number
  tasks: TaskCountGroup
  eta_seconds?: number
  /** last reconcile from external sources — poll-model backends only */
  refreshed_at?: number
}

/** Job-level resource overrides (mirrors job.Overrides). */
export interface JobOverrides {
  gpus_per_task?: number
  max_retry?: number
  timeout?: string
  env?: Record<string, string>
}

/**
 * Mirrors job.JobConfig — the ONE config shape for both directions:
 * read (JobDetail.config, re-run as template) and write (plan / preview /
 * submit bodies). The old RawJobConfig/JobConfigPayload split modeled the
 * same Go struct twice and drifted (missing description/overrides).
 * NOTE: over JSON the backend only accepts `{values: [...]}` parameter
 * specs (no UnmarshalJSON for bare arrays — that's YAML-only sugar).
 */
export interface JobConfigPayload {
  project: string
  description?: string
  note?: string
  /** scheduler job name template override ({{name}} in submit_template) */
  name?: string
  fixed_params?: Record<string, any>
  sweep: Array<{ method: string; parameters: Record<string, { values: any[] }> }>
  overrides?: JobOverrides
}

/** Read alias: raw config as submitted (note keeps its {{...}} form). */
export type RawJobConfig = JobConfigPayload

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
  /** RQ-74: verbatim pre-run failure evidence (submit rejection stderr + exit code + rendered command). */
  failure_detail?: string
  /** Raw scheduler state token before signal mapping (e.g. "COMPLETING", "R"). */
  native_state?: string
  /** Scheduler queue/partition name (e.g. "gpu-a100", "cpu-batch"). */
  queue?: string
  /** filesystem path to the task's log file — populated by GetTask only */
  log_path?: string
}

/** One metric sample (mirrors backend.MetricPoint). */
export interface MetricPoint {
  key: string
  value: number
  step?: number
  ts: number
}

/** GET /tasks/{id}/metrics without ?key= — live tail-window read. */
export interface TaskMetricsResponse {
  points: MetricPoint[]
  refreshed_at: number
}

/**
 * Mirrors logfile.Page — byte-offset based log page.
 * Field names follow the backend json tags EXACTLY: the previous
 * end_offset/total_bytes spellings didn't exist on the wire, so paging
 * guards compared undefined >= undefined and load-more silently degraded.
 */
export interface LogPage {
  lines: string[]
  /** byte offset this page starts at */
  offset: number
  /** byte offset to continue reading from */
  next_offset: number
  /** total size of the log file in bytes */
  size: number
  truncated?: boolean
  total_lines: number // -1 when unknown
  /** 0-based absolute line number of the page's first line; -1/absent = unknown */
  start_line?: number
  /** last entry is a fragment of a line longer than max_bytes (chain continues) */
  partial?: boolean
  /** first entry continues the previous page's unterminated last line */
  continues?: boolean
  /** requested offset was beyond the file size (rotation); page restarts at 0 */
  rotated?: boolean
}

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
  /** compute target these GPUs belong to (stamped during aggregation) */
  target?: string
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

/** One target's bootstrap entry: identity + capability bits (spec §4). */
export interface TargetSummary {
  name: string
  /** backend TargetConfig.Type() emits "local" | "remote" (scheduler/ssh set);
   *  its own doc comment says "remote" — keep all three until settled */
  type: 'local' | 'remote'
  /** slurm | pbs | ... | runq | "" (empty = direct execution) */
  scheduler: string
  capabilities: Capabilities
}

/**
 * GET /config — v1 bootstrap summary. `mode` is gone from the wire;
 * capabilities are declared per target.
 */
export interface ConfigResponse {
  data_path: string
  config_path: string
  default_target: string
  targets: TargetSummary[]
  /** config.yaml semantic content hash (RQ-75) — send back as If-Match on writes */
  config_generation?: string
}

/** POST /targets|jobs/{id}/refresh — D22: caller always learns the outcome. */
export interface RefreshReceipt {
  /** unix; 0 = never synced. Persisted photo timestamp, not response time. */
  refreshed_at: number
  refreshed: boolean
  /** min_interval | timeout | <sync error> */
  reason?: string
  retry_after_seconds?: number
}

/** One row of GET /health targets[] — passive reachability. */
export interface TargetHealth {
  name: string
  reachable: boolean
  last_error?: string
  /** unix; 0 = no contact yet */
  last_checked: number
}

/** One remote CLI forward's observable state (RQ-74, mirrors rfs.ForwardStatus). */
export interface ForwardStatus {
  state: 'up' | 'reconnecting' | 'closed'
  /** unix: when the current state was entered */
  since: number
  /** unix: last moment the forward was serving; absent = never online */
  last_online?: number
  /** consecutive failed sessions since last up */
  attempts?: number
  last_error?: string
}

export interface HealthResponse {
  version: string
  uptime_seconds: number
  targets: TargetHealth[]
  /** daemon identity — whose daemon answered (RQ-74) */
  hostname?: string
  /** per-target remote CLI forward state, keyed by target name (RQ-74) */
  forwards?: Record<string, ForwardStatus>
}

/** POST /jobs/plan — merged dry-run + resolve-note (single wizard call). */
export interface JobPlanResponse {
  tasks: Record<string, any>[]
  note_resolved: string
  warnings: string[]
}

/** One preflight check in the four-state grammar (passed/failed/warning/skipped). */
export interface PreflightCheck {
  name: string
  status: 'passed' | 'failed' | 'warning' | 'skipped'
  detail?: string
  /** Ready-made remediation commands (e.g. `huggingface-cli download …`). */
  commands?: string[]
}

/** Structured preflight report attached to POST /jobs/preview (RQ-76 ②). */
export interface PreflightReport {
  results?: PreflightCheck[]
  home_dir?: string
  python_prefix?: string
}

/** POST /jobs/preview — rendered dry-run text + structured preflight. */
export interface JobPreviewResponse {
  preview: string
  preflight?: PreflightReport
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

/** Backend parse-script emits ONLY these fields — choices/min/max
 *  belong to project.ParamDef, not to script parsing. */
export interface ScriptArg {
  name: string
  type: string
  default?: string
  /** "flag" = store_true switch (bare --name when true, omitted when false) */
  style?: string
}

export interface ProjectSummary {
  archived?: boolean
  name: string
  work_dir?: string
  job_count: number
}

export interface ProjectConfig {
  project_name: string
  /** compute target this project submits to (multi-target model) */
  target?: string
  working_dir: string
  command_template: string
  /** optional env file sourced before each task (backend: *string) */
  env_file?: string
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
    /** "flag" = store_true switch: rendered as bare --name / omitted */
    style?: string
  }>
}

/**
 * Write side uses the SAME shape as the read side (mirrors project.Config).
 * The old narrower ProjectPayload silently dropped every field the form
 * didn't edit (target / env_file / defaults.timeout / resume / wandb) on
 * save — writers must read-modify-write over the fetched ProjectConfig.
 */
export type ProjectPayload = ProjectConfig

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

/** Wire shape (spec §5.4): deliberately no byte offset — jumps go through
 *  log paging / pyramid raw ranges, not grep results. */
export interface SearchMatch {
  task_id: string
  line_no: number
  text: string
}

export interface JobLogSearchResponse {
  matches: SearchMatch[]
  next_offset: number
  truncated: boolean
}
