// paramTable — the flat parameter model behind the submit configure step.
//
// Design: the UI is ONE flat table of params (no group cards). Sweep
// structure is DERIVED, not hand-built:
//   - row with 0 values → fixed at default (or unset if no default)
//   - row with 1 value  → fixed, overriding default
//   - row with ≥2 values → joins the cross product (one grid block)
//   - rows in a link set → values zip row-by-row (one list block per set)
//
// This is expressively equivalent to the YAML block model (cartesian
// product is associative: N grid blocks ≡ 1 grid block; only zip sets
// carry structure). compile() / decompile() map between the two; the
// YAML/CLI format is unchanged.
//
// Pure module: no Vue imports, fully unit-testable.

export interface ParamRow {
  name: string
  /** int | float | bool | str | file | folder — decoration, not a branch:
   *  it adds validation + cell affordances + typed YAML emission only. */
  type: string
  /** ghost value used when values is empty ('' = no default → unset) */
  default: string
  values: string[]
  meta?: { min?: number; max?: number; step?: number }
}

/** A zip group: member row names. Color = index into LINK_PALETTE. */
export interface LinkSet {
  id: string
  members: string[]
}

/** Link-set accent colors. Deliberately disjoint from statusGrammar's
 *  semantic colors (green/red/amber are taken by task states). */
export const LINK_PALETTE = [
  '#1D9E75', // teal
  '#7F77DD', // purple
  '#D4537E', // pink
  '#D85A30', // coral
  '#378ADD', // blue
] as const

export function linkColor(setIndex: number): string {
  return LINK_PALETTE[setIndex % LINK_PALETTE.length]
}

// ── helpers ──

export function isBlank(v: unknown): boolean {
  return String(v ?? '').trim() === ''
}

export function activeValues(row: Pick<ParamRow, 'values'>): string[] {
  return row.values.filter(v => !isBlank(v))
}

export type RowEffect = 'unset' | 'fixed-default' | 'fixed' | 'sweep' | 'linked'

export function rowEffect(row: ParamRow, linkSets: LinkSet[]): RowEffect {
  if (linkSets.some(s => s.members.includes(row.name))) return 'linked'
  const n = activeValues(row).length
  if (n >= 2) return 'sweep'
  if (n === 1) return 'fixed'
  return isBlank(row.default) ? 'unset' : 'fixed-default'
}

// ── typed emission (mirrors backend []any semantics) ──

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
    default:
      return v
  }
}

export function validateTypedValue(value: string, type: string): boolean {
  const trimmed = value.trim()
  switch (type) {
    case 'int':
      return /^-?\d+$/.test(trimmed)
    case 'float':
      return trimmed !== '' && Number.isFinite(Number(trimmed))
    case 'bool':
      return ['true', 'false', '1', '0'].includes(trimmed.toLowerCase())
    default:
      return true
  }
}

/** Infer a type from observed values (for decompile / custom params). */
export function inferType(values: any[]): string {
  if (values.length === 0) return 'str'
  if (values.every(v => typeof v === 'boolean')) return 'bool'
  if (values.every(v => typeof v === 'number' && Number.isInteger(v))) return 'int'
  if (values.every(v => typeof v === 'number')) return 'float'
  return 'str'
}

// ── validation ──

export type TableValidation =
  | { ok: true }
  | { ok: false; message: string; rowName?: string }

export function validateTable(rows: ParamRow[], linkSets: LinkSet[]): TableValidation {
  // typed values
  for (const row of rows) {
    const bad = activeValues(row).find(v => !validateTypedValue(v, row.type))
    if (bad != null) {
      return { ok: false, message: `"${row.name}" has invalid ${row.type} value: ${bad}`, rowName: row.name }
    }
  }
  // link sets: members exist, have values, and lengths match
  const byName = new Map(rows.map(r => [r.name, r]))
  for (const set of linkSets) {
    const members = set.members.map(m => byName.get(m)).filter((r): r is ParamRow => !!r)
    if (members.length < 2) continue // degenerate set — ignored by compile
    const lengths = members.map(r => activeValues(r).length)
    if (lengths.some(l => l === 0)) {
      const empty = members[lengths.indexOf(0)]
      return { ok: false, message: `linked param "${empty.name}" has no values`, rowName: empty.name }
    }
    if (new Set(lengths).size > 1) {
      const max = Math.max(...lengths)
      const short = members[lengths.findIndex(l => l < max)]
      return {
        ok: false,
        message: `linked params must have the same number of values — "${short.name}" has ${activeValues(short).length}, expected ${max}`,
        rowName: short.name,
      }
    }
  }
  return { ok: true }
}

// ── task count (preview is truth: same math as backend Expand) ──

export function taskCount(rows: ParamRow[], linkSets: LinkSet[]): number {
  const byName = new Map(rows.map(r => [r.name, r]))
  const sets = effectiveSets(rows, linkSets)
  const linked = new Set(sets.flatMap(s => s.members))
  let total = 1
  for (const set of sets) {
    // zip length = shortest member (0 values → 0 tasks, surfaced by validate)
    const lengths = set.members.map(m => activeValues(byName.get(m)!).length)
    total *= Math.min(...lengths)
  }
  for (const row of rows) {
    if (linked.has(row.name)) continue
    const n = activeValues(row).length
    if (n >= 2) total *= n
  }
  return total
}

/** Sets that actually carry zip structure (≥2 existing members). */
function effectiveSets(rows: ParamRow[], linkSets: LinkSet[]): LinkSet[] {
  const names = new Set(rows.map(r => r.name))
  return linkSets
    .map(s => ({ ...s, members: s.members.filter(m => names.has(m)) }))
    .filter(s => s.members.length >= 2)
}

// ── compile: flat model → JobConfig payload ──

export interface CompiledJobConfig {
  project: string
  note: string
  fixed_params?: Record<string, any>
  sweep: Array<{ method: string; parameters: Record<string, { values: any[] }> }>
}

export function compile(
  projectName: string,
  note: string,
  rows: ParamRow[],
  linkSets: LinkSet[],
): CompiledJobConfig {
  const byName = new Map(rows.map(r => [r.name, r]))
  const sweep: CompiledJobConfig['sweep'] = []
  const consumed = new Set<string>()

  // link sets → list blocks
  for (const set of effectiveSets(rows, linkSets)) {
    const parameters: Record<string, { values: any[] }> = {}
    for (const name of set.members) {
      const row = byName.get(name)!
      parameters[name] = { values: activeValues(row).map(v => coerceValue(v, row.type)) }
      consumed.add(name)
    }
    sweep.push({ method: 'list', parameters })
  }

  // remaining multi-value rows → one grid block
  const gridParams: Record<string, { values: any[] }> = {}
  for (const row of rows) {
    if (consumed.has(row.name)) continue
    const vals = activeValues(row)
    if (vals.length >= 2) {
      gridParams[row.name] = { values: vals.map(v => coerceValue(v, row.type)) }
      consumed.add(row.name)
    }
  }
  if (Object.keys(gridParams).length > 0) {
    sweep.push({ method: 'grid', parameters: gridParams })
  }

  // single-value → fixed (override), empty + default → fixed (default)
  const fixed: Record<string, any> = {}
  for (const row of rows) {
    if (consumed.has(row.name)) continue
    const vals = activeValues(row)
    if (vals.length === 1) {
      fixed[row.name] = coerceValue(vals[0], row.type)
    } else if (!isBlank(row.default)) {
      fixed[row.name] = coerceValue(row.default, row.type)
    }
  }

  return {
    project: projectName,
    note,
    fixed_params: Object.keys(fixed).length > 0 ? fixed : undefined,
    sweep,
  }
}

// ── decompile: JobConfig (canonical, e.g. a job's ConfigJSON) → flat model ──
// Entry point for "re-run job as template": grid blocks flatten to plain
// rows, each list block becomes one link set, fixed_params become
// single-value rows. Lossless w.r.t. expansion semantics.

export interface DecompileResult {
  rows: ParamRow[]
  linkSets: LinkSet[]
}

export function decompile(cfg: {
  fixed_params?: Record<string, any>
  sweep?: Array<{ method: string; parameters: Record<string, { values: any[] } | any[]> }>
}): DecompileResult {
  const rows: ParamRow[] = []
  const linkSets: LinkSet[] = []
  const seen = new Set<string>()
  let setCounter = 0

  const pushRow = (name: string, values: any[]) => {
    if (seen.has(name)) return
    seen.add(name)
    rows.push({
      name,
      type: inferType(values),
      default: '',
      values: values.map(v => String(v)),
    })
  }

  for (const block of cfg.sweep ?? []) {
    const names: string[] = []
    for (const [name, spec] of Object.entries(block.parameters ?? {})) {
      const values = Array.isArray(spec) ? spec : (spec?.values ?? [])
      pushRow(name, values)
      names.push(name)
    }
    if (block.method === 'list' && names.length >= 2) {
      linkSets.push({ id: `ls${setCounter++}`, members: names })
    }
  }

  for (const [name, value] of Object.entries(cfg.fixed_params ?? {})) {
    pushRow(name, [value])
  }

  return { rows, linkSets }
}
