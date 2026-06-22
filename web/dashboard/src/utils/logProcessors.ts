/**
 * Log processing pipeline for MLOps log viewer.
 *
 * Pipeline (linear, zero-exclusion):
 *   raw → ① crFolder → ② annotate → ③ structure → ④ Drain → ⑤ motif → ⑥ render
 *
 * Key design:
 *   - Each line carries `tags: Set<string>` (non-exclusive) instead of a `kind` enum.
 *   - ALL non-structural lines go through Drain (no metric/error skip).
 *   - ALL cid≥0 lines participate in motif detection (no exclusion).
 *   - Drain provides template + diff extraction for Option A diff view.
 *   - Tags drive rendering style; groups drive fold behavior. They don't conflict.
 */

// ── Types ────────────────────────────────────────────────────

/** A metric key=value match within a line */
export interface MetricMatch {
  start: number    // char offset in display text
  end: number
  key: string
  value: string
}

/** Non-exclusive line tags — a line can carry multiple */
export type LineTag =
  | 'tqdm'
  | 'metric'
  | 'error'
  | 'warning'
  | 'info'
  | 'debug'
  | 'traceback'
  | 'traceback-end'
  | 'user-code'
  | 'table'

/** A single occurrence of a repeating motif in the log */
export interface MotifInstance {
  lineIndices: number[]   // original line indices of the motif member lines
  repeats: number         // how many times the pattern repeats in this instance
}

/** A motif group: all occurrences of a particular repeating pattern */
export interface MotifGroup {
  id: number
  pattern: number[]       // cluster IDs forming the motif
  motifLength: number     // 1 = consecutive block, ≥2 = interleaved pattern
  templates: string[]     // Drain template string for each cid in pattern
  label: string           // display label
  instances: MotifInstance[]
  totalLines: number
  totalRepeats: number
}

/** A Python traceback block: Traceback ... → XxxError */
export interface TracebackBlock {
  start: number
  end: number                                // inclusive
  errorMessage: string
  userCodeOffsets: number[]                   // offsets from start for user code lines
}

/** A block of consecutive pipe-delimited table lines */
export interface TableBlock {
  start: number
  end: number       // inclusive
  headers: string[]
  rows: string[][]  // separator lines excluded
}

/** Processor toggle state */
export interface ProcessorToggles {
  crFolder: boolean
  tracebackFold: boolean
  levelColoring: boolean
  metricHighlight: boolean
  rankColoring: boolean
}

/** User-configurable normalization rule applied before Drain */
export interface PreDrainRule {
  name: string
  pattern: string       // regex source
  replacement: string   // supports $1, $2
  enabled: boolean
}

export const DEFAULT_PRE_DRAIN_RULES: PreDrainRule[] = [
  {
    name: 'tqdm',
    pattern: String.raw`\d+%\|[^|]*\|\s*\d+\/\d+\s*\[.*?\]`,
    replacement: 'tqdm',
  },
].map(r => ({ ...r, enabled: true }))

/** Single line ready for rendering */
export interface DisplayLine {
  text: string                // display text (timestamp stripped)
  lineIdx: number             // index in post-crFolder array
  timestamp: string
  tags: Set<LineTag>          // non-exclusive tags
  rank: number                // -1 if no rank
  metrics: MetricMatch[]
  tqdmFolded: number          // how many tqdm lines were folded (0 = not tqdm)
  clusterId: number           // Drain cluster ID, -1 = unassigned
  tracebackIdx: number        // index into tracebacks[], -1 = not in traceback
}

// ── Render Item Types ───────────────────────────────────────

/**
 * Unified collapsed summary for ANY foldable group (drain, motif, traceback).
 * Replaces the old drain-placeholder + motif-fold + traceback-summary.
 */
export interface FoldSummaryItem {
  type: 'fold-summary'
  foldKey: string        // key in foldState map — same key used to toggle
  label: string          // display text
  lineCount: number
  repeats?: number       // for interleaved patterns
  variant: 'drain' | 'motif' | 'traceback'  // drives styling
}

/** Diff-view drain block: dim static tokens, highlight variable tokens */
export interface DrainBlockItem {
  type: 'drain-block'
  foldKey: string
  lines: DisplayLine[]
  template: string            // e.g. "INFO Epoch <*> loss=<*>"
  templateTokens: string[]    // e.g. ["INFO","Epoch","<*>","loss=<*>"]
  /** Per line: all tokens (whitespace-split of original text) */
  tokens: string[][]
  /** Which token indices are variable (<*> in template) */
  varMask: boolean[]
  /** Max char width per column for alignment */
  colWidths: number[]
}

/** Visible length≥2 motif: lines grouped in a collapsible panel */
export interface GroupBlockItem {
  type: 'group-block'
  foldKey: string
  label: string
  lines: DisplayLine[]
  repeats: number
  lineCount: number
}

/** Rendered table block */
export interface TableBlockItem {
  type: 'table-block'
  startLine: number
  headers: string[]
  rows: string[][]
  lineCount: number
}

/** Items the renderer iterates over */
export type RenderItem =
  | { type: 'line'; line: DisplayLine }
  | FoldSummaryItem
  | DrainBlockItem
  | GroupBlockItem
  | TableBlockItem

/** Full pipeline output */
export interface PipelineResult {
  lines: DisplayLine[]
  motifGroups: MotifGroup[]
  tracebacks: TracebackBlock[]
  tableBlocks: TableBlock[]
  drain: Drain
}

// ── Constants ────────────────────────────────────────────────

const TQDM_RE = /\d+%\|[^|]*\|\s*\d+\/\d+/
const TABLE_LINE_RE = /^\s*\|.*\|\s*$/
const TABLE_SEP_RE = /^\s*\|[-:| ]+\|\s*$/
const TIMESTAMP_RE = /^\[?(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[,.\d]*)\]?\s*/
const RANK_RE = /\[(?:Rank|rank|GPU|Worker)[- ]?(\d+)\]/i
const METRIC_SRC = /\b(\w+)\s*=\s*(\d+\.?\d*(?:[eE][+-]?\d+)?)\b/.source
const TB_START = /Traceback \(most recent call last\):/
const TB_FILE  = /^\s+File "([^"]+)"/
const TB_ERROR = /^\w+(?:Error|Exception|Warning):/
const FRAMEWORK_PATH = /site-packages|lib\/python|<frozen|<string>/
const LEVEL_RULES: Array<[RegExp, LineTag]> = [
  [/\bERROR\b|\bCRITICAL\b|\bFATAL\b/,  'error'],
  [/\bWARNING\b|\bWARN\b/,               'warning'],
  [/\bDEBUG\b|\bTRACE\b/,                'debug'],
  [/\bINFO\b/,                           'info'],
]

export const AUTO_FOLD_THRESHOLD = 20
export const MIN_BLOCK_SIZE = 3
export const LONG_LINE_THRESHOLD = 500

// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-9;]*[A-Za-z]|\x1b\].*?(?:\x07|\x1b\\)/g
export function stripAnsi(line: string): string {
  return line.replace(ANSI_RE, '')
}

const RANK_COLORS = [
  '#22d3ee', '#a78bfa', '#f472b6', '#fb923c',
  '#34d399', '#fbbf24', '#60a5fa', '#e879f9',
]

export function formatTimestamp(ts: string): string {
  const m = /(\d{2}:\d{2}:\d{2})/.exec(ts)
  return m ? m[1] : ts
}

export function rankColor(rank: number): string {
  if (rank < 0) return ''
  return RANK_COLORS[rank % RANK_COLORS.length]
}

// ── Step ①: \r Folder ───────────────────────────────────────

export function applyCrFolder(lines: string[]): {
  lines: string[]
  tqdmFoldMap: Map<number, number>
} {
  const result: string[] = []
  const tqdmFoldMap = new Map<number, number>()
  let tqdmBuf: string[] = []

  const flush = () => {
    if (tqdmBuf.length === 0) return
    const idx = result.length
    result.push(tqdmBuf[tqdmBuf.length - 1])
    if (tqdmBuf.length > 1) tqdmFoldMap.set(idx, tqdmBuf.length - 1)
    tqdmBuf = []
  }

  for (const raw of lines) {
    let line = raw
    if (raw.includes('\r')) {
      const segs = raw.split('\r').filter(Boolean)
      if (segs.length > 0) line = segs[segs.length - 1]
    }
    if (TQDM_RE.test(line)) {
      tqdmBuf.push(line)
    } else if (line.trim() === '' && tqdmBuf.length > 0) {
      // absorb blank lines between tqdm updates
    } else {
      flush()
      result.push(line)
    }
  }
  flush()
  return { lines: result, tqdmFoldMap }
}

// ── Step ②: Annotate (per-line) ─────────────────────────────

interface AnnotatedLine {
  text: string          // timestamp-stripped display text
  timestamp: string
  tags: Set<LineTag>
  rank: number
  metrics: MetricMatch[]
  tqdmFolded: number
}

function annotateLine(raw: string, tqdmFolded: number, crFolderOn: boolean): AnnotatedLine {
  let text = raw
  let timestamp = ''
  const tsm = TIMESTAMP_RE.exec(raw)
  if (tsm) { timestamp = tsm[1]; text = raw.slice(tsm[0].length) }

  const tags = new Set<LineTag>()

  // Level (non-exclusive with everything)
  for (const [re, tag] of LEVEL_RULES) {
    if (re.test(text)) { tags.add(tag); break }
  }

  // Rank
  let rank = -1
  const rm = RANK_RE.exec(text)
  if (rm) rank = parseInt(rm[1], 10)

  // Metrics
  const metrics: MetricMatch[] = []
  const mre = new RegExp(METRIC_SRC, 'g')
  let mm: RegExpExecArray | null
  while ((mm = mre.exec(text)) !== null) {
    metrics.push({ start: mm.index, end: mm.index + mm[0].length, key: mm[1], value: mm[2] })
  }
  if (metrics.length > 0) tags.add('metric')

  // tqdm
  const isTqdm = tqdmFolded > 0 || (crFolderOn && TQDM_RE.test(raw))
  if (isTqdm) tags.add('tqdm')

  return { text, timestamp, tags, rank, metrics, tqdmFolded }
}

// ── Step ③: Structure Detection (multi-line) ────────────────

export function findTracebacks(lines: string[]): TracebackBlock[] {
  const blocks: TracebackBlock[] = []
  let i = 0
  while (i < lines.length) {
    if (TB_START.test(lines[i])) {
      const start = i
      const userCode: number[] = []
      i++
      while (i < lines.length && i - start < 200) {
        const fm = TB_FILE.exec(lines[i])
        if (fm && !FRAMEWORK_PATH.test(fm[1])) userCode.push(i - start)
        if (TB_ERROR.test(lines[i])) {
          blocks.push({ start, end: i, errorMessage: lines[i], userCodeOffsets: userCode })
          i++
          break
        }
        i++
      }
    } else {
      i++
    }
  }
  return blocks
}

function parseTableCells(line: string): string[] {
  return line.trim().replace(/^\||\|$/g, '').split('|').map(c => c.trim())
}

export function findTableBlocks(texts: string[]): TableBlock[] {
  const blocks: TableBlock[] = []
  let i = 0
  while (i < texts.length) {
    if (TABLE_LINE_RE.test(texts[i])) {
      const start = i
      while (i < texts.length && TABLE_LINE_RE.test(texts[i])) i++
      const end = i - 1
      if (end > start) {
        const headers = parseTableCells(texts[start])
        const rows: string[][] = []
        for (let j = start + 1; j <= end; j++) {
          if (TABLE_SEP_RE.test(texts[j])) continue
          rows.push(parseTableCells(texts[j]))
        }
        blocks.push({ start, end, headers, rows })
      }
    } else {
      i++
    }
  }
  return blocks
}

// ── Step ④: Drain ───────────────────────────────────────────

/**
 * Pre-tokenize normalizer: replaces known variable patterns with
 * typed placeholders so Drain matches on first sight.
 */
export function normalizeLine(line: string): string {
  return line
    // Pad separators so Drain tokenizes on them → finer templates
    .replace(/\|/g, ' | ')
    .replace(/,(?!\d)/g, ' , ')
    .replace(/\b[0-9a-f]{8,}(?:-[0-9a-f]{4,})*\b/gi, '<HEX>')
    .replace(/(?:\/[\w._-]+){2,}(?:\.\w+)?/g, '<PATH>')
    .replace(/s3:\/\/\S+/gi, '<PATH>')
    .replace(/hdfs:\/\/\S+/gi, '<PATH>')
    .replace(/\b\d+\.\d+(?:[eE][+-]?\d+)?\b/g, '<NUM>')
    .replace(/\b\d{2,}\b/g, '<NUM>')
}

export function applyPreDrainRules(line: string, rules: PreDrainRule[]): string {
  let out = line
  for (const rule of rules) {
    if (!rule.enabled) continue
    try { out = out.replace(new RegExp(rule.pattern, 'g'), rule.replacement) }
    catch { /* invalid regex — skip */ }
  }
  return out
}

interface DrainNode {
  id: number
  template: string[]
  count: number
}

export class Drain {
  private tree = new Map<string, DrainNode[]>()
  private nextId = 0
  private simTh: number

  constructor(simThreshold = 0.5) { this.simTh = simThreshold }

  parse(line: string): number {
    const normalized = normalizeLine(line)
    const tokens = normalized.trim().split(/\s+/)
    if (tokens.length === 0) return -1

    const key = `${tokens.length}:${tokens[0]}`
    let nodes = this.tree.get(key)
    if (!nodes) { nodes = []; this.tree.set(key, nodes) }

    let best: DrainNode | null = null
    let bestSim = 0
    for (const node of nodes) {
      if (node.template.length !== tokens.length) continue
      let match = 0
      for (let j = 0; j < tokens.length; j++) {
        if (node.template[j] === '<*>' || node.template[j] === tokens[j]) match++
      }
      const sim = match / tokens.length
      if (sim >= this.simTh && sim > bestSim) { bestSim = sim; best = node }
    }

    if (best) {
      best.template = best.template.map((t, j) =>
        t === '<*>' || t !== tokens[j] ? '<*>' : t,
      )
      best.count++
      return best.id
    }

    const id = this.nextId++
    nodes.push({ id, template: [...tokens], count: 1 })
    return id
  }

  getTemplate(id: number): string {
    for (const nodes of this.tree.values()) {
      const n = nodes.find(n => n.id === id)
      if (n) return n.template.join(' ')
    }
    return ''
  }

  getTemplateTokens(id: number): string[] {
    for (const nodes of this.tree.values()) {
      const n = nodes.find(n => n.id === id)
      if (n) return n.template
    }
    return []
  }
}

// ── Step ⑤: Motif Detection ────────────────────────────────

/**
 * Detect repeating motifs in the cluster ID sequence.
 * ALL lines with cid ≥ 0 participate — zero exclusion.
 *
 * Phase A: length=1 — consecutive same-template runs.
 * Phase B: segment-bounded, shortest-period-first.
 *   - Only scans within contiguous unclaimed segments (natural boundaries).
 *   - Always starts from segment beginning (no arbitrary start position).
 *   - Tries period 2 first, then 3, ..., 8 (finds the most natural cycle).
 *   - Each segment produces at most one instance.
 */
export function detectMotifs(
  lines: DisplayLine[],
  drain: Drain,
  minRepeats = MIN_BLOCK_SIZE,
): MotifGroup[] {
  // Build effective sequence: all lines with valid cid
  const eff: { cid: number; idx: number }[] = []
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].clusterId < 0) continue
    eff.push({ cid: lines[i].clusterId, idx: i })
  }
  if (eff.length === 0) return []

  const claimed = new Set<number>()
  interface RawMotif {
    pattern: number[]
    instances: { effStart: number; effCount: number; repeats: number }[]
  }
  const rawByKey = new Map<string, RawMotif>()

  // Phase A: length=1 — maximal runs of same cid
  {
    let i = 0
    while (i < eff.length) {
      const cid = eff[i].cid
      let reps = 1
      let j = i + 1
      while (j < eff.length && eff[j].cid === cid) { reps++; j++ }
      if (reps >= minRepeats) {
        const key = String(cid)
        let raw = rawByKey.get(key)
        if (!raw) { raw = { pattern: [cid], instances: [] }; rawByKey.set(key, raw) }
        raw.instances.push({ effStart: i, effCount: reps, repeats: reps })
        for (let k = i; k < j; k++) claimed.add(k)
      }
      i = j
    }
  }

  // Phase B: segment-bounded, shortest-period-first
  // 1. Collect unclaimed positions into contiguous segments
  const segments: number[][] = []
  {
    let seg: number[] = []
    for (let i = 0; i < eff.length; i++) {
      if (claimed.has(i)) {
        if (seg.length > 0) { segments.push(seg); seg = [] }
      } else {
        seg.push(i)
      }
    }
    if (seg.length > 0) segments.push(seg)
  }

  // 2. For each segment, find shortest period from the start
  for (const seg of segments) {
    if (seg.length < 4) continue // need at least period 2 × 2 repeats

    let bestL = 0
    let bestReps = 0

    for (let L = 2; L <= Math.min(8, Math.floor(seg.length / 2)); L++) {
      // Canonical pattern = first L CIDs in segment
      const pat: number[] = []
      for (let k = 0; k < L; k++) pat.push(eff[seg[k]].cid)
      if (new Set(pat).size === 1) continue // same-CID already handled by Phase A

      // Count full repeats from start
      let reps = 1
      let pos = L
      while (pos + L <= seg.length) {
        let match = true
        for (let k = 0; k < L; k++) {
          if (eff[seg[pos + k]].cid !== pat[k]) { match = false; break }
        }
        if (!match) break
        reps++
        pos += L
      }

      if (reps >= minRepeats) {
        bestL = L
        bestReps = reps
        break // shortest-first: accept the first that works
      }
    }

    if (bestL > 0) {
      const total = bestReps * bestL
      const pat: number[] = []
      for (let k = 0; k < bestL; k++) pat.push(eff[seg[k]].cid)
      const key = pat.join(',')
      let raw = rawByKey.get(key)
      if (!raw) { raw = { pattern: pat, instances: [] }; rawByKey.set(key, raw) }
      raw.instances.push({ effStart: seg[0], effCount: total, repeats: bestReps })
      for (let k = 0; k < total; k++) claimed.add(seg[k])
    }
  }

  // Build MotifGroups
  const groups: MotifGroup[] = []
  let gid = 0
  for (const raw of rawByKey.values()) {
    const templates = raw.pattern.map(cid => drain.getTemplate(cid))
    const unique = [...new Set(templates)]
    let label: string
    if (unique.length === 1) {
      label = unique[0]
    } else if (unique.length <= 3) {
      label = unique.join(' → ')
    } else {
      label = unique.slice(0, 2).join(' → ') + ` (+${unique.length - 2})`
    }

    const instances: MotifInstance[] = raw.instances.map(inst => {
      const indices: number[] = []
      for (let k = inst.effStart; k < inst.effStart + inst.effCount; k++) {
        indices.push(eff[k].idx)
      }
      return { lineIndices: indices, repeats: inst.repeats }
    })

    const totalLines = instances.reduce((s, inst) => s + inst.lineIndices.length, 0)
    const totalRepeats = instances.reduce((s, inst) => s + inst.repeats, 0)

    groups.push({
      id: gid++,
      pattern: raw.pattern,
      motifLength: raw.pattern.length,
      templates, label, instances, totalLines, totalRepeats,
    })
  }

  groups.sort((a, b) => b.totalLines - a.totalLines)
  return groups
}

// ── Full Pipeline ────────────────────────────────────────────

export function processLog(
  rawLines: string[],
  toggles: ProcessorToggles,
  rules: PreDrainRule[] = DEFAULT_PRE_DRAIN_RULES,
): PipelineResult {
  // ① crFolder
  let lines: string[]
  let tqdmFoldMap: Map<number, number>
  if (toggles.crFolder) {
    const cr = applyCrFolder(rawLines)
    lines = cr.lines
    tqdmFoldMap = cr.tqdmFoldMap
  } else {
    lines = rawLines
    tqdmFoldMap = new Map()
  }

  // ② Annotate every line (per-line, no multi-line context needed)
  const annotations = lines.map((line, i) =>
    annotateLine(line, tqdmFoldMap.get(i) ?? 0, toggles.crFolder),
  )

  // ③ Structure detection (multi-line)
  const tracebacks = toggles.tracebackFold ? findTracebacks(annotations.map(a => a.text)) : []
  const tbMember = new Map<number, number>()
  for (let ti = 0; ti < tracebacks.length; ti++) {
    for (let li = tracebacks[ti].start; li <= tracebacks[ti].end; li++) tbMember.set(li, ti)
  }

  const tableBlocks = findTableBlocks(annotations.map(a => a.text))
  const tableMember = new Set<number>()
  for (const t of tableBlocks) {
    for (let li = t.start; li <= t.end; li++) tableMember.add(li)
  }

  // Tag structural membership (non-exclusive: adds to existing tags)
  for (let i = 0; i < annotations.length; i++) {
    const a = annotations[i]
    if (tbMember.has(i)) {
      a.tags.add('traceback')
      const tb = tracebacks[tbMember.get(i)!]
      if (i === tb.end) a.tags.add('traceback-end')
      if (tb.userCodeOffsets.includes(i - tb.start)) a.tags.add('user-code')
    }
    if (tableMember.has(i)) a.tags.add('table')
  }

  // ④ Drain — ALL non-structural lines participate
  const drain = new Drain()
  const cids = new Array<number>(lines.length).fill(-1)
  for (let i = 0; i < lines.length; i++) {
    if (tbMember.has(i) || tableMember.has(i)) continue
    const ruled = applyPreDrainRules(annotations[i].text, rules)
    cids[i] = drain.parse(ruled)
  }

  // Assemble DisplayLines
  const displayLines: DisplayLine[] = annotations.map((a, i) => ({
    text: a.text,
    lineIdx: i,
    timestamp: a.timestamp,
    tags: a.tags,
    rank: a.rank,
    metrics: a.metrics,
    tqdmFolded: a.tqdmFolded,
    clusterId: cids[i],
    tracebackIdx: tbMember.get(i) ?? -1,
  }))

  // ⑤ Motif detection — zero exclusion
  const motifGroups = detectMotifs(displayLines, drain)

  return { lines: displayLines, motifGroups, tracebacks, tableBlocks, drain }
}

// ── Step ⑥: Render Helpers ──────────────────────────────────

/**
 * Build a diff-view drain-block: template identifies <*> positions,
 * static tokens dim, variable tokens highlighted + aligned.
 */
function buildDiffBlock(foldKey: string, lines: DisplayLine[], drain: Drain, cid: number): DrainBlockItem {
  const templateTokens = drain.getTemplateTokens(cid)
  const template = drain.getTemplate(cid)
  const tokens = lines.map(l => l.text.trim().split(/\s+/))
  const maxCols = Math.max(templateTokens.length, ...tokens.map(t => t.length))

  // varMask: true where template has <*>
  const varMask = new Array<boolean>(maxCols).fill(false)
  for (let c = 0; c < maxCols; c++) {
    if (c < templateTokens.length && templateTokens[c].includes('<*>')) {
      varMask[c] = true
    } else {
      // Also check if actual values differ across lines
      const vals = new Set(tokens.map(row => row[c] ?? ''))
      if (vals.size > 1) varMask[c] = true
    }
  }

  // Column widths for alignment
  const colWidths = new Array<number>(maxCols).fill(0)
  for (const row of tokens) {
    for (let c = 0; c < row.length; c++) {
      if (row[c].length > colWidths[c]) colWidths[c] = row[c].length
    }
  }

  return { type: 'drain-block', foldKey, lines, template, templateTokens, tokens, varMask, colWidths }
}

/** Split sorted indices into contiguous sub-arrays */
function splitContiguous(indices: number[]): number[][] {
  if (indices.length === 0) return []
  const segs: number[][] = [[indices[0]]]
  for (let i = 1; i < indices.length; i++) {
    if (indices[i] === indices[i - 1] + 1) {
      segs[segs.length - 1].push(indices[i])
    } else {
      segs.push([indices[i]])
    }
  }
  return segs
}

/**
 * Compute default fold state from pipeline result.
 * Groups with totalLines ≥ threshold are auto-folded (all instances).
 * Tracebacks default to expanded (not in map).
 * Returns foldKey→collapsed map. Keys: `m:${groupId}:${instIdx}` for motifs.
 */
export function computeDefaultFoldState(
  result: PipelineResult,
  autoThreshold = AUTO_FOLD_THRESHOLD,
): Map<string, boolean> {
  const state = new Map<string, boolean>()
  for (const g of result.motifGroups) {
    if (g.totalLines >= autoThreshold) {
      for (let idx = 0; idx < g.instances.length; idx++) {
        state.set(`m:${g.id}:${idx}`, true)
      }
    }
  }
  return state
}

/**
 * Build the final render list.
 *
 * Unified fold logic: foldState maps foldKey→collapsed (true=collapsed).
 *   - Motif instances: foldKey = `m:${groupId}:${instIdx}`  (per-instance, not per-label)
 *   - Tracebacks:      foldKey = `tb:${idx}`
 *
 * Collapsed → fold-summary. Expanded → drain-block (len=1) or group-block (len≥2).
 */
export function buildRenderItems(
  result: PipelineResult,
  foldState: Map<string, boolean>,
  drain: Drain,
): RenderItem[] {
  // Table blocks
  const tableBlockAt = new Map<number, TableBlock>()
  const tableLines = new Set<number>()
  for (const t of result.tableBlocks) {
    tableBlockAt.set(t.start, t)
    for (let li = t.start; li <= t.end; li++) tableLines.add(li)
  }

  // Traceback blocks
  const tbBlockAt = new Map<number, { tb: TracebackBlock; idx: number }>()
  const tbConsumedLines = new Set<number>()
  for (let ti = 0; ti < result.tracebacks.length; ti++) {
    const tb = result.tracebacks[ti]
    const collapsed = foldState.get(`tb:${ti}`) ?? false
    if (collapsed) {
      tbBlockAt.set(tb.start, { tb, idx: ti })
      for (let i = tb.start; i <= tb.end; i++) tbConsumedLines.add(i)
    }
  }

  // Motif rendering — instance-level fold keys
  const motifItemAt = new Map<number, RenderItem>()
  const motifConsumed = new Set<number>()
  const hiddenScatterIds = new Set<number>()

  for (const g of result.motifGroups) {
    // Check if ALL instances of this group are collapsed (for scatter hide)
    const allCollapsed = g.motifLength === 1 && g.instances.every((_, idx) =>
      foldState.get(`m:${g.id}:${idx}`) ?? false,
    )
    if (allCollapsed) {
      for (const cid of g.pattern) hiddenScatterIds.add(cid)
    }

    if (g.motifLength === 1) {
      for (let instIdx = 0; instIdx < g.instances.length; instIdx++) {
        const inst = g.instances[instIdx]
        const foldKey = `m:${g.id}:${instIdx}`
        const collapsed = foldState.get(foldKey) ?? false
        const segments = splitContiguous(inst.lineIndices)
        for (const seg of segments) {
          if (seg.length < MIN_BLOCK_SIZE) continue
          if (collapsed) {
            motifItemAt.set(seg[0], {
              type: 'fold-summary', foldKey, label: g.label,
              lineCount: seg.length, variant: 'drain',
            })
          } else {
            const blockLines = seg.map(li => result.lines[li])
            motifItemAt.set(seg[0], buildDiffBlock(foldKey, blockLines, drain, g.pattern[0]))
          }
          for (const li of seg) motifConsumed.add(li)
        }
      }
    } else {
      // Length ≥ 2 — same fold logic, different expanded rendering
      for (let instIdx = 0; instIdx < g.instances.length; instIdx++) {
        const inst = g.instances[instIdx]
        const foldKey = `m:${g.id}:${instIdx}`
        const collapsed = foldState.get(foldKey) ?? false
        if (collapsed) {
          motifItemAt.set(inst.lineIndices[0], {
            type: 'fold-summary', foldKey, label: g.label,
            lineCount: inst.lineIndices.length,
            repeats: inst.repeats, variant: 'motif',
          })
        } else {
          const blockLines = inst.lineIndices.map(li => result.lines[li])
          motifItemAt.set(inst.lineIndices[0], {
            type: 'group-block', foldKey, label: g.label,
            lines: blockLines, repeats: inst.repeats,
            lineCount: inst.lineIndices.length,
          })
        }
        for (const li of inst.lineIndices) motifConsumed.add(li)
      }
    }
  }

  // Build items
  const items: RenderItem[] = []
  for (let i = 0; i < result.lines.length; i++) {
    const tab = tableBlockAt.get(i)
    if (tab) {
      items.push({
        type: 'table-block', startLine: i,
        headers: tab.headers, rows: tab.rows,
        lineCount: tab.end - tab.start + 1,
      })
      i = tab.end
      continue
    }
    if (tableLines.has(i)) continue

    const tbEntry = tbBlockAt.get(i)
    if (tbEntry) {
      const foldKey = `tb:${tbEntry.idx}`
      items.push({
        type: 'fold-summary', foldKey,
        label: tbEntry.tb.errorMessage,
        lineCount: tbEntry.tb.end - tbEntry.tb.start + 1,
        variant: 'traceback',
      })
      i = tbEntry.tb.end
      continue
    }
    if (tbConsumedLines.has(i)) continue

    const mItem = motifItemAt.get(i)
    if (mItem) { items.push(mItem); continue }
    if (motifConsumed.has(i)) continue
    if (hiddenScatterIds.has(result.lines[i].clusterId)) continue

    items.push({ type: 'line', line: result.lines[i] })
  }

  return items
}

// ── Segment Helpers (inline highlighting) ───────────────────

/** Text segment for inline highlighting */
export interface TextSegment {
  text: string
  cls: string   // '' | 'log-metric' | 'log-rank'
  style?: string
}

/** Split display text into segments for metric + rank inline highlighting */
export function segmentLine(line: DisplayLine, toggles: ProcessorToggles): TextSegment[] {
  const text = line.text
  if (!text) return [{ text: '', cls: '' }]

  interface Span { start: number; end: number; cls: string; style?: string }
  const spans: Span[] = []

  if (toggles.metricHighlight) {
    for (const m of line.metrics) {
      spans.push({ start: m.start, end: m.end, cls: 'log-metric' })
    }
  }

  if (toggles.rankColoring && line.rank >= 0) {
    const rm = RANK_RE.exec(text)
    if (rm) {
      spans.push({
        start: rm.index, end: rm.index + rm[0].length,
        cls: 'log-rank', style: `color:${rankColor(line.rank)}`,
      })
    }
  }

  if (spans.length === 0) return [{ text, cls: '' }]
  spans.sort((a, b) => a.start - b.start)

  const segs: TextSegment[] = []
  let pos = 0
  for (const s of spans) {
    if (s.start < pos) continue
    if (s.start > pos) segs.push({ text: text.slice(pos, s.start), cls: '' })
    segs.push({ text: text.slice(s.start, s.end), cls: s.cls, style: s.style })
    pos = s.end
  }
  if (pos < text.length) segs.push({ text: text.slice(pos), cls: '' })
  return segs
}
