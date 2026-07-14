// Drain log clustering + pre-Drain normalization rules (step 4).
import type { PreDrainRule } from './types'

// ── Step ④: Drain ───────────────────────────────────────────

/**
 * Pre-tokenize normalizer: replaces known variable patterns with
 * typed placeholders so Drain matches on first sight.
 */
export function normalizeLine(line: string): string {
  return line
    // Pad separators so Drain tokenizes on them → finer templates
    .replace(/\|/g, ' | ')
    .replace(/,(?!\d)/g, ' , ')
    .replace(/\b[0-9a-f]{8,}(?:-[0-9a-f]{4,})*\b/gi, '<HEX>')
    .replace(/(?:\/[\w._-]+){2,}(?:\.\w+)?/g, '<PATH>')
    .replace(/s3:\/\/\S+/gi, '<PATH>')
    .replace(/hdfs:\/\/\S+/gi, '<PATH>')
    .replace(/\b\d+\.\d+(?:[eE][+-]?\d+)?\b/g, '<NUM>')
    .replace(/\b\d{2,}\b/g, '<NUM>')
}

/** A user rule with its pattern compiled once. */
export interface CompiledPreDrainRule {
  re: RegExp
  replacement: string
}

/**
 * Compile enabled rules once per pipeline run. The old per-line path
 * constructed `new RegExp` for every line × every rule — on a 50k-line
 * log with 5 rules that is 250k compilations per recompute, and it
 * dominated pipeline cost. Invalid patterns are skipped here, so the
 * hot loop needs no try/catch.
 */
export function compilePreDrainRules(rules: PreDrainRule[]): CompiledPreDrainRule[] {
  const out: CompiledPreDrainRule[] = []
  for (const rule of rules) {
    if (!rule.enabled) continue
    try {
      out.push({ re: new RegExp(rule.pattern, 'g'), replacement: rule.replacement })
    } catch { /* invalid regex — skip */ }
  }
  return out
}

/** Hot path: apply pre-compiled rules to one line. (String.replace with a
 *  global regex always scans from 0 and resets lastIndex — reuse is safe.) */
export function applyCompiledPreDrainRules(line: string, compiled: CompiledPreDrainRule[]): string {
  let out = line
  for (const r of compiled) out = out.replace(r.re, r.replacement)
  return out
}

/** Convenience wrapper for one-off callers and tests; the pipeline itself
 *  compiles once and uses applyCompiledPreDrainRules in the loop. */
export function applyPreDrainRules(line: string, rules: PreDrainRule[]): string {
  return applyCompiledPreDrainRules(line, compilePreDrainRules(rules))
}

interface DrainNode {
  id: number
  key: string // tree bucket, kept for O(1) removal
  template: string[]
  count: number
}

/**
 * Streaming Drain clusterer with WINDOWED semantics: parse() adds a line,
 * remove() subtracts one by cluster id (refcount). Removal is
 * count-accurate but template-width-conservative — a template widened to
 * `<*>` by a since-removed line never re-narrows (un-generalizing is
 * unsound). Harmless for fold/motif display; do not treat template
 * wildcards as an exact reflection of the current window.
 */
export class Drain {
  private tree = new Map<string, DrainNode[]>()
  private byId = new Map<number, DrainNode>()
  private nextId = 0
  private simTh: number

  constructor(simThreshold = 0.5) { this.simTh = simThreshold }

  parse(line: string): number {
    const normalized = normalizeLine(line)
    const tokens = normalized.trim().split(/\s+/)
    if (tokens.length === 0) return -1

    const key = `${tokens.length}:${tokens[0]}`
    let nodes = this.tree.get(key)
    if (!nodes) { nodes = []; this.tree.set(key, nodes) }

    let best: DrainNode | null = null
    let bestSim = 0
    for (const node of nodes) {
      if (node.template.length !== tokens.length) continue
      let match = 0
      for (let j = 0; j < tokens.length; j++) {
        if (node.template[j] === '<*>' || node.template[j] === tokens[j]) match++
      }
      const sim = match / tokens.length
      if (sim >= this.simTh && sim > bestSim) { bestSim = sim; best = node }
    }

    if (best) {
      best.template = best.template.map((t, j) =>
        t === '<*>' || t !== tokens[j] ? '<*>' : t,
      )
      best.count++
      return best.id
    }

    const id = this.nextId++
    const node: DrainNode = { id, key, template: [...tokens], count: 1 }
    nodes.push(node)
    this.byId.set(id, node)
    return id
  }

  /** Subtract one line from its cluster (windowed trim / re-parse of the
   *  pending tail). Refcounted; empty clusters are dropped entirely. */
  remove(id: number): void {
    const node = this.byId.get(id)
    if (!node) return
    if (--node.count > 0) return
    this.byId.delete(id)
    const bucket = this.tree.get(node.key)
    if (bucket) {
      const i = bucket.indexOf(node)
      if (i >= 0) bucket.splice(i, 1)
      if (bucket.length === 0) this.tree.delete(node.key)
    }
  }

  count(id: number): number {
    return this.byId.get(id)?.count ?? 0
  }

  getTemplate(id: number): string {
    return this.byId.get(id)?.template.join(' ') ?? ''
  }

  getTemplateTokens(id: number): string[] {
    return this.byId.get(id)?.template ?? []
  }
}

