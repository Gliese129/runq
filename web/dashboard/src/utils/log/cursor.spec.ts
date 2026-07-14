import { describe, it, expect } from 'vitest'
import { applyPage, type CursorState } from './cursor'
import type { LogPage } from '@/types/api'

function page(p: Partial<LogPage>): LogPage {
  return {
    lines: [],
    offset: 0,
    next_offset: 0,
    size: 0,
    total_lines: -1,
    ...p,
  }
}

const state = (endOffset: number, tailPartial = false): CursorState => ({ endOffset, tailPartial })

describe('log cursor state machine', () => {
  // ── Normal paths ──

  it('appends the page that continues the buffer', () => {
    const a = applyPage(state(100), page({ offset: 100, next_offset: 150, size: 200, lines: ['a', 'b'] }))
    expect(a).toEqual({
      kind: 'append', lines: ['a', 'b'], mergeFirst: false,
      nextOffset: 150, size: 200, tailPartial: false,
    })
  })

  it('merges a continues page onto a partial tail', () => {
    const a = applyPage(
      state(100, true),
      page({ offset: 100, next_offset: 160, size: 200, lines: ['tail-rest', 'next'], continues: true }),
    )
    expect(a.kind).toBe('append')
    if (a.kind === 'append') {
      expect(a.mergeFirst).toBe(true)
      expect(a.tailPartial).toBe(false)
    }
  })

  it('propagates partial (fragment chain keeps going)', () => {
    const a = applyPage(
      state(100, true),
      page({ offset: 100, next_offset: 160, size: 500, lines: ['frag'], continues: true, partial: true }),
    )
    expect(a.kind).toBe('append')
    if (a.kind === 'append') expect(a.tailPartial).toBe(true)
  })

  it('does not merge a continues page when the tail is complete (tail-open inside a mega-line)', () => {
    const a = applyPage(
      state(100, false),
      page({ offset: 100, next_offset: 160, size: 200, lines: ['frag'], continues: true }),
    )
    expect(a.kind).toBe('append')
    if (a.kind === 'append') expect(a.mergeFirst).toBe(false)
  })

  it('ignores the empty steady-state page', () => {
    const a = applyPage(state(100), page({ offset: 100, next_offset: 100, size: 100 }))
    expect(a.kind).toBe('ignore')
  })

  // ── The four failure chains (frozen contract §14) ──

  it('half-line race: a stale continuation straddling the cursor resyncs', () => {
    // SSE delivered a fragment to offset 100; a raced GET already applied
    // the continuation and moved the cursor to 120. The late SSE page
    // [100, 130) overlaps the cursor — applying it would duplicate bytes.
    const a = applyPage(
      state(120, false),
      page({ offset: 100, next_offset: 130, size: 200, lines: ['x'], continues: true }),
    )
    expect(a.kind).toBe('resync')
  })

  it('rotation: rotated page resets regardless of offsets', () => {
    const a = applyPage(
      state(500, true),
      page({ offset: 0, next_offset: 30, size: 30, lines: ['fresh'], rotated: true }),
    )
    expect(a).toEqual({
      kind: 'reset', lines: ['fresh'], nextOffset: 30, size: 30, tailPartial: false,
    })
  })

  it('rotation to an empty file still resets (clears the buffer)', () => {
    const a = applyPage(state(500), page({ offset: 0, next_offset: 0, size: 0, rotated: true }))
    expect(a.kind).toBe('reset')
    if (a.kind === 'reset') expect(a.lines).toEqual([])
  })

  it('out-of-order: a gap ahead of the cursor resyncs (page is not dropped silently)', () => {
    const a = applyPage(state(100), page({ offset: 300, next_offset: 400, size: 400, lines: ['later'] }))
    expect(a.kind).toBe('resync')
  })

  it('duplicate page fully behind the cursor is ignored', () => {
    const a = applyPage(state(200), page({ offset: 100, next_offset: 200, size: 300, lines: ['dup'] }))
    expect(a.kind).toBe('ignore')
    const b = applyPage(state(200), page({ offset: 50, next_offset: 120, size: 300, lines: ['older'] }))
    expect(b.kind).toBe('ignore')
  })
})
