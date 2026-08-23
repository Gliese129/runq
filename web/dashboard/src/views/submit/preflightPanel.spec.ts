import { describe, it, expect } from 'vitest'
import { orderChecks } from './preflightPanel'

describe('orderChecks', () => {
  it('puts problems first', () => {
    const out = orderChecks([
      { name: 'a', status: 'passed' },
      { name: 'b', status: 'failed' },
      { name: 'c', status: 'skipped' },
      { name: 'd', status: 'warning' },
    ] as any)
    expect(out.map(c => c.status)).toEqual(['failed', 'warning', 'passed', 'skipped'])
  })

  it('handles absent input and unknown statuses last', () => {
    expect(orderChecks(undefined)).toEqual([])
    const out = orderChecks([{ name: 'x', status: 'weird' }, { name: 'y', status: 'passed' }] as any)
    expect(out.map(c => c.name)).toEqual(['y', 'x'])
  })
})
