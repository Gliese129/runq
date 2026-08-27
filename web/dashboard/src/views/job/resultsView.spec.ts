// resultsView.spec — the adapter is the frontend mirror of the CLI's
// slice semantics (internal/cli/results_cmd.go); these cases pin the
// shared contract: latest = last x-bearing record, off-axis tail
// excluded, lagging rows annotated, template unknown-keys stay literal.
import { describe, it, expect } from 'vitest'
import type { JobResultsResponse } from '@/types/api'
import {
  latestIdx, tableRows, renderLabel, guessDir, colDecimals,
  bestIndex, groupXOptions, resultsMarkdown, groupSeries, labelKeys,
  resolveBaseIdx, metricsAt,
} from './resultsView'

/** Two groups: "base" has steps 100,200 + one off-axis tail record;
 *  "wide" has a single step 100 (lagging). One str label axis "data". */
function fixture(): JobResultsResponse {
  return {
    source: 'runq.record(**axes)',
    parsed: 4, skipped: 0, truncated: false, updated_at: 1000, n: 4,
    schema: {
      groups: [
        { key: 's:base', offset: 0, count: 3 },
        { key: 's:wide', offset: 3, count: 1 },
      ],
      tasks: [
        { id: 't1', offset: 0, count: 3 },
        { id: 't2', offset: 3, count: 1 },
      ],
      axes: {
        model: { type: 'str', role: 'identity', vocab: ['base', 'wide'] },
        step: { type: 'num', role: 'x' },
        data: { type: 'str', role: 'label', vocab: ['c4'] },
      },
      x_axes: ['step'],
      metrics: ['loss', 'acc'],
    },
    cols: {
      ts: [1, 2, 3, 4],
      axes: {
        model: [0, 0, 0, 1],
        step: [100, 200, null, 100],
        data: [0, 0, 0, null],
      },
      metrics: {
        loss: [2.5, 2.1, null, 2.31],
        acc: [0.6, 0.7, 0.72, 0.65],
      },
    },
  }
}

describe('latestIdx', () => {
  it('walks back over the off-axis tail to the last x-bearing record', () => {
    const res = fixture()
    expect(latestIdx(res, res.schema.groups[0], 'step')).toBe(1)
    expect(latestIdx(res, res.schema.groups[1], 'step')).toBe(3)
  })

  it('degrades to sequence order without an x axis', () => {
    const res = fixture()
    expect(latestIdx(res, res.schema.groups[0], '')).toBe(2)
  })

  it('falls back to the last record when no record bears x', () => {
    const res = fixture()
    res.cols.axes.step = [null, null, null, null]
    expect(latestIdx(res, res.schema.groups[0], 'step')).toBe(2)
  })
})

describe('tableRows', () => {
  it('one row per group at its latest slice, lagging rows flagged', () => {
    const rows = tableRows(fixture())
    expect(rows).toHaveLength(2)
    expect(rows[0].key).toBe('base')
    expect(rows[0].atX).toBe(200)
    expect(rows[0].behind).toBe(false)
    expect(rows[0].metrics.loss).toBe(2.1)
    // wide only reached step 100 — annotated, not silently compared
    expect(rows[1].atX).toBe(100)
    expect(rows[1].behind).toBe(true)
    expect(rows[1].metrics.loss).toBe(2.31)
  })

  it('decodes label axes through the vocab, holes as em-dash', () => {
    const rows = tableRows(fixture())
    expect(rows[0].labels.data).toBe('c4')
    expect(rows[1].labels.data).toBe('—')
    expect(labelKeys(fixture())).toEqual(['group', 'model', 'data'])
  })
})

describe('renderLabel', () => {
  it('substitutes known keys, keeps unknown keys literal', () => {
    const row = tableRows(fixture())[0]
    expect(renderLabel('{model} · {data}', row)).toBe('base · c4')
    expect(renderLabel('{typo}', row)).toBe('{typo}')
    expect(renderLabel('', row)).toBe('base')
  })
})

describe('column helpers', () => {
  it('guesses direction from the metric name', () => {
    expect(guessDir('loss')).toBe('min')
    expect(guessDir('eval_ppl')).toBe('min')
    expect(guessDir('latency_ms')).toBe('min')
    expect(guessDir('acc')).toBe('max')
  })

  it('aligns a column on its widest decimal, capped at 4', () => {
    expect(colDecimals([2.5, 2.31, null])).toBe(2)
    expect(colDecimals([1, 2])).toBe(0)
    expect(colDecimals([0.123456])).toBe(4)
  })

  it('bestIndex honors direction and skips holes', () => {
    const rows = tableRows(fixture())
    expect(bestIndex(rows, 'loss', 'min')).toBe(0)
    expect(bestIndex(rows, 'loss', 'max')).toBe(1)
  })
})

describe('Δ base picker', () => {
  it('offers only x-bearing records of the chosen group', () => {
    const opts = groupXOptions(fixture(), 0, 'step')
    expect(opts.map(o => o.xv)).toEqual([100, 200])
    expect(opts.map(o => o.idx)).toEqual([0, 1])
  })
})

describe('resolveBaseIdx (Codex F4: polling offset shift)', () => {
  it('a grown earlier group shifts offsets; the (key, x) base still resolves to the same record', () => {
    const v1 = fixture()
    const idx1 = resolveBaseIdx(v1, 's:wide', 100)
    expect(idx1).toBe(3)
    expect(metricsAt(v1, idx1!).loss).toBe(2.31)

    // Next poll: group "base" gained a record at step 300 — "wide" moved.
    const v2 = fixture()
    v2.schema.groups = [
      { key: 's:base', offset: 0, count: 4 },
      { key: 's:wide', offset: 4, count: 1 },
    ]
    v2.cols.axes.model = [0, 0, 0, 0, 1]
    v2.cols.axes.step = [100, 200, 300, null, 100]
    v2.cols.axes.data = [0, 0, 0, 0, null]
    v2.cols.metrics.loss = [2.5, 2.1, 1.9, null, 2.31]
    v2.cols.metrics.acc = [0.6, 0.7, 0.75, 0.72, 0.65]
    v2.n = 5

    const idx2 = resolveBaseIdx(v2, 's:wide', 100)
    expect(idx2).toBe(4) // NOT the stale absolute index 3 (now base@null-x)
    expect(metricsAt(v2, idx2!).loss).toBe(2.31)
  })

  it('a vanished x falls back to the group latest; a vanished group returns null', () => {
    const res = fixture()
    const fallback = resolveBaseIdx(res, 's:base', 999)
    expect(fallback).toBe(1) // base's latest x-bearing record
    expect(resolveBaseIdx(res, 's:base', null)).toBe(1)
    expect(resolveBaseIdx(res, 's:gone', 100)).toBeNull()
  })
})

describe('resultsMarkdown', () => {
  it('emits a GFM table with best bolded and lagging annotated', () => {
    const rows = tableRows(fixture())
    const md = resultsMarkdown(rows, ['loss'], '{model}', { loss: 2 }, { loss: 'min' })
    expect(md).toContain('| run | loss ↓ |')
    expect(md).toContain('| base | **2.10** |')
    expect(md).toContain('| wide (@100) | 2.31 |')
  })
})

describe('groupSeries', () => {
  it('per-group x-sorted points, holes dropped, empty series pruned', () => {
    const s = groupSeries(fixture(), 'loss', 'step')
    expect(s).toHaveLength(2)
    // base's off-axis record (null step) contributes no point
    expect(s[0].points).toEqual([{ x: 100, y: 2.5 }, { x: 200, y: 2.1 }])
    expect(s[1].points).toEqual([{ x: 100, y: 2.31 }])
  })
})
