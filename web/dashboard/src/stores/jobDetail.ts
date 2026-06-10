import { defineStore } from 'pinia'
import { ref } from 'vue'
import { jobsApi } from '@/apis/jobs'
import { tasksApi } from '@/apis/tasks'
import { useConfigStore } from '@/stores/config'
import type { JobDetail, CompareRow, TaskView } from '@/types/api'

export const useJobDetailStore = defineStore('jobDetail', () => {
  const detail = ref<JobDetail | null>(null)
  const compare = ref<CompareRow[]>([])
  const loading = ref(false)

  // Tasks with a kill in flight (kill_async backends only). Frontend-local
  // optimistic state — NEVER persisted (design philosophy #1: the DB records
  // only confirmed facts; "cancelling" is a hope, so it lives and dies here).
  // Cleared per task as soon as a poll shows it left "running".
  const cancelling = ref(new Set<string>())

  /** Status to render for a task: overlays the transient cancelling state. */
  function displayStatus(t: TaskView): string {
    return cancelling.value.has(t.id) && t.status === 'running' ? 'cancelling' : t.status
  }

  function pruneCancelling() {
    if (cancelling.value.size === 0 || !detail.value) return
    const next = new Set<string>()
    for (const t of detail.value.tasks) {
      if (cancelling.value.has(t.id) && t.status === 'running') next.add(t.id)
    }
    cancelling.value = next
  }

  async function fetchDetail(jobId: string, silent = false) {
    loading.value = true
    try {
      detail.value = await jobsApi.get(jobId, { silent })
      pruneCancelling()
    } catch {
      // swallow
    } finally {
      loading.value = false
    }
  }

  async function fetchCompare(jobId: string, key: string, desc: boolean) {
    compare.value = await jobsApi.compare(jobId, key, desc)
  }

  async function killTask(taskId: string) {
    await tasksApi.kill(taskId)
    if (useConfigStore().killAsync) {
      cancelling.value = new Set([...cancelling.value, taskId])
    }
  }

  async function retryTask(taskId: string) {
    await tasksApi.retry(taskId)
  }

  async function killJob(jobId: string) {
    await jobsApi.kill(jobId)
    if (useConfigStore().killAsync && detail.value) {
      const next = new Set(cancelling.value)
      for (const t of detail.value.tasks) {
        if (t.status === 'running') next.add(t.id)
      }
      cancelling.value = next
    }
  }

  async function pauseJob(jobId: string) {
    await jobsApi.pause(jobId)
  }

  async function resumeJob(jobId: string) {
    await jobsApi.resume(jobId)
  }

  /** Manual reconcile (poll-model backends), then re-fetch the detail. */
  async function refreshJob(jobId: string) {
    await jobsApi.refresh(jobId)
    await fetchDetail(jobId, true)
  }

  function $reset() {
    detail.value = null
    compare.value = []
    loading.value = false
    cancelling.value = new Set()
  }

  return {
    detail, compare, loading, cancelling,
    displayStatus,
    fetchDetail, fetchCompare,
    killTask, retryTask, killJob, pauseJob, resumeJob, refreshJob,
    $reset,
  }
})
