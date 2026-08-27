// overviewView.spec — pins the attention-signature contract (ack survives
// only while the situation is unchanged), the additive unknown rule, and
// the matrix row derivation (configured ∪ used targets).
import { describe, it, expect } from 'vitest'
import type { JobSummary } from '@/types/api'
import { attentionItems, splitAcked, targetMatrix, toggleCell, applyFilter } from './overviewView'

const NOW = 1_000_000

function job(over: Partial<JobSummary> & { id: string }): JobSummary {
  return {
    archived: false, project: 'p', note: '', status: 'done', target: 'local',
    created_at: NOW - 100,
    tasks: { total: 4, pending: 0, running: 0, completed: 4, failed: 0 },
    ...over,
    // deep-merge tasks so callers override single counts
  } as JobSummary
}

describe('attentionItems', () => {
  it('collects failures and unknowns from the last 24h only', () => {
    const items = attentionItems([
      job({ id: 'a', status: 'failed', tasks: { total: 3, pending: 0, running: 0, completed: 0, failed: 3 } }),
      job({ id: 'b', status: 'partial', tasks: { total: 4, pending: 0, running: 0, completed: 3, failed: 1 } }),
      job({ id: 'old', status: 'failed', created_at: NOW - 90_000, tasks: { total: 1, pending: 0, running: 0, completed: 0, failed: 1 } }),
      job({ id: 'fine' }),
    ], NOW)
    expect(items.map(x => x.key)).toEqual(['a', 'b'])
    expect(items[0].tone).toBe('error')
    expect(items[1].tone).toBe('warning')
  })

  it('unknown is additive, never shadowed by the failure branch', () => {
    const [item] = attentionItems([
      job({ id: 'u', status: 'partial', tasks: { total: 5, pending: 0, running: 0, completed: 2, failed: 2, unknown: 1 } }),
    ], NOW)
    expect(item.textKey).toBe('overview.attn_some_failed_unknown')
    expect(item.sig).toBe('partial:2:1')
    const [pure] = attentionItems([
      job({ id: 'u2', status: 'running', tasks: { total: 5, pending: 0, running: 2, completed: 2, failed: 0, unknown: 1 } }),
    ], NOW)
    expect(pure.textKey).toBe('overview.attn_unknown')
    expect(pure.tone).toBe('warning')
  })
})

describe('splitAcked', () => {
  it('an ack holds only while the signature matches', () => {
    const items = attentionItems([
      job({ id: 'a', status: 'partial', tasks: { total: 4, pending: 0, running: 0, completed: 3, failed: 1 } }),
    ], NOW)
    expect(splitAcked(items, { a: items[0].sig }).open).toHaveLength(0)
    expect(splitAcked(items, { a: 'partial:0:0' }).open).toHaveLength(1) // situation changed → reopens
  })
})

describe('targetMatrix', () => {
  const targets = [
    { name: 'local', type: 'local' as const, scheduler: '', capabilities: {} as any },
    { name: 'idle-hpc', type: 'remote' as const, scheduler: 'slurm', capabilities: {} as any },
  ]
  const jobs = [
    job({ id: '1', status: 'running', target: 'local' }),
    job({ id: '2', status: 'failed', target: 'local' }),
    job({ id: '3', status: 'done', target: 'retired' }),
  ]

  it('rows = configured ∪ used targets; empty targets still show', () => {
    const { rows } = targetMatrix(jobs, targets, [])
    expect(rows.map(r => r.name)).toEqual(['local', 'idle-hpc', 'retired'])
    expect(rows[1].total).toBe(0)
  })

  it('status columns include only statuses that occur; grand totals sum all', () => {
    const { statusCols, grand } = targetMatrix(jobs, targets, [])
    expect(statusCols).toEqual(['running', 'done', 'failed'])
    expect(grand).toEqual({ running: 1, failed: 1, done: 1 })
  })

  it('daemon targets carry free/total GPUs; others honestly show none', () => {
    const gpus = [
      { index: 0, name: 'A100', mem_total_mb: 0, mem_used_mb: 0, util_percent: 0, target: 'local', task_id: 'tk1' },
      { index: 1, name: 'A100', mem_total_mb: 0, mem_used_mb: 0, util_percent: 0, target: 'local' },
    ]
    const { rows } = targetMatrix(jobs, targets, gpus)
    expect(rows[0].gpus).toEqual({ free: 1, total: 2 })
    expect(rows[1].gpus).toBeNull()
  })
})

describe('filter', () => {
  it('toggleCell toggles the same cell off, replaces a different one', () => {
    let f = toggleCell({ target: '', status: '' }, 'local', 'failed')
    expect(f).toEqual({ target: 'local', status: 'failed' })
    f = toggleCell(f, 'local', 'failed')
    expect(f).toEqual({ target: '', status: '' })
  })

  it('applyFilter intersects both axes and sorts newest first', () => {
    const jobs = [
      job({ id: '1', status: 'failed', target: 'local', created_at: NOW - 50 }),
      job({ id: '2', status: 'failed', target: 'hpc', created_at: NOW - 10 }),
      job({ id: '3', status: 'done', target: 'local', created_at: NOW - 5 }),
    ]
    expect(applyFilter(jobs, { target: 'local', status: 'failed' }).map(j => j.id)).toEqual(['1'])
    expect(applyFilter(jobs, { target: '', status: 'failed' }).map(j => j.id)).toEqual(['2', '1'])
  })
})
