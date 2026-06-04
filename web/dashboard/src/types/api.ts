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
}

export interface TaskView {
  id: string
  status: string
  params: Record<string, any>
  current_step?: number
  started_at?: number
  finished_at?: number
  elapsed_seconds?: number
  exit_code?: number
  retry_count: number
  wandb_run_id?: string
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

export interface FeatureFlags {
  gpu_map: boolean
  pause_resume: boolean
}

export interface ConfigResponse {
  mode: string
  data_path: string
  config_path: string
  features: FeatureFlags
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
}

export interface WebhookConfig {
  url: string
  events: string[]
}
