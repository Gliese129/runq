// Incremental (windowed) log pipeline: follow-mode appends cost
// O(batch + pending tail) instead of re-processing the whole buffer.
//
// Model: [ stable prefix | pending tail(raw) ]
//  - stable: finalized DisplayLines/structures — never touched again
//  - pending: the last <= ~2*PENDING_WINDOW raw lines, fully re-processed
//    on every push (cross-line effects — \r folds, tracebacks, tables —
//    can only mutate near the growing edge)
//  - Drain: ONE tree shared across segments; re-processing the pending
//    tail first SUBTRACTS its previous contribution (Drain.remove), so
//    cluster counts stay exact. Templates never re-narrow (see Drain doc).
//  - finalization: when pending outgrows the window, its head is cut at a
//    SAFE raw line (no fold/structure can span the cut) and processed
//    into stable. Pathological logs with no safe cut simply keep growing
//    pending — correctness degrades to the one-shot cost, never breaks.
//  - motifs: global by nature; recomputed over the full window, throttled
//    (MOTIF_THROTTLE_MS) instead of per push.
//
// Trim (ring buffer) is reset-based for now: the absolute-line-number
// plumbing that a cheap head-trim needs is the same work as RQ-22's
// open-at-tail / scroll-up-backfill, and will land there. Drain.remove +
// this class's segment layout are the prepared substrate.
import type { PipelineResult, ProcessorToggles, PreDrainRule, DisplayLine, TracebackBlock, TableBlock, MotifGroup } from './types'
import { DEFAULT_PRE_DRAIN_RULES } from './types'
import { Drain, compilePreDrainRules, type CompiledPreDrainRule } from './drain'
import { processSegment } from './pipeline'
import { detectMotifs } from './motifs'
import { TQDM_RE, TB_FILE, TB_START, TABLE_LINE_RE } from './lex'

const PENDING_WINDOW = 250
export const MOTIF_THROTTLE_MS = 2000

/** A raw line at which it is safe to cut segments: neither it nor its
 *  neighbour can participate in a fold or multi-line structure. */
function safeCut(prev: string | undefined, line: string): boolean {
  const plain = (s: string) =>
    !s.includes('\r') && s.trim() !== '' && !TQDM_RE.test(s) &&
    !TB_START.test(s) && !TB_FILE.test(s) && !TABLE_LINE_RE.test(s)
  return plain(line) && (prev === undefined || plain(prev))
}

export class IncrementalLogPipeline {
  private stable: DisplayLine[] = []
  private stableTb: TracebackBlock[] = []
  private stableTables: TableBlock[] = []
  private pendingRaw: string[] = []
  private pendingDisplay: DisplayLine[] = []
  private pendingTb: TracebackBlock[] = []
  private pendingTables: TableBlock[] = []
  private pendingCids: number[] = []
  private drain = new Drain()
  private motifs: MotifGroup[] = []
  private motifsAt = 0
  private motifsDirty = false

  private toggles: ProcessorToggles
  private compiled: CompiledPreDrainRule[]

  constructor(toggles: ProcessorToggles, rules: PreDrainRule[] = DEFAULT_PRE_DRAIN_RULES) {
    this.toggles = { ...toggles }
    this.compiled = compilePreDrainRules(rules)
  }

  /** Full rebuild — used for initial load, trim, and toggle/rule changes. */
  reset(allRaw: string[], toggles?: ProcessorToggles, rules?: PreDrainRule[]): void {
    if (toggles) this.toggles = { ...toggles }
    if (rules) this.compiled = compilePreDrainRules(rules)
    this.stable = []
    this.stableTb = []
    this.stableTables = []
    this.pendingRaw = []
    this.pendingDisplay = []
    this.pendingTb = []
    this.pendingTables = []
    this.pendingCids = []
    this.drain = new Drain()
    this.push(allRaw)
    this.recomputeMotifs()
  }

  /** Append new raw lines; only the pending tail is re-processed. */
  push(newRaw: string[]): void {
    if (newRaw.length > 0) this.pendingRaw.push(...newRaw)
    this.finalizeIfLarge()
    this.reprocessPending()
    this.motifsDirty = true
    if (Date.now() - this.motifsAt >= MOTIF_THROTTLE_MS) this.recomputeMotifs()
  }

  /** Recompute motif groups over the whole window (throttled by push). */
  recomputeMotifs(): void {
    this.motifs = detectMotifs(this.lines(), this.drain)
    this.motifsAt = Date.now()
    this.motifsDirty = false
  }

  get motifsStale(): boolean { return this.motifsDirty }

  private lines(): DisplayLine[] {
    return this.stable.length === 0
      ? this.pendingDisplay
      : this.stable.concat(this.pendingDisplay)
  }

  result(): PipelineResult {
    if (this.motifsDirty && Date.now() - this.motifsAt >= MOTIF_THROTTLE_MS) {
      this.recomputeMotifs()
    }
    return {
      lines: this.lines(),
      motifGroups: this.motifs,
      tracebacks: this.stableTb.concat(this.pendingTb),
      tableBlocks: this.stableTables.concat(this.pendingTables),
      drain: this.drain,
    }
  }

  /** Move the head of an oversized pending tail into stable, cutting at a
   *  safe raw line so no fold/structure spans the boundary. */
  private finalizeIfLarge(): void {
    if (this.pendingRaw.length <= PENDING_WINDOW * 2) return
    let cut = this.pendingRaw.length - PENDING_WINDOW
    while (cut > 0 && !safeCut(this.pendingRaw[cut - 1], this.pendingRaw[cut])) cut--
    if (cut === 0) return // no safe boundary — keep growing, stay correct

    // Undo the pending tail's Drain contribution, then split.
    for (const cid of this.pendingCids) if (cid >= 0) this.drain.remove(cid)
    const head = this.pendingRaw.slice(0, cut)
    this.pendingRaw = this.pendingRaw.slice(cut)
    this.pendingCids = []
    this.pendingDisplay = []
    this.pendingTb = []
    this.pendingTables = []

    const seg = processSegment(head, this.toggles, this.compiled, this.drain, this.stable.length)
    const tbBase = this.stableTb.length
    for (const d of seg.display) if (d.tracebackIdx >= 0) d.tracebackIdx += tbBase
    this.stable.push(...seg.display)
    this.stableTb.push(...seg.tracebacks)
    this.stableTables.push(...seg.tableBlocks)
  }

  /** Re-run the pending tail through the segment pipeline (subtract old
   *  Drain contribution first so counts stay exact). */
  private reprocessPending(): void {
    for (const cid of this.pendingCids) if (cid >= 0) this.drain.remove(cid)
    const seg = processSegment(this.pendingRaw, this.toggles, this.compiled, this.drain, this.stable.length)
    const tbBase = this.stableTb.length
    for (const d of seg.display) if (d.tracebackIdx >= 0) d.tracebackIdx += tbBase
    this.pendingDisplay = seg.display
    this.pendingTb = seg.tracebacks
    this.pendingTables = seg.tableBlocks
    this.pendingCids = seg.cids
  }
}
