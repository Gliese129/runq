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

export function applyPreDrainRules(line: string, rules: PreDrainRule[]): string {
  let out = line
  for (const rule of rules) {
    if (!rule.enabled) continue
    try { out = out.replace(new RegExp(rule.pattern, 'g'), rule.replacement) }
    catch { /* invalid regex — skip */ }
  }
  return out
}

interface DrainNode {
  id: number
  template: string[]
  count: number
}

export class Drain {
  private tree = new Map<string, DrainNode[]>()
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
    nodes.push({ id, template: [...tokens], count: 1 })
    return id
  }

  getTemplate(id: number): string {
    for (const nodes of this.tree.values()) {
      const n = nodes.find(n => n.id === id)
      if (n) return n.template.join(' ')
    }
    return ''
  }

  getTemplateTokens(id: number): string[] {
    for (const nodes of this.tree.values()) {
      const n = nodes.find(n => n.id === id)
      if (n) return n.template
    }
    return []
  }
}

