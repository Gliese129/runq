// Motif (repeating pattern) detection over clustered lines (step 5).
import type { DisplayLine, MotifGroup, MotifInstance } from './types'
import { MIN_BLOCK_SIZE } from './types'
import type { Drain } from './drain'

// ── Step ⑤: Motif Detection ────────────────────────────────

/**
 * Detect repeating motifs in the cluster ID sequence.
 * ALL lines with cid ≥ 0 participate — zero exclusion.
 *
 * Phase A: length=1 — consecutive same-template runs.
 * Phase B: segment-bounded, shortest-period-first.
 *   - Only scans within contiguous unclaimed segments (natural boundaries).
 *   - Always starts from segment beginning (no arbitrary start position).
 *   - Tries period 2 first, then 3, ..., 8 (finds the most natural cycle).
 *   - Each segment produces at most one instance.
 */
export function detectMotifs(
  lines: DisplayLine[],
  drain: Drain,
  minRepeats = MIN_BLOCK_SIZE,
): MotifGroup[] {
  // Build effective sequence: all lines with valid cid
  const eff: { cid: number; idx: number }[] = []
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].clusterId < 0) continue
    eff.push({ cid: lines[i].clusterId, idx: i })
  }
  if (eff.length === 0) return []

  const claimed = new Set<number>()
  interface RawMotif {
    pattern: number[]
    instances: { effStart: number; effCount: number; repeats: number }[]
  }
  const rawByKey = new Map<string, RawMotif>()

  // Phase A: length=1 — maximal runs of same cid
  {
    let i = 0
    while (i < eff.length) {
      const cid = eff[i].cid
      let reps = 1
      let j = i + 1
      while (j < eff.length && eff[j].cid === cid) { reps++; j++ }
      if (reps >= minRepeats) {
        const key = String(cid)
        let raw = rawByKey.get(key)
        if (!raw) { raw = { pattern: [cid], instances: [] }; rawByKey.set(key, raw) }
        raw.instances.push({ effStart: i, effCount: reps, repeats: reps })
        for (let k = i; k < j; k++) claimed.add(k)
      }
      i = j
    }
  }

  // Phase B: segment-bounded, shortest-period-first
  // 1. Collect unclaimed positions into contiguous segments
  const segments: number[][] = []
  {
    let seg: number[] = []
    for (let i = 0; i < eff.length; i++) {
      if (claimed.has(i)) {
        if (seg.length > 0) { segments.push(seg); seg = [] }
      } else {
        seg.push(i)
      }
    }
    if (seg.length > 0) segments.push(seg)
  }

  // 2. For each segment, find shortest period from the start
  for (const seg of segments) {
    if (seg.length < 4) continue // need at least period 2 × 2 repeats

    let bestL = 0
    let bestReps = 0

    for (let L = 2; L <= Math.min(8, Math.floor(seg.length / 2)); L++) {
      // Canonical pattern = first L CIDs in segment
      const pat: number[] = []
      for (let k = 0; k < L; k++) pat.push(eff[seg[k]].cid)
      if (new Set(pat).size === 1) continue // same-CID already handled by Phase A

      // Count full repeats from start
      let reps = 1
      let pos = L
      while (pos + L <= seg.length) {
        let match = true
        for (let k = 0; k < L; k++) {
          if (eff[seg[pos + k]].cid !== pat[k]) { match = false; break }
        }
        if (!match) break
        reps++
        pos += L
      }

      if (reps >= minRepeats) {
        bestL = L
        bestReps = reps
        break // shortest-first: accept the first that works
      }
    }

    if (bestL > 0) {
      const total = bestReps * bestL
      const pat: number[] = []
      for (let k = 0; k < bestL; k++) pat.push(eff[seg[k]].cid)
      const key = pat.join(',')
      let raw = rawByKey.get(key)
      if (!raw) { raw = { pattern: pat, instances: [] }; rawByKey.set(key, raw) }
      raw.instances.push({ effStart: seg[0], effCount: total, repeats: bestReps })
      for (let k = 0; k < total; k++) claimed.add(seg[k])
    }
  }

  // Build MotifGroups
  const groups: MotifGroup[] = []
  let gid = 0
  for (const raw of rawByKey.values()) {
    const templates = raw.pattern.map(cid => drain.getTemplate(cid))
    const unique = [...new Set(templates)]
    let label: string
    if (unique.length === 1) {
      label = unique[0]
    } else if (unique.length <= 3) {
      label = unique.join(' → ')
    } else {
      label = unique.slice(0, 2).join(' → ') + ` (+${unique.length - 2})`
    }

    const instances: MotifInstance[] = raw.instances.map(inst => {
      const indices: number[] = []
      for (let k = inst.effStart; k < inst.effStart + inst.effCount; k++) {
        indices.push(eff[k].idx)
      }
      return { lineIndices: indices, repeats: inst.repeats }
    })

    const totalLines = instances.reduce((s, inst) => s + inst.lineIndices.length, 0)
    const totalRepeats = instances.reduce((s, inst) => s + inst.repeats, 0)

    groups.push({
      id: gid++,
      pattern: raw.pattern,
      motifLength: raw.pattern.length,
      templates, label, instances, totalLines, totalRepeats,
    })
  }

  groups.sort((a, b) => b.totalLines - a.totalLines)
  return groups
}

