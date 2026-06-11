import type { ProjectParam } from '@/types/submit'
import type { JobConfigPayload, ProjectPayload } from '@/types/api'
import {
  compile, taskCount, validateTable, rowEffect, isBlank, activeValues,
  type ParamRow, type LinkSet,
} from './paramTable'

export interface SubmitProjectDraft {
  name: string
  workDir: string
  cmd: string
  setupCmd: string
  gpus: number
  maxRetry: number
  envType: string
  envPath: string
  envName: string
  envText: string
  params: ProjectParam[]
}

export type ConfigureValidation =
  | { ok: true }
  | { ok: false; message: string; rowName?: string }

export function totalTaskCount(rows: ParamRow[], linkSets: LinkSet[]): number {
  return taskCount(rows, linkSets)
}

/** Human formula for the table footer, e.g. "lr(3) × bs(2) × [opt+mom](2)". */
export function sweepSummary(rows: ParamRow[], linkSets: LinkSet[]): string {
  const parts: string[] = []
  const linked = new Set<string>()
  for (const set of linkSets) {
    const members = set.members
      .map(m => rows.find(r => r.name === m))
      .filter((r): r is ParamRow => !!r)
    if (members.length < 2) continue
    for (const m of members) linked.add(m.name)
    const n = Math.min(...members.map(m => activeValues(m).length))
    parts.push(`[${members.map(m => m.name).join('+')}](${n})`)
  }
  for (const row of rows) {
    if (linked.has(row.name)) continue
    const n = activeValues(row).length
    if (n >= 2) parts.push(`${row.name}(${n})`)
  }
  return parts.join(' × ')
}

export function validateConfigure(rows: ParamRow[], linkSets: LinkSet[]): ConfigureValidation {
  return validateTable(rows, linkSets)
}

export function buildJobConfig(
  projectName: string,
  note: string,
  rows: ParamRow[],
  linkSets: LinkSet[],
): JobConfigPayload {
  return compile(projectName, note, rows, linkSets)
}

/** Names of params that actually vary across tasks (for review highlighting). */
export function sweptParamNames(rows: ParamRow[], linkSets: LinkSet[]): Set<string> {
  const names = new Set<string>()
  for (const row of rows) {
    const effect = rowEffect(row, linkSets)
    if (effect === 'sweep') names.add(row.name)
    if (effect === 'linked' && activeValues(row).length > 0) names.add(row.name)
  }
  return names
}

/** Fixed params as they will be submitted (single value or default). */
export function fixedParamPreview(rows: ParamRow[], linkSets: LinkSet[]): Record<string, string> {
  const fixed: Record<string, string> = {}
  for (const row of rows) {
    const effect = rowEffect(row, linkSets)
    if (effect === 'fixed') fixed[row.name] = activeValues(row)[0]
    else if (effect === 'fixed-default') fixed[row.name] = row.default
  }
  return fixed
}

export function dryRunHeaders(
  dryRunResult: Record<string, any>[],
  params: ProjectParam[],
): { title: string; key: string }[] {
  if (dryRunResult.length === 0) return []
  const keys = new Set<string>()
  for (const row of dryRunResult) {
    for (const key of Object.keys(row)) keys.add(key)
  }
  const ordered: string[] = []
  for (const p of params) {
    if (keys.delete(p.name)) ordered.push(p.name)
  }
  ordered.push(...Array.from(keys).sort())
  return ordered.map(k => ({ title: k, key: k }))
}

/** "KEY=VALUE per line" → map (comments and blanks ignored). */
export function parseEnvText(text: string): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const line of (text || '').split('\n')) {
    const l = line.trim()
    if (!l || l.startsWith('#')) continue
    const i = l.indexOf('=')
    if (i <= 0) continue
    out[l.slice(0, i).trim()] = l.slice(i + 1).trim()
  }
  return Object.keys(out).length > 0 ? out : undefined
}

export function buildProjectPayload(project: SubmitProjectDraft): ProjectPayload {
  const payload: ProjectPayload = {
    project_name: project.name.trim(),
    working_dir: project.workDir,
    command_template: project.cmd,
    setup_command: project.setupCmd.trim() || undefined,
    environment: parseEnvText(project.envText),
    defaults: { gpus_per_task: project.gpus, max_retry: project.maxRetry },
  }
  if (project.envType) {
    payload.python_env = {
      type: project.envType,
      path: project.envPath || undefined,
      name: project.envName || undefined,
    }
  }

  const params = project.params.filter(p => p.name.trim())
  if (params.length > 0) {
    payload.params = params.map(p => {
      const choices = p.values?.filter(v => !isBlank(v))
      return {
        name: p.name.trim(),
        type: p.type,
        default: !isBlank(p.default) ? p.default : undefined,
        choices: choices?.length ? choices : undefined,
        min: p.min ?? undefined,
        max: p.max ?? undefined,
        include: p.include, // persist curation — heuristic must run only once
        strict: p.strict || undefined,
        scope: p.scope || undefined,
      }
    })
  }
  return payload
}

export { isBlank as isBlankValue, activeValues } from './paramTable'
