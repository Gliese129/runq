import type { InjectionKey } from 'vue'
import type { ProjectConfig, ProjectSummary } from '@/types/api'
import type { ParamRow, LinkSet } from '@/views/submit/paramTable'

// ── Parameter types ──
export const PARAM_TYPES = ['int', 'float', 'bool', 'str', 'file', 'folder', 'list'] as const
export type ParamType = (typeof PARAM_TYPES)[number]

export interface ProjectParam {
  name: string
  type: string            // ParamType
  default: string
  include: boolean
  min?: number            // int/float: valid range
  max?: number            // int/float: valid range
  values?: string[]       // str: selectable choices; list: items; file/folder: paths
  strict?: boolean        // choices are a contract, not suggestions
  scope?: string          // 'scheduler' = submit_template-only param
  style?: string          // 'flag' = store_true switch: bare --name / omitted
}

export interface ParamMeta {
  min?: number
  max?: number
  step?: number
}

export interface SubmitState {
  projectName: string
  matchedProjects: ProjectSummary[]
  // The SELECTED project's loaded config: param palette, job-name template,
  // setup command (HF suggestion append target), and the raw source for
  // read-modify-write saves. Project AUTHORING lives on the project-edit
  // page (RQ2-3 c1) — the submit flow never creates projects.
  newProject: {
    name: string
    workDir: string
    cmd: string
    setupCmd: string
    gpus: number
    maxRetry: number
    envType: string
    envPath: string
    envName: string
    envText: string        // KEY=VALUE per line — project environment
    jobName: string        // job_name template — scheduler job name default
    params: ProjectParam[]
    // The project.Config as fetched — buildProjectPayload read-modify-writes
    // over it so fields the form doesn't edit survive a save.
    source?: ProjectConfig
  }
  /** compute target this submit goes to (identity bar pill) */
  target: string
  note: string
  // Per-submit scheduler job name override; '' = use the project's
  // job_name template (or the rq-{{task_id}} default).
  jobName: string
  // Flat param model (see paramTable.ts): one row per param; sweep
  // structure is derived from value counts + link sets, never hand-built.
  rows: ParamRow[]
  linkSets: LinkSet[]
  totalTaskCount: number
  displayTaskCount: number
  sweepSummary: string
  dryRunResult: Record<string, any>[]
  dryRunLoading: boolean
  dryRunError: string
  dryRunHeaders: { title: string; key: string }[]
  /** backend-resolved note from POST /jobs/plan ({{version}} scanned) */
  noteResolved: string
  submitting: boolean
  preflightEnabled: boolean
  prefs: any
}

export const SUBMIT_STATE_KEY: InjectionKey<SubmitState> = Symbol('submit-state')
