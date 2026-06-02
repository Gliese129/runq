import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '../api/client'
import type { JobDetail, CompareRow, MatrixView } from '../api/types'

export const useJobDetailStore = defineStore('jobDetail', () => {
  const detail = ref<JobDetail | null>(null)
  const compare = ref<CompareRow[]>([])
  const matrix = ref<MatrixView | null>(null)
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

  async function fetchMatrix(jobId: string, rowKey: string, colKey: string, valueKey: string) {
    const params = new URLSearchParams({ row: rowKey, col: colKey, value: valueKey })
    matrix.value = await api.get<MatrixView>(`/jobs/${jobId}/matrix?${params}`)
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
    matrix.value = null
    loading.value = false
  }

  return {
    detail, compare, matrix, loading,
    fetchDetail, fetchCompare, fetchMatrix,
    killTask, retryTask, killJob, pauseJob, resumeJob,
    $reset,
  }
})
