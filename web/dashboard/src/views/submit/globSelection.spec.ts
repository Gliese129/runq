import { describe, it, expect } from 'vitest'
import { mergeGlobSelection } from './globSelection'

describe('mergeGlobSelection', () => {
  it('fresh row: selects everything', () => {
    const r = mergeGlobSelection([], [], ['a', 'b'])
    expect(r.next).toEqual(['a', 'b'])
    expect(r.missing).toEqual([])
  })

  it('hydrated snapshot (?fromJob/draft): keeps the frozen subset, never expands', () => {
    // Codex r1 F1 fixture: source job pinned only a.pt; today's scan
    // finds a/b/c — the re-run must reproduce the pinned batch.
    const r = mergeGlobSelection([], ['a.pt'], ['a.pt', 'b.pt', 'c.pt'])
    expect(r.next).toEqual(['a.pt'])
    expect(r.missing).toEqual([])
  })

  it('hydrated snapshot with vanished paths reports them', () => {
    const r = mergeGlobSelection([], ['a.pt', 'gone.pt'], ['a.pt', 'b.pt'])
    expect(r.next).toEqual(['a.pt'])
    expect(r.missing).toEqual(['gone.pt'])
  })

  it('rescan: keeps selection and adopts new arrivals only', () => {
    const r = mergeGlobSelection(['a', 'b', 'c'], ['b', 'c'], ['a', 'b', 'c', 'd'])
    expect(r.next).toEqual(['b', 'c', 'd']) // a stays deselected, d adopted
    expect(r.missing).toEqual([])
  })

  it('rescan with deletions drops and reports', () => {
    const r = mergeGlobSelection(['a', 'b'], ['a', 'b'], ['b'])
    expect(r.next).toEqual(['b'])
    expect(r.missing).toEqual(['a'])
  })
})
