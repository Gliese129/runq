import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '../api/client'
import type { JobDetail, CompareRow } from '../api/types'

export const useJobDetailStore = defineStore('jobDetail', () => {
  const detail = ref<JobDetail | null>(null)
  const compare = ref<CompareRow[]>([])
  const loading = ref(false)

  async function fetchDetail(jobId: string, silent = false) {
    loading.value = true
    try {
      detail.value = await api.get<JobDetail>(`/jobs/${jobId}`, { silent })
    } catch {
      // swallow
    } finally {
      loading.value = false
    }
  }

  async function fetchCompare(jobId: string, key: string, desc: boolean) {
    const order = desc ? 'desc' : 'asc'
    compare.value = await api.get<CompareRow[]>(`/jobs/${jobId}/compare?key=${encodeURIComponent(key)}&order=${order}`)
  }

  async function killTask(taskId: string) {
    await api.post(`/tasks/${taskId}/kill`)
  }

  async function retryTask(taskId: string) {
    await api.post(`/tasks/${taskId}/retry`)
  }

  async function killJob(jobId: string) {
    await api.post(`/jobs/${jobId}/kill`)
  }

  async function pauseJob(jobId: string) {
    await api.post(`/jobs/${jobId}/pause`)
  }

  async function resumeJob(jobId: string) {
    await api.post(`/jobs/${jobId}/resume`)
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
