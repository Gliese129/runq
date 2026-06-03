import type { InjectionKey } from 'vue'
import type { FSEntry, ParseResult } from '@/types/api'

export interface ArgState {
  name: string
  type: string
  default?: string
  value: string
  sweep: boolean
  sweepValues: string[]
  boolValue: boolean
}

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
  selectedScript: FSEntry | null
  parseResult: ParseResult | null
  projectName: string
  note: string
  args: ArgState[]
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
  fromJobId: string
  prefs: any
  groupTaskCount: (group: SweepGroup) => number
  getNextGroupId: () => number
}

export const SUBMIT_STATE_KEY: InjectionKey<SubmitState> = Symbol('submit-state')
