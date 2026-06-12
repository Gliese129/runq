import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { jobsApi } from '@/apis/jobs'
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
      jobs.value = await jobsApi.list({ silent })
    } catch {
      // swallow — connection state tracked globally
    } finally {
      loading.value = false
    }
  }

  // ── Archive (single frontend source of truth) ──
  // Explicitly-archived jobs, globally; and per-project SCOPED lists (the
  // scoped query skips the archived-project cascade — used by the page of
  // an archived project, whose jobs are absent from the global list).
  const archived = ref<JobSummary[]>([])
  const scoped = ref<Record<string, JobSummary[]>>({})

  async function fetchArchived() {
    try {
      archived.value = await jobsApi.listArchived()
    } catch { /* decoration — connection state tracked globally */ }
  }

  async function fetchScoped(project: string) {
    try {
      scoped.value[project] = await jobsApi.listByProject(project, { silent: true })
    } catch { /* decoration */ }
  }

  // Mutations own their refresh (see stores/projects.ts for the rationale).
  async function archiveJob(id: string) {
    await jobsApi.archive(id)
    await Promise.all([fetchJobs(true), fetchArchived()])
  }

  async function unarchiveJob(id: string) {
    await jobsApi.unarchive(id)
    await Promise.all([fetchJobs(true), fetchArchived()])
  }

  return {
    jobs, loading, projects, jobsByProject, totalRunning, totalPending, totalFailed, fetchJobs,
    archived, scoped, fetchArchived, fetchScoped, archiveJob, unarchiveJob,
  }
})
