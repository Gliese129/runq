import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import { useQuery, useMutation, useQueryClient } from '@tanstack/vue-query'
import { tasksApi } from '@/apis/tasks'
import { useConfigStore } from '@/stores/config'
import { useCancelling } from '@/composables/useCancelling'
import { qk } from './keys'

// `unknown` is active on purpose (RQ-74): the backend treats it as live
// work awaiting reconcile — the UI must keep polling so the page notices
// when reconcile settles it (running / terminal) without a manual refresh.
const ACTIVE_TASK = new Set(['running', 'pending', 'unknown'])

export function useTaskQuery(taskId: MaybeRefOrGetter<string>) {
  const config = useConfigStore()
  return useQuery({
    queryKey: computed(() => qk.task(toValue(taskId))),
    queryFn: ({ signal }) => tasksApi.get(toValue(taskId), { silent: true, signal }),
    refetchInterval: (query) => {
      const s = query.state.data?.status
      return s && ACTIVE_TASK.has(s) ? (config.isPoll ? 30_000 : 3_000) : false
    },
  })
}

/** Metric points; polls alongside the task while it is active. */
export function useTaskMetricsQuery(
  taskId: MaybeRefOrGetter<string>,
  active: MaybeRefOrGetter<boolean>,
) {
  return useQuery({
    queryKey: computed(() => qk.taskMetrics(toValue(taskId))),
    queryFn: () => tasksApi.metrics(toValue(taskId)),
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
