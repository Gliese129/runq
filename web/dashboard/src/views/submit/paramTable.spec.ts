import { describe, it, expect } from 'vitest'
import {
  compile, decompile, taskCount, validateTable, rowEffect, inferType,
  type ParamRow, type LinkSet,
} from './paramTable'

const row = (name: string, type: string, values: string[], def = ''): ParamRow =>
  ({ name, type, default: def, values })

describe('compile', () => {
  it('multi-value rows form one grid block, typed', () => {
    const rows = [row('lr', 'float', ['0.001', '0.01']), row('bs', 'int', ['32', '64'])]
    const cfg = compile('p', '', rows, [])
    expect(cfg.sweep).toEqual([
      { method: 'grid', parameters: { lr: { values: [0.001, 0.01] }, bs: { values: [32, 64] } } },
    ])
    expect(cfg.fixed_params).toBeUndefined()
  })

  it('link set becomes a list block; others stay grid', () => {
    const rows = [
      row('optimizer', 'str', ['sgd', 'adam']),
      row('momentum', 'float', ['0.9', '0.0']),
      row('lr', 'float', ['0.1', '0.2', '0.3']),
    ]
    const sets: LinkSet[] = [{ id: 'a', members: ['optimizer', 'momentum'] }]
    const cfg = compile('p', '', rows, sets)
    expect(cfg.sweep).toHaveLength(2)
    expect(cfg.sweep[0]).toEqual({
      method: 'list',
      parameters: { optimizer: { values: ['sgd', 'adam'] }, momentum: { values: [0.9, 0.0] } },
    })
    expect(cfg.sweep[1].method).toBe('grid')
    expect(Object.keys(cfg.sweep[1].parameters)).toEqual(['lr'])
  })

  it('single value → fixed override; empty + default → fixed default; empty no default → omitted', () => {
    const rows = [
      row('epochs', 'int', ['100']),
      row('seed', 'int', [], '42'),
      row('tag', 'str', []),
    ]
    const cfg = compile('p', '', rows, [])
    expect(cfg.sweep).toEqual([])
    expect(cfg.fixed_params).toEqual({ epochs: 100, seed: 42 })
  })

  it('bool and blank values are handled', () => {
    const rows = [row('amp', 'bool', ['true', 'false']), row('pad', 'str', ['', '  '])]
    const cfg = compile('p', '', rows, [])
    expect(cfg.sweep[0].parameters.amp.values).toEqual([true, false])
    expect(cfg.fixed_params).toBeUndefined()
  })

  it('degenerate link set (1 member) is ignored — member falls back to grid', () => {
    const rows = [row('lr', 'float', ['0.1', '0.2'])]
    const cfg = compile('p', '', rows, [{ id: 'a', members: ['lr'] }])
    expect(cfg.sweep).toEqual([
      { method: 'grid', parameters: { lr: { values: [0.1, 0.2] } } },
    ])
  })

  it('two link sets cross-product as independent list blocks', () => {
    const rows = [
      row('a', 'int', ['1', '2']), row('b', 'int', ['3', '4']),
      row('c', 'str', ['x', 'y', 'z']), row('d', 'str', ['u', 'v', 'w']),
    ]
    const sets: LinkSet[] = [
      { id: 's1', members: ['a', 'b'] },
      { id: 's2', members: ['c', 'd'] },
    ]
    const cfg = compile('p', '', rows, sets)
    expect(cfg.sweep.map(b => b.method)).toEqual(['list', 'list'])
    expect(taskCount(rows, sets)).toBe(6)
  })
})

describe('taskCount', () => {
  it('matches backend expansion math', () => {
    const rows = [
      row('lr', 'float', ['0.001', '0.01', '0.1']),
      row('bs', 'int', ['32', '64']),
      row('optimizer', 'str', ['sgd', 'adam']),
      row('momentum', 'float', ['0.9', '0.0']),
      row('epochs', 'int', ['100']),
    ]
    const sets: LinkSet[] = [{ id: 'a', members: ['optimizer', 'momentum'] }]
    expect(taskCount(rows, sets)).toBe(3 * 2 * 2)
  })

  it('no sweep → 1 task', () => {
    expect(taskCount([row('epochs', 'int', ['100'])], [])).toBe(1)
  })

  it('linked member with 0 values → 0 tasks', () => {
    const rows = [row('a', 'int', ['1', '2']), row('b', 'int', [])]
    expect(taskCount(rows, [{ id: 's', members: ['a', 'b'] }])).toBe(0)
  })
})

describe('validateTable', () => {
  it('rejects type-invalid values', () => {
    const res = validateTable([row('lr', 'float', ['0.1', 'abc'])], [])
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.rowName).toBe('lr')
  })

  it('rejects unequal link lengths with counts in message', () => {
    const rows = [row('a', 'int', ['1', '2', '3']), row('b', 'int', ['1', '2'])]
    const res = validateTable(rows, [{ id: 's', members: ['a', 'b'] }])
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.rowName).toBe('b')
      expect(res.message).toContain('has 2')
      expect(res.message).toContain('expected 3')
    }
  })

  it('accepts equal link lengths and ignores degenerate sets', () => {
    const rows = [row('a', 'int', ['1', '2']), row('b', 'int', ['3', '4'])]
    expect(validateTable(rows, [{ id: 's', members: ['a', 'b'] }]).ok).toBe(true)
    expect(validateTable(rows, [{ id: 's', members: ['a'] }]).ok).toBe(true)
  })
})

describe('rowEffect', () => {
  it('classifies rows', () => {
    const sets: LinkSet[] = [{ id: 's', members: ['linked'] }]
    expect(rowEffect(row('x', 'int', []), [])).toBe('unset')
    expect(rowEffect(row('x', 'int', [], '42'), [])).toBe('fixed-default')
    expect(rowEffect(row('x', 'int', ['1']), [])).toBe('fixed')
    expect(rowEffect(row('x', 'int', ['1', '2']), [])).toBe('sweep')
    expect(rowEffect(row('linked', 'int', ['1']), sets)).toBe('linked')
  })
})

describe('decompile (re-run job as template)', () => {
  it('round-trips a mixed config', () => {
    const original = compile('p', 'note', [
      row('lr', 'float', ['0.001', '0.01', '0.1']),
      row('optimizer', 'str', ['sgd', 'adam']),
      row('momentum', 'float', ['0.9', '0.0']),
      row('epochs', 'int', ['100']),
    ], [{ id: 's', members: ['optimizer', 'momentum'] }])

    const { rows, linkSets } = decompile(original)

    expect(rows.map(r => r.name).sort()).toEqual(['epochs', 'lr', 'momentum', 'optimizer'])
    expect(linkSets).toHaveLength(1)
    expect(linkSets[0].members.sort()).toEqual(['momentum', 'optimizer'])
    expect(rows.find(r => r.name === 'lr')!.type).toBe('float')
    expect(rows.find(r => r.name === 'epochs')!.values).toEqual(['100'])

    // recompiling reproduces identical expansion semantics
    const recompiled = compile('p', 'note', rows, linkSets)
    expect(recompiled.fixed_params).toEqual(original.fixed_params)
    expect(recompiled.sweep).toEqual(expect.arrayContaining(original.sweep))
    expect(taskCount(rows, linkSets)).toBe(3 * 2)
  })

  it('accepts shorthand array parameter specs', () => {
    const { rows } = decompile({ sweep: [{ method: 'grid', parameters: { lr: [0.1, 0.2] } as any }] })
    expect(rows[0].values).toEqual(['0.1', '0.2'])
    expect(rows[0].type).toBe('float')
  })

  it('infers types from values', () => {
    expect(inferType([1, 2])).toBe('int')
    expect(inferType([0.1, 1])).toBe('float')
    expect(inferType([true])).toBe('bool')
    expect(inferType(['a', 1])).toBe('str')
  })
})
