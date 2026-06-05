import type { InjectionKey } from 'vue'
import type { ProjectSummary } from '@/types/api'

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
}

export interface ParamMeta {
  min?: number
  max?: number
  step?: number
}

export interface GroupParam {
  name: string
  type: string            // int | float | bool | str | file | folder | list
  default: string
  values: string[]
  meta?: ParamMeta
}

export interface SweepGroup {
  id: string
  type: 'grid' | 'list'
  expanded: boolean
  params: GroupParam[]
}

export interface SubmitState {
  step: number
  projectName: string
  matchedProjects: ProjectSummary[]
  newProject: {
    name: string
    workDir: string
    cmd: string
    gpus: number
    maxRetry: number
    envType: string
    envPath: string
    envName: string
    creating: boolean
    error: string
    params: ProjectParam[]
  }
  note: string
  groups: SweepGroup[]
  groupIdCounter: number
  usedParamNames: Set<string>
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
  groupTaskCount: (group: SweepGroup) => number
  getNextGroupId: () => number
}

export const SUBMIT_STATE_KEY: InjectionKey<SubmitState> = Symbol('submit-state')
