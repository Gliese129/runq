// activityMath.spec — pins the cumulative→delta contract and the
// associativity of re-bucketing (sum deltas, last cumulative), which is
// what keeps click-seek targets (cumBytes/cumLines) exact at any zoom.
import { describe, it, expect } from 'vitest'
import { toCells, pickStep, bucketize, niceAxis } from './activityMath'

const points = [
  { ts: 100, lines: 50, bytes: 500 },
  { ts: 160, lines: 120, bytes: 1300 },
  { ts: 220, lines: 120, bytes: 1300 }, // stall: no new output
  { ts: 280, lines: 200, bytes: 2100 },
]

describe('toCells', () => {
  it('derives per-sample deltas from cumulative columns', () => {
    const cells = toCells(points)
    expect(cells.map(c => c.lines)).toEqual([50, 70, 0, 80])
    expect(cells.map(c => c.bytes)).toEqual([500, 800, 0, 800])
    expect(cells[3].cumLines).toBe(200)
    expect(cells[3].cumBytes).toBe(2100)
  })
})

describe('pickStep', () => {
  it('stays raw while samples fit, snaps to readable intervals beyond', () => {
    expect(pickStep(60, 900)).toBe(1)
    expect(pickStep(180, 900)).toBe(2)
    expect(pickStep(1200, 900)).toBe(15)
    expect(pickStep(1_000_000, 900)).toBe(240)
  })
})

describe('bucketize', () => {
  it('sums deltas and keeps the LAST cumulative per bucket', () => {
    const cells = toCells(points)
    const buckets = bucketize(cells, 0, 3, 2)
    expect(buckets).toHaveLength(2)
    expect(buckets[0].lines).toBe(120)      // 50 + 70
    expect(buckets[0].cumLines).toBe(120)   // last of the pair
    expect(buckets[0].idx).toBe(0)
    expect(buckets[1].lines).toBe(80)       // 0 + 80
    expect(buckets[1].cumBytes).toBe(2100)
    expect(buckets[1].idx).toBe(2)
  })

  it('windows into the full array with idx anchored to it', () => {
    const cells = toCells(points)
    const buckets = bucketize(cells, 1, 3, 1)
    expect(buckets.map(b => b.idx)).toEqual([1, 2, 3])
    expect(buckets[0].lines).toBe(70)
  })
})

describe('niceAxis', () => {
  it('produces 4 equal bands topping at or above the peak', () => {
    const { axisMax, ticks } = niceAxis(37)
    expect(axisMax).toBeGreaterThanOrEqual(37)
    expect(ticks).toHaveLength(5)
    expect(ticks[0]).toBe(axisMax)
    expect(ticks[4]).toBe(0)
    const gaps = ticks.slice(1).map((v, i) => ticks[i] - v)
    expect(new Set(gaps.map(g => g.toFixed(2))).size).toBe(1)
  })
})
