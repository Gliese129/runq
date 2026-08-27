// activityMath (RQ2-4 ①, kit ScreensTask ActivityCurve) — pure math for
// the log-activity density curve. activity.tsv columns are CUMULATIVE
// (that's what makes owning-side stride decimation lossless), so the
// curve plots per-sample deltas; buckets re-aggregate by summing deltas
// and keeping the last cumulative value — associative, like the on-disk
// pyramid.
import type { ActivityPoint } from '@/types/api'

export interface ActivityCell {
  ts: number
  /** Lines written during this sample (delta). */
  lines: number
  bytes: number
  /** Cumulative lines at sample end — the log line number for seeking. */
  cumLines: number
  /** Cumulative bytes at sample end — the log byte offset for seeking. */
  cumBytes: number
}

/** Cumulative rows → per-sample delta cells. The first sample's delta is
 *  its own cumulative value (everything before the first sample). */
export function toCells(points: ActivityPoint[]): ActivityCell[] {
  return points.map((p, i) => {
    const prev = points[i - 1]
    return {
      ts: p.ts,
      lines: p.lines - (prev ? prev.lines : 0),
      bytes: p.bytes - (prev ? prev.bytes : 0),
      cumLines: p.lines,
      cumBytes: p.bytes,
    }
  })
}

/** Coarsest bucket size (in samples) that still fills the plot with one
 *  point per ~10 viewBox units. Snaps to readable intervals so the unit
 *  label ("/5 min") is a number people actually use. */
export function pickStep(visibleSamples: number, viewWidth: number): number {
  const want = visibleSamples / (viewWidth / 10)
  return [1, 2, 5, 10, 15, 30, 60, 120].find(s => s >= want) || 240
}

export interface ActivityBucket extends ActivityCell {
  /** Index (into the full cell array) of the bucket's first sample. */
  idx: number
  /** Samples merged into this bucket. */
  span: number
}

/** Re-bucket a visible slice: sum deltas, keep the last cumulative. */
export function bucketize(cells: ActivityCell[], i0: number, i1: number, step: number): ActivityBucket[] {
  const raw = cells.slice(i0, i1 + 1)
  if (step <= 1) return raw.map((c, k) => ({ ...c, idx: i0 + k, span: 1 }))
  const out: ActivityBucket[] = []
  for (let k = 0; k < raw.length; k += step) {
    const grp = raw.slice(k, k + step)
    const last = grp[grp.length - 1]
    out.push({
      ts: grp[0].ts,
      lines: grp.reduce((a, c) => a + c.lines, 0),
      bytes: grp.reduce((a, c) => a + c.bytes, 0),
      cumLines: last.cumLines,
      cumBytes: last.cumBytes,
      idx: i0 + k,
      span: grp.length,
    })
  }
  return out
}

/** Y axis as an arithmetic sequence: 4 bands of one round step, so the
 *  values and the gaps between them are equal. The top sits at or just
 *  above the visible peak. */
export function niceAxis(peak: number): { axisMax: number; ticks: number[] } {
  const raw = Math.max(peak, 1) / 4
  const mag = Math.pow(10, Math.floor(Math.log10(Math.max(raw, 1))))
  const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => s >= raw) || mag * 10
  return { axisMax: step * 4, ticks: [4, 3, 2, 1, 0].map(k => +(step * k).toFixed(2)) }
}
