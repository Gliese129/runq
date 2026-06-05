import type { ProjectParam, SweepGroup } from '@/types/submit'
import type { JobConfigPayload, ProjectPayload } from '@/types/api'

export interface SubmitProjectDraft {
  name: string
  workDir: string
  cmd: string
  gpus: number
  maxRetry: number
  envType: string
  envPath: string
  envName: string
  params: ProjectParam[]
}

export interface ConfigureValidationOptions {
  listLengthMismatchMessage?: string
}

export type ConfigureValidation =
  | { ok: true }
  | { ok: false; message: string }

export function groupTaskCount(group: SweepGroup): number {
  if (group.params.length === 0) return 0
  const counts = group.params.map(p => activeValues(p).length)
  if (counts.every(c => c === 0)) return 0
  if (group.type === 'grid') {
    return counts.filter(c => c > 0).reduce((acc, c) => acc * c, 1)
  }
  const nonZero = counts.filter(c => c > 0)
  return nonZero.length > 0 ? Math.min(...nonZero) : 0
}

export function totalTaskCount(groups: SweepGroup[]): number {
  const activeGroups = groups.filter(g => g.params.length > 0)
  if (activeGroups.length === 0) return 1
  const counts = activeGroups.map(g => groupTaskCount(g))
  if (counts.some(c => c === 0)) return 0
  return counts.reduce((product, c) => product * c, 1)
}

export function sweepSummary(groups: SweepGroup[]): string {
  if (groups.length === 0) return ''
  return groups
    .filter(g => g.params.length > 0)
    .map(g => {
      const label = g.type === 'grid' ? 'Grid' : 'List'
      const parts = g.params.map(p => `${p.name}(${activeValues(p).length})`)
      return `[${label}] ${parts.join(g.type === 'grid' ? ' x ' : ', ')}`
    })
    .join(' + ')
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

export function buildProjectPayload(project: SubmitProjectDraft): ProjectPayload {
  const payload: ProjectPayload = {
    project_name: project.name.trim(),
    working_dir: project.workDir,
    command_template: project.cmd,
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
      const choices = p.values?.filter(v => !isBlankValue(v))
      return {
        name: p.name.trim(),
        type: p.type,
        default: !isBlankValue(p.default) ? p.default : undefined,
        choices: choices?.length ? choices : undefined,
        min: p.min ?? undefined,
        max: p.max ?? undefined,
      }
    })
  }
  return payload
}

export function buildJobConfig(
  projectName: string,
  note: string,
  project: SubmitProjectDraft,
  groups: SweepGroup[],
): JobConfigPayload {
  const sweep: JobConfigPayload['sweep'] = []
  const sweptNames = new Set<string>()

  for (const g of groups) {
    const active = g.params.filter(p => activeValues(p).length > 0)
    if (active.length === 0) continue
    const parameters: Record<string, { values: any[] }> = {}
    for (const p of active) {
      const cleaned = activeValues(p)
      parameters[p.name] = { values: cleaned.map(v => coerceValue(v, p.type)) }
      sweptNames.add(p.name)
    }
    sweep.push({ method: g.type, parameters })
  }

  const fixedParams: Record<string, any> = {}
  for (const p of project.params) {
    if (!p.include || isBlankValue(p.default) || sweptNames.has(p.name)) continue
    fixedParams[p.name] = coerceValue(p.default, p.type)
  }

  return {
    project: projectName,
    note,
    fixed_params: Object.keys(fixedParams).length > 0 ? fixedParams : undefined,
    sweep,
  }
}

export function validateConfigure(
  groups: SweepGroup[],
  options: ConfigureValidationOptions = {},
): ConfigureValidation {
  const activeGroups = groups.filter(g => g.params.length > 0)
  for (const g of activeGroups) {
    const emptyParams = g.params.filter(p => activeValues(p).length === 0)
    if (emptyParams.length > 0) {
      return { ok: false, message: `"${emptyParams[0].name}" has no values — add values or remove it` }
    }
    for (const p of g.params) {
      const invalid = activeValues(p).find(v => validateTypedValue(v, p.type))
      if (invalid != null) {
        return { ok: false, message: `"${p.name}" has invalid ${p.type} value: ${invalid}` }
      }
    }
  }

  for (const g of groups) {
    if (g.type !== 'list' || g.params.length === 0) continue
    const lengths = g.params.map(p => activeValues(p).length).filter(l => l > 0)
    if (lengths.length > 0 && new Set(lengths).size > 1) {
      return {
        ok: false,
        message: options.listLengthMismatchMessage || 'List group 中各参数的值数量必须相等',
      }
    }
  }
  return { ok: true }
}

export function coerceValue(v: string, type: string): any {
  const trimmed = v.trim()
  switch (type) {
    case 'int': {
      const n = parseInt(trimmed, 10)
      return isNaN(n) ? v : n
    }
    case 'float': {
      const n = parseFloat(trimmed)
      return isNaN(n) ? v : n
    }
    case 'bool':
      return trimmed.toLowerCase() === 'true' || trimmed === '1'
    case 'str':
    case 'file':
    case 'folder':
    case 'list':
      return v
    default:
      return v
  }
}

export function activeValues(p: { values: string[] }): string[] {
  return p.values.filter(v => !isBlankValue(v))
}

export function isBlankValue(v: unknown): boolean {
  return String(v ?? '').trim() === ''
}

function validateTypedValue(value: string, type: string): string {
  const trimmed = value.trim()
  switch (type) {
    case 'int':
      return /^-?\d+$/.test(trimmed) ? '' : value
    case 'float':
      return trimmed !== '' && Number.isFinite(Number(trimmed)) ? '' : value
    case 'bool':
      return ['true', 'false', '1', '0'].includes(trimmed.toLowerCase()) ? '' : value
    default:
      return ''
  }
}
