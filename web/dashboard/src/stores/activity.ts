import { defineStore } from 'pinia'
import { ref } from 'vue'
import { jobsApi } from '@/apis/jobs'
import type { ActivityPoint } from '@/types/api'

export const useActivityStore = defineStore('activity', () => {
  const taskActivities = ref<Map<string, ActivityPoint[]>>(new Map())
  const jobTimeRange = ref<[number, number]>([0, 0])
  const loading = ref(false)

  async function fetchActivity(jobId: string) {
    loading.value = true
    try {
      const res = await jobsApi.activity(jobId)
      const map = new Map<string, ActivityPoint[]>()
      for (const t of res.tasks) {
        map.set(t.task_id, t.points)
      }
      taskActivities.value = map
      if (res.job_start != null && res.job_end != null) {
        jobTimeRange.value = [res.job_start, res.job_end]
      }
    } catch {
      // swallow
    } finally {
      loading.value = false
    }
  }

  function $reset() {
    taskActivities.value = new Map()
    jobTimeRange.value = [0, 0]
    loading.value = false
  }

  return { taskActivities, jobTimeRange, loading, fetchActivity, $reset }
})
