import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import { useQuery, useMutation, useQueryClient } from '@tanstack/vue-query'
import { tasksApi } from '@/apis/tasks'
import { useConfigStore } from '@/stores/config'
import { useCancelling } from '@/composables/useCancelling'
import { qk } from './keys'
import type { JobDetail, TaskView } from '@/types/api'

// `unknown` is active on purpose (RQ-74): the backend treats it as live
// work awaiting reconcile — the UI must keep polling so the page notices
// when reconcile settles it (running / terminal) without a manual refresh.
const ACTIVE_TASK = new Set(['running', 'submitting', 'pending', 'unknown'])

export function useTaskQuery(
  taskId: MaybeRefOrGetter<string>,
  jobId?: MaybeRefOrGetter<string>,
) {
  const config = useConfigStore()
  const qc = useQueryClient()
  return useQuery({
    queryKey: computed(() => qk.task(toValue(taskId))),
    queryFn: ({ signal }) => tasksApi.get(toValue(taskId), { silent: true, signal }),
    // First paint from the job cache's task row (the page the user just
    // left): GET /tasks/{id} reconciles the job remotely and can take
    // seconds on a slow target — the identity/params/status the user
    // already saw must not wait for it. Detail-only fields (log_path,
    // command, …) are optional on TaskView and fill in when the real
    // response lands; isPlaceholderData distinguishes the two.
    placeholderData: (): TaskView | undefined => {
      const jid = jobId ? toValue(jobId) : ''
      if (!jid) return undefined
      return qc
        .getQueryData<JobDetail>(qk.job(jid))
        ?.tasks.find((tk) => tk.id === toValue(taskId))
    },
    refetchInterval: (query) => {
      const s = query.state.data?.status
      return s && ACTIVE_TASK.has(s) ? (config.isPoll ? 30_000 : 3_000) : false
    },
  })
}

/** Metric points; polls alongside the task while it is active. `enabled`
 *  gates the fetch itself (not just polling): the endpoint tails up to
 *  256 KB from the target, so hidden tabs must not pay for it. */
export function useTaskMetricsQuery(
  taskId: MaybeRefOrGetter<string>,
  active: MaybeRefOrGetter<boolean>,
  enabled: MaybeRefOrGetter<boolean> = true,
) {
  return useQuery({
    queryKey: computed(() => qk.taskMetrics(toValue(taskId))),
    queryFn: () => tasksApi.metrics(toValue(taskId)),
    enabled: computed(() => toValue(enabled)),
    refetchInterval: () => (toValue(active) ? 3_000 : false),
  })
}

export function useTaskActions(jobId?: MaybeRefOrGetter<string>) {
  const qc = useQueryClient()
  const config = useConfigStore()
  const { markTask } = useCancelling()

  const invalidateTask = (taskId: string) => {
    qc.invalidateQueries({ queryKey: qk.task(taskId) })
    const jid = jobId ? toValue(jobId) : ''
    if (jid) qc.invalidateQueries({ queryKey: qk.job(jid) })
    qc.invalidateQueries({ queryKey: qk.jobs })
  }

  const kill = useMutation({
    mutationFn: (taskId: string) => tasksApi.kill(taskId),
    onSuccess: (_d, taskId) => {
      if (config.killAsync) markTask(taskId)
      invalidateTask(taskId)
    },
  })

  const retry = useMutation({
    mutationFn: (p: { taskId: string; confirm?: boolean }) => tasksApi.retry(p.taskId, p.confirm),
    onSuccess: (_d, p) => invalidateTask(p.taskId),
  })

  return { kill, retry }
}
