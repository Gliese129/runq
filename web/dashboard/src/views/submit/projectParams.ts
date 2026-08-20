// Shared project-param helpers: normalize backend/param-parse shapes into
// the frontend ProjectParam model and merge fresh script-parse results over
// persisted definitions. Used by the submit flow (loading a project into
// the param table) and the project editor — one vocabulary, one copy.
import type { ProjectConfig, ParseResult } from '@/types/api'
import type { ProjectParam } from '@/types/submit'

export const PARAM_TYPE_NAMES = ['int', 'float', 'str', 'bool', 'file', 'folder', 'list']

export const COMMON_PARAMS = new Set([
  'epoch', 'epochs', 'num_epochs', 'n_epochs', 'max_epochs',
  'lr', 'learning_rate', 'learning-rate',
  'bs', 'batch_size', 'batch-size',
  'seed', 'num_workers', 'device', 'output', 'output_dir',
])

export function normalizeType(rawType: string): string {
  const lower = (rawType || '').toLowerCase()
  if (lower === 'str' || lower === 'string') return 'str'
  if (lower === 'int' || lower === 'integer') return 'int'
  if (lower === 'float' || lower === 'number') return 'float'
  if (lower === 'bool' || lower === 'boolean') return 'bool'
  if (lower === 'file') return 'file'
  if (lower === 'folder' || lower === 'dir' || lower === 'directory') return 'folder'
  if (lower === 'path') return 'file'
  if (lower === 'list' || lower === 'array') return 'list'
  if (PARAM_TYPE_NAMES.includes(lower)) return lower
  return 'str'
}

/** Build a ProjectParam from a parsed arg or persisted def, moving default
 *  into values for str/file/folder. Persisted `include` is the user's
 *  curation and survives; absent include = never curated. */
export function normalizeParam(a: NonNullable<ProjectConfig['params']>[number]): ProjectParam {
  const type = normalizeType(a.type)
  // String() coercion is load-bearing: js-yaml parses `default: 42` to a
  // JS number, and `42 || ''` keeps it a number — the save payload then
  // fails Go's `ParamDef.Default string` decode (feedback group 2).
  // "None"/null from old configs means "no default".
  const rawDef = a.default == null ? '' : String(a.default)
  const def = rawDef === 'None' ? '' : rawDef
  const values = Array.isArray(a.choices) ? a.choices.map(String) : []
  if (['str', 'file', 'folder'].includes(type) && def && !values.includes(def)) {
    values.unshift(def)
  }
  return {
    name: a.name, type, default: def, include: a.include ?? true,
    values: values.length > 0 ? values : undefined,
    min: a.min, max: a.max,
    strict: a.strict || undefined,
    scope: a.scope || undefined,
    style: a.style || undefined,
  }
}

/** Merge fresh script-parse results over persisted params: parse discovers
 *  new args, persisted definitions win on everything they already say. */
export function mergeParsedParams(current: ProjectParam[], args: ParseResult['args']): ProjectParam[] {
  const existing = new Map(current.map(p => [p.name, p]))
  const merged: ProjectParam[] = []
  for (const arg of args || []) {
    const discovered = normalizeParam(arg)
    const saved = existing.get(discovered.name)
    if (!saved) { merged.push(discovered); continue }
    existing.delete(discovered.name)
    merged.push({
      ...discovered,
      ...saved,
      values: saved.values?.length ? [...saved.values] : discovered.values ? [...discovered.values] : undefined,
      min: saved.min ?? discovered.min,
      max: saved.max ?? discovered.max,
    })
  }
  for (const saved of existing.values()) merged.push(saved)
  return merged
}

/** Locate the python script the command template runs (for param re-parse). */
export function inferScriptPath(cfg: ProjectConfig): string {
  const cmd = cfg.command_template || ''
  const match = cmd.match(/(?:^|\s)([^\s"'`]+\.py)(?:\s|$)/)
  if (!match) return ''
  const script = match[1]
  if (script.startsWith('/')) return script
  const base = (cfg.working_dir || '').replace(/\/+$/, '')
  return base ? `${base}/${script}` : script
}

/** First-time curation heuristic: auto-select common params when > 5 are
 *  discovered. Mutates in place; run ONLY when no include flag was ever
 *  persisted — the user's curation is the truth afterwards. */
export function autoIncludeCommonParams(params: ProjectParam[]) {
  if (params.length <= 5) return
  for (const p of params) p.include = COMMON_PARAMS.has(p.name.toLowerCase())
  if (!params.some(p => p.include)) {
    for (let i = 0; i < Math.min(5, params.length); i++) params[i].include = true
  }
}
