// Full pipeline assembly: raw lines -> PipelineResult (processLog), plus
// the reusable SEGMENT processor the incremental pipeline builds on.
import type { PipelineResult, ProcessorToggles, PreDrainRule, DisplayLine, TracebackBlock, TableBlock } from './types'
import { DEFAULT_PRE_DRAIN_RULES } from './types'
import { applyCrFolder, annotateLine } from './lex'
import { findTracebacks, findTableBlocks } from './structures'
import { Drain, applyCompiledPreDrainRules, compilePreDrainRules, type CompiledPreDrainRule } from './drain'
import { detectMotifs } from './motifs'

export interface SegmentResult {
  display: DisplayLine[]
  tracebacks: TracebackBlock[]
  tableBlocks: TableBlock[]
  /** cluster id per display line (-1 = structural, not drained) — kept so
   *  a windowed caller can subtract this segment from the Drain again */
  cids: number[]
}

/**
 * Process one self-contained run of RAW lines: fold → annotate → detect
 * structures → drain. Shared by the one-shot processLog and the
 * incremental pipeline (which re-runs its pending tail through here).
 * `baseIdx` offsets lineIdx / structure indices so segments concatenate
 * into one coherent coordinate space; `drain` accumulates ACROSS segments.
 */
export function processSegment(
  rawLines: string[],
  toggles: ProcessorToggles,
  compiledRules: CompiledPreDrainRule[],
  drain: Drain,
  baseIdx: number,
): SegmentResult {
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

  // ③ Structure detection (multi-line, local to this segment — the
  // incremental caller guarantees structures never span segment cuts)
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
  const cids = new Array<number>(lines.length).fill(-1)
  for (let i = 0; i < lines.length; i++) {
    if (tbMember.has(i) || tableMember.has(i)) continue
    const ruled = applyCompiledPreDrainRules(annotations[i].text, compiledRules)
    cids[i] = drain.parse(ruled)
  }

  // Assemble DisplayLines in the caller's coordinate space
  const display: DisplayLine[] = annotations.map((a, i) => ({
    text: a.text,
    lineIdx: baseIdx + i,
    timestamp: a.timestamp,
    tags: a.tags,
    rank: a.rank,
    metrics: a.metrics,
    tqdmFolded: a.tqdmFolded,
    clusterId: cids[i],
    tracebackIdx: tbMember.get(i) ?? -1,
  }))

  // Shift structure indices into the shared coordinate space
  for (const tb of tracebacks) { tb.start += baseIdx; tb.end += baseIdx }
  for (const t of tableBlocks) { t.start += baseIdx; t.end += baseIdx }
  // tracebackIdx above is segment-local; the incremental caller re-bases it.

  return { display, tracebacks, tableBlocks, cids }
}

// ── Full Pipeline (one-shot) ─────────────────────────────────

export function processLog(
  rawLines: string[],
  toggles: ProcessorToggles,
  rules: PreDrainRule[] = DEFAULT_PRE_DRAIN_RULES,
): PipelineResult {
  const drain = new Drain()
  const seg = processSegment(rawLines, toggles, compilePreDrainRules(rules), drain, 0)
  const motifGroups = detectMotifs(seg.display, drain)
  return { lines: seg.display, motifGroups, tracebacks: seg.tracebacks, tableBlocks: seg.tableBlocks, drain }
}
