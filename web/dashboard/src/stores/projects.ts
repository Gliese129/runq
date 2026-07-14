import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { projectsApi } from '@/apis/projects'
import { queryClient } from '@/queries/client'
import { qk } from '@/queries/keys'
import type { ProjectSummary } from '@/types/api'

export const useProjectStore = defineStore('projects', () => {
  const list = ref<ProjectSummary[]>([])
  const loading = ref(false)
  const selected = ref<string | null>(null)

  async function fetch() {
    loading.value = true
    try {
      list.value = await projectsApi.list()
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

  // Default views hide archived projects (Overview's archived row is the
  // recovery entry); `list` keeps everything for callers that need it.
  const visible = computed(() => list.value.filter(p => !p.archived))
  const archived = computed(() => list.value.filter(p => p.archived))

  // Mutations live HERE and own their refresh: every consumer reading the
  // computeds above stays consistent for free. Components must not call
  // projectsApi.archive/unarchive directly — that's how the "sidebar lags
  // the toggle" class of bug was born.
  async function archive(name: string) {
    await projectsApi.archive(name)
    await afterArchiveMutation()
  }

  async function unarchive(name: string) {
    await projectsApi.unarchive(name)
    await afterArchiveMutation()
  }

  async function afterArchiveMutation() {
    // Project archive cascades over job visibility — refresh the project
    // list AND invalidate every job-list query variant.
    await Promise.all([
      fetch(),
      queryClient.invalidateQueries({ queryKey: qk.jobs }),
    ])
  }

  return { list, visible, archived, loading, selected, current, fetch, select, archive, unarchive }
})
