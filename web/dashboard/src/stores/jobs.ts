import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/apis/client'
import type { JobSummary } from '@/types/api'

export const useJobsStore = defineStore('jobs', () => {
  const jobs = ref<JobSummary[]>([])
  const loading = ref(false)

  const projects = computed(() => {
    const set = new Set(jobs.value.map(j => j.project))
    return [...set].sort()
  })

  const jobsByProject = computed(() => {
    const map = new Map<string, JobSummary[]>()
    for (const j of jobs.value) {
      const arr = map.get(j.project) || []
      arr.push(j)
      map.set(j.project, arr)
    }
    return map
  })

  // Aggregate counts across all jobs
  const totalRunning = computed(() => jobs.value.reduce((s, j) => s + j.tasks.running, 0))
  const totalPending = computed(() => jobs.value.reduce((s, j) => s + j.tasks.pending, 0))
  const totalFailed = computed(() => jobs.value.reduce((s, j) => s + j.tasks.failed, 0))

  async function fetchJobs(silent = false) {
    loading.value = true
    try {
      jobs.value = await api.get<JobSummary[]>('/jobs', { silent })
    } catch {
      // swallow — connection state tracked globally
    } finally {
      loading.value = false
    }
  }

  return { jobs, loading, projects, jobsByProject, totalRunning, totalPending, totalFailed, fetchJobs }
})
