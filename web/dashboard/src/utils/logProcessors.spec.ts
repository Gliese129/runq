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

    // Expanded: group-block (panel with lines)
    const expanded = buildRenderItems(result, new Map(), result.drain)
    expect(expanded.some(item => item.type === 'group-block')).toBe(true)
  })

  it('creates diff-view drain-blocks for length=1 motifs', () => {
    const lines = [
      'INFO Epoch 1 loss=0.5 lr=0.001',
      'INFO Epoch 2 loss=0.4 lr=0.001',
      'INFO Epoch 3 loss=0.3 lr=0.001',
    ]

    const result = processLog(lines, toggles)
    const motif = result.motifGroups.find(g => g.motifLength === 1)
    expect(motif).toBeTruthy()

    // Expanded: drain-block with diff view
    const items = buildRenderItems(result, new Map(), result.drain)
    const block = items.find(item => item.type === 'drain-block')
    expect(block).toBeTruthy()
    if (block?.type === 'drain-block') {
      expect(block.lines).toHaveLength(3)
      expect(block.varMask.length).toBeGreaterThan(0)
      expect(block.foldKey).toMatch(/^m:\d+:\d+$/)
    }
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
