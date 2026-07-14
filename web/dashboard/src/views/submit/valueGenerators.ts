// valueGenerators — pure numeric sequence generators for the ⚡ menu in
// value cells. All outputs are display-ready strings: significant-digit
// rounding is built in so geomspace never leaks 0.020000000000000004.
//
// Modes mirror what researchers actually reach for:
//   linear  — arithmetic steps (min..max by step)
//   log     — geometric, evenly spaced in log space (lr, weight_decay)
//   ratio   — start × ratio^k (batch sizes: 32,64,128,...)
//   around  — default × {0.25, 0.5, 1, 2, 4} (probe around a known-good value)
//   seeds   — 0..n-1 (multi-seed runs)

export const MAX_GENERATED = 50

function cleanNumber(v: number, isInt: boolean): string {
  if (isInt) return String(Math.round(v))
  // 10 significant digits kills float artifacts while preserving 1e-7 scales
  return String(parseFloat(v.toPrecision(10)))
}

export function linearSpace(min: number, max: number, step: number, isInt: boolean): string[] {
  if (!(step > 0) || min > max) return []
  const out: string[] = []
  // min + i*step (not v += step): accumulation compounds float error, and
  // the old absolute 1e-9 epsilon dropped the endpoint at large magnitudes
  // (one ulp of 1e10 is already > 1e-9). Epsilon scales with step instead.
  for (let i = 0; out.length < MAX_GENERATED; i++) {
    const v = min + i * step
    if (v > max + step * 1e-9) break
    out.push(cleanNumber(v, isInt))
  }
  return dedupe(out)
}

export function logSpace(min: number, max: number, count: number, isInt: boolean): string[] {
  if (!(min > 0) || !(max > 0) || min >= max || count < 2) return []
  const n = Math.min(Math.floor(count), MAX_GENERATED)
  const lmin = Math.log(min)
  const lmax = Math.log(max)
  const out: string[] = []
  for (let i = 0; i < n; i++) {
    out.push(cleanNumber(Math.exp(lmin + ((lmax - lmin) * i) / (n - 1)), isInt))
  }
  return dedupe(out)
}

export function ratioSpace(start: number, ratio: number, count: number, isInt: boolean): string[] {
  if (!Number.isFinite(start) || !(ratio > 0) || ratio === 1 || count < 1) return []
  const n = Math.min(Math.floor(count), MAX_GENERATED)
  const out: string[] = []
  let v = start
  for (let i = 0; i < n; i++) {
    out.push(cleanNumber(v, isInt))
    v *= ratio
  }
  return dedupe(out)
}

export const AROUND_FACTORS = [0.25, 0.5, 1, 2, 4]

export function aroundDefault(def: number, isInt: boolean, factors = AROUND_FACTORS): string[] {
  if (!Number.isFinite(def) || def === 0) return []
  return dedupe(factors.map(f => cleanNumber(def * f, isInt)))
}

export function seedRange(n: number): string[] {
  const count = Math.min(Math.floor(n), MAX_GENERATED)
  if (count < 1) return []
  return Array.from({ length: count }, (_, i) => String(i))
}

function dedupe(values: string[]): string[] {
  return [...new Set(values)]
}

// ── Generator syntax sugar ──
// Typed directly into a numeric value cell: `log 1 16 5`, `linear 1 5 1`,
// `ratio 32 2 4`, `seeds 3`. Same engines as the ⚡ menu — sugar and popover
// can never drift apart. `rand` is deliberately absent until the backend
// Distribution semantics are designed (sugar must not outrun semantics).
//
// Interception is loss-free: it only applies to int/float cells, where a
// bare keyword would be an invalid value anyway.

export interface GeneratorExpr {
  keyword: string
  values: string[]
}

export function parseGeneratorExpr(input: string, isInt: boolean): GeneratorExpr | null {
  const tokens = input.trim().split(/[,\s]+/).filter(Boolean)
  if (tokens.length < 2) {
    // `seeds 3` is the shortest form; a lone keyword previews nothing
    if (tokens.length === 1 && isKeyword(tokens[0])) return { keyword: normKeyword(tokens[0]), values: [] }
    return null
  }
  const keyword = normKeyword(tokens[0])
  if (!keyword) return null
  const args = tokens.slice(1).map(Number)
  if (args.some(n => !Number.isFinite(n))) return null

  switch (keyword) {
    case 'log':
      if (args.length !== 3) return { keyword, values: [] }
      return { keyword, values: logSpace(args[0], args[1], args[2], isInt) }
    case 'linear':
      if (args.length !== 3) return { keyword, values: [] }
      return { keyword, values: linearSpace(args[0], args[1], args[2], isInt) }
    case 'ratio':
      if (args.length !== 3) return { keyword, values: [] }
      return { keyword, values: ratioSpace(args[0], args[1], args[2], isInt) }
    case 'seeds':
      if (args.length !== 1) return { keyword, values: [] }
      return { keyword, values: seedRange(args[0]) }
    default:
      return null
  }
}

const KEYWORD_ALIASES: Record<string, string> = {
  log: 'log', logspace: 'log', geom: 'log',
  linear: 'linear', lin: 'linear', linspace: 'linear',
  ratio: 'ratio', mul: 'ratio',
  seeds: 'seeds', seed: 'seeds',
}

function isKeyword(t: string): boolean {
  return t.toLowerCase() in KEYWORD_ALIASES
}

function normKeyword(t: string): string {
  return KEYWORD_ALIASES[t.toLowerCase()] ?? ''
}
