// overviewView (RQ2-4 ⑤, kit ScreensA OverviewScreen) — pure logic for
// the overview's two upgraded blocks: the needs-attention list (the only
// block on the page allowed alarm colour, and the only one that
// disappears when there is nothing to say) and the targets × status
// matrix (where the work actually is, doubling as the page's filter).
import type { JobSummary, GPUSlot, TargetSummary } from '@/types/api'

// ── Needs attention ──

export interface AttentionItem {
  key: string
  /** Signature of the reported STATE — the ack is stored against this,
   *  so an acknowledged row comes back the moment the situation changes
   *  (more tasks fail, an unknown resolves into a failure). */
  sig: string
  tone: 'error' | 'warning'
  icon: string
  job: JobSummary
  /** i18n key + params — rendering stays in the component. */
  textKey: string
  textParams: Record<string, string | number>
}

const DAY = 86_400

/** Jobs from the last 24h a human has to decide about. `unknown` is a
 *  first-class state ("no verdict"), never shadowed by the failure
 *  branch — a job can carry both. Placement/drift rows are Phase 2. */
export function attentionItems(jobs: JobSummary[], now: number): AttentionItem[] {
  const out: AttentionItem[] = []
  for (const j of jobs) {
    if (now - j.created_at > DAY) continue
    const unknown = j.tasks.unknown ?? 0
    const allFailed = j.status === 'failed'
    const someFailed = !allFailed && j.tasks.failed > 0
    if (!allFailed && !someFailed && unknown === 0) continue
    let textKey: string
    if (allFailed) textKey = unknown > 0 ? 'overview.attn_all_failed_unknown' : 'overview.attn_all_failed'
    else if (someFailed) textKey = unknown > 0 ? 'overview.attn_some_failed_unknown' : 'overview.attn_some_failed'
    else textKey = 'overview.attn_unknown'
    out.push({
      key: j.id,
      sig: `${j.status}:${j.tasks.failed}:${unknown}`,
      tone: allFailed ? 'error' : 'warning',
      icon: allFailed ? 'mdi-alert-circle-outline' : someFailed ? 'mdi-alert-outline' : 'mdi-help-circle-outline',
      job: j,
      textKey,
      textParams: { project: j.project, failed: j.tasks.failed, total: j.tasks.total, unknown },
    })
  }
  return out
}

/** Split into open vs acknowledged. Acknowledging is not deleting: the
 *  stored signature must still match, otherwise the row reopens. */
export function splitAcked(items: AttentionItem[], acked: Record<string, string>) {
  const open = items.filter(x => acked[x.key] !== x.sig)
  return { open, ackedCount: items.length - open.length }
}

// ── Targets × status matrix ──

export const JOB_STATUS_COLS = ['running', 'pending', 'paused', 'done', 'partial', 'failed', 'killed'] as const

export interface TargetRow {
  name: string
  /** slurm | pbs | … | '' — '' with gpus=null means plain remote. */
  scheduler: string
  counts: Record<string, number>
  total: number
  /** free/total GPUs for daemon-managed targets; null = no visibility
   *  (scheduler targets honestly show none). */
  gpus: { free: number; total: number } | null
}

/** Rows come from the configured targets PLUS every target jobs actually
 *  ran on — a target with nothing on it still shows up empty ("there is
 *  room here" is information); a retired target with jobs still shows. */
export function targetMatrix(
  jobs: JobSummary[],
  targets: TargetSummary[],
  gpus: GPUSlot[],
): { rows: TargetRow[]; statusCols: string[]; grand: Record<string, number> } {
  const names = [...new Set([...targets.map(t => t.name), ...jobs.map(j => j.target).filter(Boolean)])]
  const rows: TargetRow[] = names.map(name => {
    const meta = targets.find(t => t.name === name)
    const mine = jobs.filter(j => j.target === name)
    const counts: Record<string, number> = {}
    for (const j of mine) counts[j.status] = (counts[j.status] || 0) + 1
    const slots = gpus.filter(g => g.target === name)
    return {
      name,
      scheduler: meta?.scheduler ?? '',
      counts,
      total: mine.length,
      gpus: slots.length > 0
        ? { free: slots.filter(g => !g.task_id).length, total: slots.length }
        : null,
    }
  })
  const grand: Record<string, number> = {}
  for (const j of jobs) grand[j.status] = (grand[j.status] || 0) + 1
  // Only status columns that exist somewhere — empty columns are noise.
  const statusCols = JOB_STATUS_COLS.filter(s => rows.some(r => r.counts[s]))
  return { rows, statusCols, grand }
}

export interface MatrixFilter {
  target: string
  status: string
}

/** One filter, two axes, both set by clicking a matrix cell. */
export function toggleCell(f: MatrixFilter, target: string, status: string): MatrixFilter {
  return f.target === target && f.status === status ? { target: '', status: '' } : { target, status }
}

export function applyFilter(jobs: JobSummary[], f: MatrixFilter): JobSummary[] {
  let list = jobs
  if (f.target) list = list.filter(j => j.target === f.target)
  if (f.status) list = list.filter(j => j.status === f.status)
  return [...list].sort((a, b) => b.created_at - a.created_at)
}
