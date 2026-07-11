import { describe, it, expect } from 'vitest'
import { linearSpace, logSpace, ratioSpace, aroundDefault, seedRange, parseGeneratorExpr, MAX_GENERATED } from './valueGenerators'

describe('linearSpace', () => {
  it('generates inclusive arithmetic steps', () => {
    expect(linearSpace(0, 1, 0.25, false)).toEqual(['0', '0.25', '0.5', '0.75', '1'])
  })
  it('rounds ints', () => {
    expect(linearSpace(1, 5, 2, true)).toEqual(['1', '3', '5'])
  })
  it('rejects bad input', () => {
    expect(linearSpace(5, 1, 1, false)).toEqual([])
    expect(linearSpace(0, 1, 0, false)).toEqual([])
  })
  it('caps at MAX_GENERATED', () => {
    expect(linearSpace(0, 1e6, 1, true).length).toBe(MAX_GENERATED)
  })
  // Regression: `v += step` accumulated float error and the absolute 1e-9
  // epsilon dropped endpoints — 0.1×3 = 0.30000000000000004 > 0.3 + 1e-9.
  it('keeps the endpoint despite float accumulation', () => {
    expect(linearSpace(0, 0.3, 0.1, false)).toEqual(['0', '0.1', '0.2', '0.3'])
    expect(linearSpace(0, 0.7, 0.1, false)).toContain('0.7')
  })
  it('keeps the endpoint at large magnitudes (relative epsilon)', () => {
    expect(linearSpace(0, 3e10, 1e10, false)).toEqual(['0', '10000000000', '20000000000', '30000000000'])
  })
})

describe('logSpace', () => {
  it('spaces evenly in log space without float artifacts', () => {
    expect(logSpace(1e-4, 1e-1, 4, false)).toEqual(['0.0001', '0.001', '0.01', '0.1'])
  })
  it('handles non-decade ranges cleanly', () => {
    const vals = logSpace(0.01, 0.04, 3, false)
    expect(vals).toEqual(['0.01', '0.02', '0.04'])
  })
  it('requires positive min/max and count ≥ 2', () => {
    expect(logSpace(0, 1, 3, false)).toEqual([])
    expect(logSpace(0.1, 0.01, 3, false)).toEqual([])
    expect(logSpace(0.01, 0.1, 1, false)).toEqual([])
  })
})

describe('ratioSpace', () => {
  it('powers of two for batch sizes', () => {
    expect(ratioSpace(32, 2, 4, true)).toEqual(['32', '64', '128', '256'])
  })
  it('fractional ratios', () => {
    expect(ratioSpace(1, 0.1, 3, false)).toEqual(['1', '0.1', '0.01'])
  })
  it('rejects ratio 1 and bad counts', () => {
    expect(ratioSpace(1, 1, 3, false)).toEqual([])
    expect(ratioSpace(1, 2, 0, false)).toEqual([])
  })
})

describe('aroundDefault', () => {
  it('probes around the default', () => {
    expect(aroundDefault(0.01, false)).toEqual(['0.0025', '0.005', '0.01', '0.02', '0.04'])
  })
  it('dedupes int collapse', () => {
    expect(aroundDefault(2, true)).toEqual(['1', '2', '4', '8'])
  })
  it('rejects zero default', () => {
    expect(aroundDefault(0, false)).toEqual([])
  })
})

describe('seedRange', () => {
  it('generates 0..n-1', () => {
    expect(seedRange(3)).toEqual(['0', '1', '2'])
  })
  it('rejects non-positive', () => {
    expect(seedRange(0)).toEqual([])
  })
})

describe('parseGeneratorExpr (syntax sugar)', () => {
  it('log min max count', () => {
    expect(parseGeneratorExpr('log 1 16 5', true)?.values).toEqual(['1', '2', '4', '8', '16'])
    expect(parseGeneratorExpr('log 1e-4 1e-1 4', false)?.values).toEqual(['0.0001', '0.001', '0.01', '0.1'])
  })
  it('linear min max step', () => {
    expect(parseGeneratorExpr('linear 1 5 1', true)?.values).toEqual(['1', '2', '3', '4', '5'])
    expect(parseGeneratorExpr('lin 0 1 0.5', false)?.values).toEqual(['0', '0.5', '1'])
  })
  it('ratio start ratio count', () => {
    expect(parseGeneratorExpr('ratio 32 2 4', true)?.values).toEqual(['32', '64', '128', '256'])
  })
  it('seeds n (with alias)', () => {
    expect(parseGeneratorExpr('seeds 3', true)?.values).toEqual(['0', '1', '2'])
    expect(parseGeneratorExpr('seed 2', true)?.values).toEqual(['0', '1'])
  })
  it('tolerates commas', () => {
    expect(parseGeneratorExpr('log 1, 16, 5', true)?.values).toEqual(['1', '2', '4', '8', '16'])
  })
  it('non-keyword input falls through (null)', () => {
    expect(parseGeneratorExpr('0.1 0.2 0.3', false)).toBeNull()
    expect(parseGeneratorExpr('hello 1 2', false)).toBeNull()
  })
  it('keyword with wrong arity previews empty, not null', () => {
    expect(parseGeneratorExpr('log 1 16', true)).toEqual({ keyword: 'log', values: [] })
    expect(parseGeneratorExpr('log', true)).toEqual({ keyword: 'log', values: [] })
  })
  it('keyword with garbage args is null (treated as plain input)', () => {
    expect(parseGeneratorExpr('log a b c', true)).toBeNull()
  })
})
