// projectGroups.spec — pins the web-only-lens semantics: seeding is a
// suggestion (degenerate groupings start flat), removal releases members
// instead of deleting them, and sanitize never trusts ui.json shapes.
import { describe, it, expect } from 'vitest'
import {
  UNGROUPED, seedGroups, sanitizeGroups, assign, renameGroup,
  removeGroup, createGroup, toggleCollapsed, groupProjects, emptyState,
} from './projectGroups'

const projects = [
  { name: 'lm-base', work_dir: '/lab/nlp/lm-base', job_count: 4 },
  { name: 'lm-wide', work_dir: '/lab/nlp/lm-wide', job_count: 2 },
  { name: 'vit', work_dir: '/lab/vision/vit', job_count: 7 },
  { name: 'scratch', work_dir: '/scratch', job_count: 0 },
]

describe('seedGroups', () => {
  it('proposes groups from the work_dir parent segment', () => {
    const s = seedGroups(projects)
    expect(s.order).toEqual(['nlp', 'vision'])
    expect(s.map['lm-base']).toBe('nlp')
    expect(s.map.vit).toBe('vision')
    expect(s.map.scratch).toBe(UNGROUPED) // single segment → no parent
    expect(s.seeded).toBe(true)
  })

  it('a degenerate grouping (fewer than two groups) starts flat', () => {
    const s = seedGroups([
      { name: 'a', work_dir: '/lab/nlp/a' },
      { name: 'b', work_dir: '/lab/nlp/b' },
    ])
    expect(s.order).toEqual([])
  })
})

describe('sanitizeGroups', () => {
  it('rejects malformed documents', () => {
    expect(sanitizeGroups(null)).toBeNull()
    expect(sanitizeGroups({ order: 'nope', map: {} })).toBeNull()
  })

  it('drops unknown-group assignments and stale collapsed entries', () => {
    const s = sanitizeGroups({
      order: ['nlp', 7, ''],
      map: { a: 'nlp', b: 'gone', c: 3 },
      collapsed: ['nlp', 'gone'],
    })!
    expect(s.order).toEqual(['nlp'])
    expect(s.map).toEqual({ a: 'nlp', b: UNGROUPED })
    expect(s.collapsed).toEqual(['nlp'])
  })
})

describe('mutations', () => {
  const base = { order: ['nlp'], map: { a: 'nlp' }, collapsed: [] }

  it('assign moves a project and clears the seeded flag', () => {
    const s = assign({ ...base, seeded: true }, 'b', 'nlp')
    expect(s.map.b).toBe('nlp')
    expect(s.seeded).toBe(false)
    // Unknown target group is a no-op, not an invention.
    expect(assign(base, 'b', 'ghost')).toBe(base)
  })

  it('rename carries members and collapse state, refuses collisions', () => {
    const s = renameGroup({ order: ['nlp', 'cv'], map: { a: 'nlp' }, collapsed: ['nlp'] }, 'nlp', 'text')
    expect(s.order).toEqual(['text', 'cv'])
    expect(s.map.a).toBe('text')
    expect(s.collapsed).toEqual(['text'])
    expect(renameGroup(s, 'text', 'cv')).toBe(s) // collision → no-op
  })

  it('remove releases members to ungrouped', () => {
    const s = removeGroup(base, 'nlp')
    expect(s.order).toEqual([])
    expect(s.map.a).toBe(UNGROUPED)
  })

  it('create picks a fresh name', () => {
    const { next, name } = createGroup({ ...base, order: ['New group'] })
    expect(name).toBe('New group 2')
    expect(next.order).toContain('New group 2')
  })

  it('toggleCollapsed flips membership', () => {
    const s1 = toggleCollapsed(base, 'nlp')
    expect(s1.collapsed).toEqual(['nlp'])
    expect(toggleCollapsed(s1, 'nlp').collapsed).toEqual([])
  })
})

describe('groupProjects', () => {
  it('pivots the flat list, sums job_count, tails the ungrouped', () => {
    const s = { order: ['nlp'], map: { 'lm-base': 'nlp', 'lm-wide': 'nlp' }, collapsed: [] }
    const { groups, ungrouped } = groupProjects(projects, s)
    expect(groups).toHaveLength(1)
    expect(groups[0].members.map(p => p.name)).toEqual(['lm-base', 'lm-wide'])
    expect(groups[0].jobCount).toBe(6)
    expect(ungrouped.map(p => p.name)).toEqual(['vit', 'scratch'])
  })

  it('an empty state leaves everything ungrouped', () => {
    const { groups, ungrouped } = groupProjects(projects, emptyState())
    expect(groups).toHaveLength(0)
    expect(ungrouped).toHaveLength(4)
  })
})
