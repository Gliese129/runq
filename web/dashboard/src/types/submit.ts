import type { InjectionKey } from 'vue'
import type { ProjectSummary } from '@/types/api'
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
}

export interface ParamMeta {
  min?: number
  max?: number
  step?: number
}

export interface SubmitState {
  step: number
  projectName: string
  matchedProjects: ProjectSummary[]
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
    creating: boolean
    error: string
    params: ProjectParam[]
    // True once the user actually edited project config in this session.
    // goNext only saves the project when dirty (or when creating) — selecting
    // an existing project and clicking Next is side-effect free.
    dirty: boolean
  }
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
  submitting: boolean
  preflightEnabled: boolean
  prefs: any
}

export const SUBMIT_STATE_KEY: InjectionKey<SubmitState> = Symbol('submit-state')
