// Full pipeline assembly: raw lines -> PipelineResult (processLog).
import type { PipelineResult, ProcessorToggles, PreDrainRule, DisplayLine } from './types'
import { DEFAULT_PRE_DRAIN_RULES } from './types'
import { applyCrFolder, annotateLine } from './lex'
import { findTracebacks, findTableBlocks } from './structures'
import { Drain, applyPreDrainRules } from './drain'
import { detectMotifs } from './motifs'

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
