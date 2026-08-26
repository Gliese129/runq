// resultsView (RQ2-4 ②) — the D-ticket adapter over the columnar results
// wire (RQ2-1 §A). The wire's single dimension is the record index; this
// module pivots range slices into table rows (latest per group) and chart
// series (x-ordered per group). Pure functions — the card renders, this
// decides. Slice semantics mirror internal/cli/results_cmd.go exactly.
import type { JobResultsResponse, ResultAxis, ResultRange } from '@/types/api'

/** First x candidate is the wire's sort key (schema.x_axes order). */
export function primaryX(res: JobResultsResponse): string {
  return res.schema.x_axes[0] ?? ''
}

function toNum(v: number | boolean | null | undefined): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

/** A group's "latest" record. X-bearing records form a monotonic prefix
 *  (off-axis records are a ts-ordered tail), so latest = last x-bearing
 *  index, walking back over the null tail. No x axis, or no x-bearing
 *  records at all → sequence order, i.e. the group's last record. */
export function latestIdx(res: JobResultsResponse, g: ResultRange, x: string): number {
  const last = g.offset + g.count - 1
  if (!x) return last
  const col = res.cols.axes[x]
  if (!col) return last
  for (let i = last; i >= g.offset; i--) {
    if (toNum(col[i]) !== null) return i
  }
  return last
}

/** Decode one axis cell: vocab lookup for str, plain rendering otherwise.
 *  null (hole or type-conflict nulling) reads as an em-dash. */
export function axisCell(ax: ResultAxis, v: number | boolean | null | undefined): string {
  if (v === null || v === undefined) return '—'
  if (ax.type === 'str') {
    const i = typeof v === 'number' ? v : -1
    return i >= 0 && i < (ax.vocab?.length ?? 0) ? ax.vocab![i] : '?'
  }
  return String(v)
}

/** Identity keys are typed on the wire (s:/n:/b:/task:) so "1" ≠ 1 ≠ true
 *  never collide; the UI shows the untyped face. Task-fallback groups show
 *  the short id. */
export function displayGroupKey(key: string): string {
  if (key.startsWith('task:')) return key.slice(5, 13)
  const m = /^[snb]:(.*)$/s.exec(key)
  return m ? m[1] : key
}

export interface ResultRow {
  gi: number
  /** Display face of the group's identity key. */
  key: string
  /** Record index of the latest slice. */
  idx: number
  /** Primary-x value at idx — the honesty annotation for lagging rows. */
  atX: number | null
  /** True when this row's atX trails the furthest row (annotate "@x"). */
  behind: boolean
  /** Decoded axis values at idx (identity + label roles), plus "group". */
  labels: Record<string, string>
  metrics: Record<string, number | null>
}

/** Axis names usable in the row-label template, in a stable order. */
export function labelKeys(res: JobResultsResponse): string[] {
  const keys = ['group']
  for (const [name, ax] of Object.entries(res.schema.axes)) {
    if (ax.role === 'identity' || ax.role === 'label') keys.push(name)
  }
  return keys
}

/** One row per group, holding its latest record. `behind` compares each
 *  row's atX to the maximum across rows — mixed-progress sweeps annotate
 *  instead of silently comparing different steps. */
export function tableRows(res: JobResultsResponse): ResultRow[] {
  const x = primaryX(res)
  const rows: ResultRow[] = res.schema.groups.map((g, gi) => {
    const idx = latestIdx(res, g, x)
    const labels: Record<string, string> = { group: displayGroupKey(g.key ?? '') }
    for (const [name, ax] of Object.entries(res.schema.axes)) {
      if (ax.role === 'identity' || ax.role === 'label') {
        labels[name] = axisCell(ax, res.cols.axes[name]?.[idx])
      }
    }
    const metrics: Record<string, number | null> = {}
    for (const m of res.schema.metrics) {
      metrics[m] = toNum(res.cols.metrics[m]?.[idx])
    }
    return {
      gi,
      key: displayGroupKey(g.key ?? ''),
      idx,
      atX: x ? toNum(res.cols.axes[x]?.[idx]) : null,
      behind: false,
      labels,
      metrics,
    }
  })
  const maxX = rows.reduce<number | null>(
    (acc, r) => (r.atX !== null && (acc === null || r.atX > acc) ? r.atX : acc), null)
  if (maxX !== null) {
    for (const r of rows) r.behind = r.atX !== null && r.atX < maxX
  }
  return rows
}

/** Render the row-label template: "{key}" substitutes, unknown keys stay
 *  literal (visible typo beats silent blank). Empty template → group key. */
export function renderLabel(template: string, row: ResultRow): string {
  const t = template.trim()
  if (!t) return row.key
  return t.replace(/\{([^}]+)\}/g, (whole, k: string) =>
    k in row.labels ? row.labels[k] : whole)
}

/** Best-direction guess per metric name; the header arrow flips it. */
export function guessDir(col: string): 'min' | 'max' {
  return /loss|err|error|ppl|perplex|latency|_ms$|time|nll/i.test(col) ? 'min' : 'max'
}

/** Column-wide decimal count: the max decimals any value carries, capped
 *  at 4 — every cell in a column renders with the same width. */
export function colDecimals(vals: (number | null)[]): number {
  let d = 0
  for (const v of vals) {
    if (v === null) continue
    const s = String(v)
    const dot = s.indexOf('.')
    if (dot >= 0) d = Math.max(d, Math.min(4, s.length - dot - 1))
    if (s.includes('e-')) d = 4
  }
  return d
}

export function fmtNum(v: number | null, dec: number): string {
  if (v === null) return '—'
  return v.toFixed(dec)
}

/** Index (into rows) of the best value for a column, or -1. */
export function bestIndex(rows: ResultRow[], col: string, dir: 'min' | 'max'): number {
  let best = -1
  let bv = 0
  rows.forEach((r, i) => {
    const v = r.metrics[col]
    if (v === null) return
    if (best === -1 || (dir === 'min' ? v < bv : v > bv)) { best = i; bv = v }
  })
  return best
}

/** X-bearing records of one group, for the Δ-base step picker. */
export function groupXOptions(res: JobResultsResponse, gi: number, x: string): { idx: number; xv: number }[] {
  const g = res.schema.groups[gi]
  if (!g || !x) return []
  const col = res.cols.axes[x]
  if (!col) return []
  const out: { idx: number; xv: number }[] = []
  for (let i = g.offset; i < g.offset + g.count; i++) {
    const xv = toNum(col[i])
    if (xv !== null) out.push({ idx: i, xv })
  }
  return out
}

/** Metric record at an arbitrary record index (the Δ base). */
export function metricsAt(res: JobResultsResponse, idx: number): Record<string, number | null> {
  const out: Record<string, number | null> = {}
  for (const m of res.schema.metrics) out[m] = toNum(res.cols.metrics[m]?.[idx])
  return out
}

/** GitHub-flavored markdown export of the visible table. */
export function resultsMarkdown(
  rows: ResultRow[],
  cols: string[],
  template: string,
  decimals: Record<string, number>,
  dirs: Record<string, 'min' | 'max'>,
): string {
  const head = ['run', ...cols.map(c => `${c} ${dirs[c] === 'min' ? '↓' : '↑'}`)]
  const lines = [
    `| ${head.join(' | ')} |`,
    `| ${head.map((_, i) => (i === 0 ? '---' : '---:')).join(' | ')} |`,
  ]
  const bests = new Map(cols.map(c => [c, bestIndex(rows, c, dirs[c] ?? guessDir(c))]))
  rows.forEach((r, i) => {
    const label = renderLabel(template, r) + (r.behind && r.atX !== null ? ` (@${r.atX})` : '')
    const cells = cols.map(c => {
      const s = fmtNum(r.metrics[c], decimals[c] ?? 0)
      return bests.get(c) === i ? `**${s}**` : s
    })
    lines.push(`| ${label} | ${cells.join(' | ')} |`)
  })
  return lines.join('\n')
}

export interface GroupSeries {
  gi: number
  key: string
  points: { x: number; y: number }[]
}

/** One chart series per group for (metric, x): records where both are
 *  non-null, ordered by x. Any x candidate works — for a non-primary x
 *  the group's wire order isn't sorted on it, so sort here. */
export function groupSeries(res: JobResultsResponse, metric: string, x: string): GroupSeries[] {
  const mcol = res.cols.metrics[metric]
  const xcol = res.cols.axes[x]
  if (!mcol || !xcol) return []
  return res.schema.groups.map((g, gi) => {
    const points: { x: number; y: number }[] = []
    for (let i = g.offset; i < g.offset + g.count; i++) {
      const xv = toNum(xcol[i])
      const yv = toNum(mcol[i])
      if (xv !== null && yv !== null) points.push({ x: xv, y: yv })
    }
    points.sort((a, b) => a.x - b.x)
    return { gi, key: displayGroupKey(g.key ?? ''), points }
  }).filter(s => s.points.length > 0)
}
