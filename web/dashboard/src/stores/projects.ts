import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/api/client'

export interface ProjectSummary {
  name: string
  work_dir?: string
  job_count: number
}

export const useProjectStore = defineStore('projects', () => {
  const list = ref<ProjectSummary[]>([])
  const loading = ref(false)
  const selected = ref<string | null>(null)

  async function fetch() {
    loading.value = true
    try {
      list.value = await api.get<ProjectSummary[]>('/projects')
    } catch {
      list.value = []
    } finally {
      loading.value = false
    }
  }

  function select(name: string | null) {
    selected.value = name
  }

  const current = computed(() =>
    list.value.find(p => p.name === selected.value) || null
  )

  return { list, loading, selected, current, fetch, select }
})
