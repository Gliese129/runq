// Render layer: fold state, render items and inline segments (step 6).
import type { DisplayLine, PipelineResult, ProcessorToggles, RenderItem, DrainBlockItem, GroupBlockItem, FoldSummaryItem, TableBlockItem, TableBlock, TracebackBlock } from './types'
import { AUTO_FOLD_THRESHOLD, MIN_BLOCK_SIZE } from './types'
import type { Drain } from './drain'
import { RANK_RE, rankColor } from './lex'

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

