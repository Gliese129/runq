import { describe, expect, it } from 'vitest'
import {
  buildRenderItems,
  computeDefaultFoldState,
  processLog,
  type ProcessorToggles,
} from './logProcessors'

const toggles: ProcessorToggles = {
  crFolder: true,
  tracebackFold: true,
  levelColoring: true,
  metricHighlight: true,
  rankColoring: true,
}

describe('log motif rendering', () => {
  it('folds interleaved motifs when hidden', () => {
    const lines = [
      'Warning: swallow|japanese_mt_bench|0 does not have judge_score_coding_avg.',
      'Warning: swallow|japanese_mt_bench_truncate_6144|0 does not have judge_score_coding_avg.',
      'No samples found for japanese_mtbench_coding',
      'Warning: swallow|japanese_mt_bench|0 does not have judge_score_math_avg.',
      'Warning: swallow|japanese_mt_bench_truncate_6144|0 does not have judge_score_math_avg.',
      'No samples found for japanese_mtbench_math',
      'Warning: swallow|japanese_mt_bench|0 does not have judge_score_writing_avg.',
      'Warning: swallow|japanese_mt_bench_truncate_6144|0 does not have judge_score_writing_avg.',
      'No samples found for japanese_mtbench_writing',
    ]

    const result = processLog(lines, toggles)
    const motif = result.motifGroups.find(g => g.motifLength > 1)
    expect(motif).toBeTruthy()
    expect(motif?.totalLines).toBe(9)

    // Collapsed: fold-summary (instance-level keys)
    const foldState = new Map<string, boolean>()
    for (let idx = 0; idx < motif!.instances.length; idx++) {
      foldState.set(`m:${motif!.id}:${idx}`, true)
    }
    const folded = buildRenderItems(result, foldState, result.drain)
    expect(folded.some(item => item.type === 'fold-summary')).toBe(true)

    // Expanded: flattened motif block (head + lines + tail)
    const expanded = buildRenderItems(result, new Map(), result.drain)
    const head = expanded.find(item => item.type === 'block-head')
    expect(head).toBeTruthy()
    if (head?.type === 'block-head') {
      expect(head.blockKind).toBe('motif')
      expect(head.repeats).toBeGreaterThan(1)
    }
    expect(expanded.filter(i => i.type === 'block-line' && i.blockKind === 'motif')).toHaveLength(9)
    expect(expanded.some(i => i.type === 'block-tail')).toBe(true)
  })

  it('flattens diff-view drain blocks for length=1 motifs', () => {
    const lines = [
      'INFO Epoch 1 loss=0.5 lr=0.001',
      'INFO Epoch 2 loss=0.4 lr=0.001',
      'INFO Epoch 3 loss=0.3 lr=0.001',
    ]

    const result = processLog(lines, toggles)
    const motif = result.motifGroups.find(g => g.motifLength === 1)
    expect(motif).toBeTruthy()

    // Expanded: block-head + one block-line per line + block-tail
    const items = buildRenderItems(result, new Map(), result.drain)
    const head = items.find(item => item.type === 'block-head')
    expect(head).toBeTruthy()
    if (head?.type === 'block-head') {
      expect(head.blockKind).toBe('drain')
      expect(head.lineCount).toBe(3)
      expect(head.foldKey).toMatch(/^m:\d+:\d+$/)
    }
    const blockLines = items.filter(i => i.type === 'block-line')
    expect(blockLines).toHaveLength(3)
    for (const bl of blockLines) {
      if (bl.type !== 'block-line') continue
      // Diff view is precomputed per line, alignment arrays shared per block
      expect(bl.cid).toBeGreaterThanOrEqual(0)
      expect(bl.tokens!.length).toBeGreaterThan(0)
      expect(bl.varMask!.length).toBeGreaterThan(0)
      expect(bl.colWidths!.length).toBe(bl.varMask!.length)
    }
    const tailIdx = items.findIndex(i => i.type === 'block-tail')
    const headIdx = items.findIndex(i => i.type === 'block-head')
    expect(tailIdx).toBe(headIdx + 4) // head, 3 lines, tail — contiguous
  })

  it('flattens a large expanded block and folds it to a single summary', () => {
    // Well above AUTO_FOLD_THRESHOLD — the case virtualization exists for
    const lines: string[] = []
    for (let i = 0; i < 200; i++) lines.push(`INFO step ${i} loss=0.${i}`)

    const result = processLog(lines, toggles)
    const motif = result.motifGroups.find(g => g.motifLength === 1)
    expect(motif).toBeTruthy()

    // Expanded: every line is a TOP-LEVEL item (head + N lines + tail)
    const expanded = buildRenderItems(result, new Map(), result.drain)
    const head = expanded.find(i => i.type === 'block-head')
    expect(head).toBeTruthy()
    const lineItems = expanded.filter(i => i.type === 'block-line')
    expect(lineItems.length).toBe(motif!.totalLines)
    if (head?.type === 'block-head') expect(head.lineCount).toBe(lineItems.length)
    expect(expanded.filter(i => i.type === 'block-tail')).toHaveLength(1)

    // Collapsed (default auto-fold): one fold-summary, no block items
    const folded = buildRenderItems(result, computeDefaultFoldState(result), result.drain)
    expect(folded.filter(i => i.type === 'fold-summary')).toHaveLength(1)
    expect(folded.some(i => i.type === 'block-head' || i.type === 'block-line' || i.type === 'block-tail')).toBe(false)
  })

  it('uses unified fold state for tracebacks', () => {
    const lines = [
      'Traceback (most recent call last):',
      '  File "train.py", line 10, in main',
      'ValueError: bad input',
    ]

    const result = processLog(lines, toggles)
    expect(result.tracebacks).toHaveLength(1)

    // Default: not folded (lines render normally)
    const items = buildRenderItems(result, new Map(), result.drain)
    expect(items.filter(i => i.type === 'line')).toHaveLength(3)

    // Folded: fold-summary with traceback variant
    const foldState = new Map([['tb:0', true]])
    const folded = buildRenderItems(result, foldState, result.drain)
    const summary = folded.find(i => i.type === 'fold-summary')
    expect(summary).toBeTruthy()
    if (summary?.type === 'fold-summary') {
      expect(summary.variant).toBe('traceback')
      expect(summary.foldKey).toBe('tb:0')
    }
  })

  it('computeDefaultFoldState auto-folds large groups', () => {
    // Generate enough lines to exceed AUTO_FOLD_THRESHOLD (20)
    const lines: string[] = []
    for (let i = 0; i < 25; i++) lines.push(`INFO step ${i} loss=0.${i}`)

    const result = processLog(lines, toggles)
    const defaults = computeDefaultFoldState(result)
    // Should have auto-folded the large group
    expect([...defaults.values()].some(v => v === true)).toBe(true)
  })
})
