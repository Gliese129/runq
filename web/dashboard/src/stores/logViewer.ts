import { defineStore } from 'pinia'
import { ref, reactive } from 'vue'
import { jobsApi } from '@/apis/jobs'
import type { SearchMatch } from '@/types/api'
import type { ProcessorToggles, PreDrainRule } from '@/utils/logProcessors'
import { DEFAULT_PRE_DRAIN_RULES } from '@/utils/logProcessors'

export const useLogViewerStore = defineStore('logViewer', () => {
  const currentOffset = ref(0)
  const totalBytes = ref(0)
  const lines = ref<string[]>([])
  const searchQuery = ref('')
  const searchResults = ref<SearchMatch[]>([])
  const searchTruncated = ref(false)
  const processors = reactive<ProcessorToggles>({
    crFolder: true,
    tracebackFold: true,
    levelColoring: true,
    metricHighlight: true,
    rankColoring: true,
  })
  const preDrainRules = ref<PreDrainRule[]>(
    DEFAULT_PRE_DRAIN_RULES.map(r => ({ ...r })),
  )

  async function search(jobId: string, q: string, offset = 0) {
    searchQuery.value = q
    try {
      const res = await jobsApi.logSearch(jobId, q, offset)
      if (offset === 0) {
        searchResults.value = res.matches
      } else {
        searchResults.value = [...searchResults.value, ...res.matches]
      }
      searchTruncated.value = res.truncated
    } catch {
      // swallow
    }
  }

  function clearSearch() {
    searchQuery.value = ''
    searchResults.value = []
    searchTruncated.value = false
  }

  function toggleProcessor(name: string) {
    const key = name as keyof ProcessorToggles
    if (key in processors) {
      processors[key] = !processors[key]
    }
  }

  function addRule(rule: PreDrainRule) {
    preDrainRules.value.push(rule)
  }

  function updateRule(index: number, rule: PreDrainRule) {
    if (index >= 0 && index < preDrainRules.value.length) {
      preDrainRules.value[index] = rule
    }
  }

  function removeRule(index: number) {
    preDrainRules.value.splice(index, 1)
  }

  function toggleRule(index: number) {
    if (index >= 0 && index < preDrainRules.value.length) {
      preDrainRules.value[index].enabled = !preDrainRules.value[index].enabled
    }
  }

  function $reset() {
    currentOffset.value = 0
    totalBytes.value = 0
    lines.value = []
    searchQuery.value = ''
    searchResults.value = []
    searchTruncated.value = false
    Object.assign(processors, {
      crFolder: true,
      tracebackFold: true,
      levelColoring: true,
      metricHighlight: true,
      rankColoring: true,
    })
    preDrainRules.value = DEFAULT_PRE_DRAIN_RULES.map(r => ({ ...r }))
  }

  return {
    currentOffset, totalBytes, lines,
    searchQuery, searchResults, searchTruncated,
    processors, preDrainRules,
    search, clearSearch, toggleProcessor,
    addRule, updateRule, removeRule, toggleRule,
    $reset,
  }
})
