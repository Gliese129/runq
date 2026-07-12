import { ref } from 'vue'
import type { TaskView } from '@/types/api'

// Tasks with a kill in flight (kill_async backends only). Frontend-local
// optimistic state — NEVER persisted (design philosophy #1: the DB records
// only confirmed facts; "cancelling" is a hope, so it lives and dies here).
// Module-level singleton so EVERY page (list, job detail, task detail)
// renders the same transient state — the old copy lived inside the
// jobDetail store and the list pages never saw it.
const cancelling = ref(new Set<string>())

function markTask(taskId: string) {
  cancelling.value = new Set([...cancelling.value, taskId])
}

function markTasks(taskIds: string[]) {
  if (taskIds.length === 0) return
  cancelling.value = new Set([...cancelling.value, ...taskIds])
}

/**
 * Clear entries as soon as a fresh poll shows the task left "running".
 * Only tasks PRESENT in the payload are adjudicated — the set is global
 * across jobs, so a job page pruning with ITS task list (or a task page
 * pruning with a single task) must never touch entries it can't see.
 */
function prune(tasks: TaskView[]) {
  if (cancelling.value.size === 0 || tasks.length === 0) return
  const present = new Map(tasks.map(t => [t.id, t.status]))
  const next = new Set([...cancelling.value].filter(id => {
    const status = present.get(id)
    return status === undefined || status === 'running'
  }))
  if (next.size !== cancelling.value.size) cancelling.value = next
}

/** Status to render for a task: overlays the transient cancelling state. */
function displayStatus(t: TaskView): string {
  return cancelling.value.has(t.id) && t.status === 'running' ? 'cancelling' : t.status
}

export function useCancelling() {
  return { cancelling, markTask, markTasks, prune, displayStatus }
}
