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

// Shared types, defaults and thresholds for the log pipeline.
import type { Drain } from './drain'

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


export const AUTO_FOLD_THRESHOLD = 20
export const MIN_BLOCK_SIZE = 3
export const LONG_LINE_THRESHOLD = 500
