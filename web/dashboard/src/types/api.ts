// Mirrors Go view types in internal/dashboard/types.go

export interface TaskCountGroup {
  total: number
  pending: number
  running: number
  completed: number
  failed: number
}

export interface JobSummary {
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
}

export interface TaskLogResponse {
  lines: string[]
  total_lines: number
  start: number
  end: number
  error?: string
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
}

export interface CompareRow {
  task_id: string
  params: Record<string, any>
  best: number
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
 * kill_async: show a local transient "cancelling" state after kill.
 */
export interface Capabilities {
  gpu_map: boolean
  pause_resume: boolean
  live_log: boolean
  retry: boolean
  state_model: 'push' | 'poll'
  kill_async: boolean
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
  name: string
  work_dir?: string
  job_count: number
}

export interface ProjectConfig {
  project_name: string
  working_dir: string
  command_template: string
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
  }>
}

export interface ProjectPayload {
  project_name: string
  working_dir: string
  command_template: string
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
  }>
}

export interface WebhookConfig {
  url: string
  events: string[]
}
