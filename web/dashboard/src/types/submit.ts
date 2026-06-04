import type { InjectionKey } from 'vue'
import type { ProjectSummary } from '@/types/api'

export interface GroupParam {
  name: string
  type: string
  default: string
  values: string[]
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
    envType: string        // 'venv' | 'conda' | 'uv' | 'system' | ''
    envPath: string        // venv/uv: relative dir
    envName: string        // conda: env name
    creating: boolean
    error: string
    params: Array<{ name: string; type: string; default: string; include: boolean }>
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
  prefs: any
  groupTaskCount: (group: SweepGroup) => number
  getNextGroupId: () => number
}

export const SUBMIT_STATE_KEY: InjectionKey<SubmitState> = Symbol('submit-state')
