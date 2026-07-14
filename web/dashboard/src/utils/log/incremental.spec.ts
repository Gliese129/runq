import { describe, it, expect } from 'vitest'
import { processLog } from './pipeline'
import { IncrementalLogPipeline } from './incremental'
import { DEFAULT_PRE_DRAIN_RULES, type ProcessorToggles, type PipelineResult } from './types'

const TOGGLES: ProcessorToggles = {
  crFolder: true, tracebackFold: true, levelColoring: true,
  metricHighlight: true, rankColoring: true,
}

// Deterministic pseudo-random batch splitting (seeded LCG).
function lcg(seed: number) {
  let s = seed
  return () => (s = (s * 48271) % 2147483647) / 2147483647
}

function makeLog(n: number): string[] {
  const lines: string[] = []
  for (let i = 0; i < n; i++) {
    if (i % 97 === 40) {
      // traceback block (multi-line structure, may straddle batch cuts)
      lines.push('Traceback (most recent call last):')
      lines.push('  File "/app/train.py", line 42, in step')
      lines.push('  File "/usr/lib/python3/site-packages/torch/x.py", line 7, in fwd')
      lines.push('ValueError: boom at step ' + i)
    } else if (i % 89 === 30) {
      // tqdm run (folds; blank absorbed)
      lines.push(` 10%|█         | ${i}/500`)
      lines.push('')
      lines.push(` 12%|█▏        | ${i + 1}/500`)
    } else if (i % 61 === 20) {
      lines.push(`| epoch | loss |`)
      lines.push(`| ${i} | 0.${i % 7} |`)
    } else if (i % 7 === 0) {
      lines.push(`2026-07-13 10:00:0${i % 10} INFO step=${i} loss=0.${i % 997}`)
    } else {
      lines.push(`[Rank ${i % 4}] epoch ${Math.floor(i / 100)} batch ${i % 100}/100`)
    }
  }
  return lines
}

/** clusterId is an OPAQUE handle — allocation order differs between the
 *  incremental path (subtract/re-parse cycles) and the one-shot path.
 *  Canonicalize to first-appearance order so we compare the GROUPING
 *  (which lines share a cluster), not the arbitrary ids. */
function normalize(r: PipelineResult) {
  const canon = new Map<number, number>()
  const mapCid = (cid: number) => {
    if (cid < 0) return cid
    if (!canon.has(cid)) canon.set(cid, canon.size)
    return canon.get(cid)!
  }
  return {
    lines: r.lines.map(l => ({ ...l, clusterId: mapCid(l.clusterId), tags: [...l.tags].sort() })),
    tracebacks: r.tracebacks,
    tableBlocks: r.tableBlocks,
  }
}

describe('IncrementalLogPipeline ≡ processLog', () => {
  // The one correctness contract: ANY batch split must produce the same
  // lines/structures as a single-shot run — including splits that land
  // inside tqdm runs, tracebacks and tables.
  it('random batch splits match the one-shot result', () => {
    const raw = makeLog(3000)
    const oneShot = processLog(raw, TOGGLES, DEFAULT_PRE_DRAIN_RULES)

    for (const seed of [1, 7, 42]) {
      const rnd = lcg(seed)
      const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
      let i = 0
      while (i < raw.length) {
        const n = 1 + Math.floor(rnd() * 120)
        inc.push(raw.slice(i, i + n))
        i += n
      }
      inc.recomputeMotifs()
      const got = inc.result()
      expect(normalize(got)).toEqual(normalize(oneShot))
      // Drain counts must be exact after all the subtract/re-add cycles.
      for (const l of got.lines) {
        if (l.clusterId >= 0) expect(got.drain.count(l.clusterId)).toBeGreaterThan(0)
      }
    }
  })

  it('reset(rebuild) matches one-shot (trim path)', () => {
    const raw = makeLog(1200)
    const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    inc.push(raw.slice(0, 700))
    // simulate a ring-buffer trim: keep the last 800 lines, rebuild
    const kept = raw.slice(400)
    inc.reset(kept)
    const oneShot = processLog(kept, TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    expect(normalize(inc.result())).toEqual(normalize(oneShot))
  })

  it('empty pushes and single-line pushes are safe', () => {
    const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    inc.push([])
    inc.push(['only line'])
    inc.push([])
    expect(inc.result().lines.map(l => l.text)).toEqual(['only line'])
  })

  // ── replaceTailLine (log contract v2: continues-fragment merge) ──

  it('replaceTailLine ≡ pushing the full line in one shot', () => {
    const raw = makeLog(1200)
    const full = raw.concat(['2026-07-13 10:01:00 INFO final step loss=0.123'])
    const oneShot = processLog(full, TOGGLES, DEFAULT_PRE_DRAIN_RULES)

    // Stream everything, but deliver the last line as fragment + merge —
    // exactly what a continues page does to the buffer.
    const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    inc.push(raw)
    inc.push(['2026-07-13 10:01:00 INFO fin'])
    inc.replaceTailLine('2026-07-13 10:01:00 INFO final step loss=0.123')
    inc.recomputeMotifs()

    expect(normalize(inc.result())).toEqual(normalize(oneShot))
    for (const l of inc.result().lines) {
      if (l.clusterId >= 0) expect(inc.result().drain.count(l.clusterId)).toBeGreaterThan(0)
    }
  })

  it('replaceTailLine chains (multi-fragment mega line) stay equivalent', () => {
    const oneShot = processLog(['start', 'ABCDEF'], TOGGLES, DEFAULT_PRE_DRAIN_RULES)

    const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    inc.push(['start'])
    inc.push(['AB'])
    inc.replaceTailLine('ABCD')
    inc.replaceTailLine('ABCDEF')
    inc.recomputeMotifs()

    expect(normalize(inc.result())).toEqual(normalize(oneShot))
  })

  it('replaceTailLine on an empty pipeline degrades to push', () => {
    const inc = new IncrementalLogPipeline(TOGGLES, DEFAULT_PRE_DRAIN_RULES)
    inc.replaceTailLine('lonely')
    expect(inc.result().lines.map(l => l.text)).toEqual(['lonely'])
  })
})
