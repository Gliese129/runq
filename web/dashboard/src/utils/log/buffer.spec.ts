import { describe, it, expect } from 'vitest'
import { trimLogBuffer } from './buffer'

describe('trimLogBuffer', () => {
  it('does nothing below limit+hysteresis', () => {
    const lines = Array.from({ length: 23 }, (_, i) => `l${i}`)
    expect(trimLogBuffer(lines, 20, 4)).toBe(0)
    expect(lines.length).toBe(23)
  })

  it('trims back to limit once hysteresis is exceeded (batched cost)', () => {
    const lines = Array.from({ length: 25 }, (_, i) => `l${i}`)
    expect(trimLogBuffer(lines, 20, 4)).toBe(5)
    expect(lines.length).toBe(20)
    expect(lines[0]).toBe('l5') // oldest dropped, order preserved
    expect(lines[19]).toBe('l24')
  })

  it('trims in place (same array reference — the pipeline watch relies on it)', () => {
    const lines = Array.from({ length: 30 }, (_, i) => `l${i}`)
    const ref = lines
    trimLogBuffer(lines, 20, 4)
    expect(lines).toBe(ref)
  })
})
