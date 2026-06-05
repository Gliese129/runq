import { defineStore } from 'pinia'
import { ref } from 'vue'
import { jobsApi } from '@/apis/jobs'
import { tasksApi } from '@/apis/tasks'
import type { JobDetail, CompareRow } from '@/types/api'

export const useJobDetailStore = defineStore('jobDetail', () => {
  const detail = ref<JobDetail | null>(null)
  const compare = ref<CompareRow[]>([])
  const loading = ref(false)

  async function fetchDetail(jobId: string, silent = false) {
    loading.value = true
    try {
      detail.value = await jobsApi.get(jobId, { silent })
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
  }

  async function retryTask(taskId: string) {
    await tasksApi.retry(taskId)
  }

  async function killJob(jobId: string) {
    await jobsApi.kill(jobId)
  }

  async function pauseJob(jobId: string) {
    await jobsApi.pause(jobId)
  }

  async function resumeJob(jobId: string) {
    await jobsApi.resume(jobId)
  }

  function $reset() {
    detail.value = null
    compare.value = []
    loading.value = false
  }

  return {
    detail, compare, loading,
    fetchDetail, fetchCompare,
    killTask, retryTask, killJob, pauseJob, resumeJob,
    $reset,
  }
})
