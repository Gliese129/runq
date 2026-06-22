import { ref, computed, watch, nextTick, type Ref } from 'vue'
import { useLogViewerStore } from '@/stores/logViewer'
import type { DisplayLine, DrainBlockItem, GroupBlockItem } from '@/utils/logProcessors'
import {
  processLog,
  buildRenderItems,
  computeDefaultFoldState,
  segmentLine,
  LONG_LINE_THRESHOLD,
} from '@/utils/logProcessors'

/**
 * Shared log surface: the parse pipeline, unified fold state, motif-group
 * toggling, per-line render helpers, and in-pane search/scroll. Both the task
 * log view (TaskDetail) and the standalone log viewer (LogViewer) consume this
 * so their behavior can't drift apart — the duplicated copies had already
 * diverged (the group key/label mismatch). Route-specific loading and chrome
 * stay in the component; everything log-surface lives here. (RQ-12)
 *
 * @param logLines     reactive raw log lines (the component owns loading them)
 * @param logContainer the scroll container element, for scroll targeting
 */
export function useLogSurface(
  logLines: Ref<string[]>,
  logContainer: Ref<HTMLElement | undefined>,
) {
  const logStore = useLogViewerStore()

  // ── Fold / expand state ──
  const foldOverrides = ref(new Map<string, boolean>())
  const expandedLines = ref(new Set<number>())

  function isLong(line: DisplayLine): boolean {
    return line.text.length > LONG_LINE_THRESHOLD
  }

  function isExpanded(line: DisplayLine): boolean {
    return expandedLines.value.has(line.lineIdx)
  }

  function toggleExpand(line: DisplayLine) {
    if (expandedLines.value.has(line.lineIdx)) expandedLines.value.delete(line.lineIdx)
    else expandedLines.value.add(line.lineIdx)
  }

  function truncateText(text: string): string {
    return text.slice(0, LONG_LINE_THRESHOLD)
  }

  const pipelineResult = computed(() => processLog(logLines.value, logStore.processors, logStore.preDrainRules))

  /** Unified fold state: foldKey → collapsed. Merges auto-fold defaults with user overrides. */
  const foldState = computed(() => {
    const defaults = computeDefaultFoldState(pipelineResult.value)
    for (const [key, val] of foldOverrides.value) defaults.set(key, val)
    return defaults
  })

  /** Which group IDs have ALL instances collapsed (for side panel highlight) */
  const effectiveHidden = computed(() => {
    const hidden = new Set<number>()
    for (const g of pipelineResult.value.motifGroups) {
      const allCollapsed = g.instances.every((_, idx) =>
        foldState.value.get(`m:${g.id}:${idx}`) ?? false,
      )
      if (allCollapsed) hidden.add(g.id)
    }
    return hidden
  })

  const renderItems = computed(() =>
    buildRenderItems(pipelineResult.value, foldState.value, pipelineResult.value.drain),
  )

  function toggleFold(foldKey: string) {
    const current = foldState.value.get(foldKey) ?? false
    foldOverrides.value.set(foldKey, !current)
  }

  /** Batch-toggle all instances of a group (from side panel) */
  function toggleGroup(groupId: number) {
    const g = pipelineResult.value.motifGroups.find(g => g.id === groupId)
    if (!g) return
    const allCollapsed = g.instances.every((_, idx) =>
      foldState.value.get(`m:${g.id}:${idx}`) ?? false,
    )
    const newVal = !allCollapsed
    for (let idx = 0; idx < g.instances.length; idx++) {
      foldOverrides.value.set(`m:${g.id}:${idx}`, newVal)
    }
  }

  function scrollToGroup(groupId: number) {
    const container = logContainer.value
    if (!container) return
    // Motif group foldKeys are `m:${groupId}:${instIdx}`; scroll to the first instance.
    const prefix = `m:${groupId}:`
    for (const el of container.querySelectorAll('[data-fold-key]')) {
      const key = el.getAttribute('data-fold-key')
      if (key && key.startsWith(prefix)) {
        el.scrollIntoView({ behavior: 'smooth', block: 'center' })
        return
      }
    }
  }

  function lineClasses(line: DisplayLine): Record<string, boolean> {
    if (!logStore.processors.levelColoring) return {}
    return {
      'log-error': line.tags.has('error'),
      'log-warning': line.tags.has('warning'),
      'log-info': line.tags.has('info'),
      'log-debug': line.tags.has('debug'),
      'log-user-code': line.tags.has('user-code'),
    }
  }

  function getSegments(line: DisplayLine) {
    return segmentLine(line, logStore.processors)
  }

  /** Reset all user fold/expand overrides (e.g. when loading a new log). */
  function resetPipeline() {
    foldOverrides.value = new Map()
    expandedLines.value = new Set()
  }

  // ── Search ──
  const searchOpen = ref(false)
  const searchQuery = ref('')
  const searchInput = ref<{ focus: () => void } | null>(null)
  const searchIdx = ref(0)

  const searchMatches = computed(() => {
    if (!searchQuery.value) return [] as number[]
    const q = searchQuery.value.toLowerCase()
    const matches: number[] = []
    for (let i = 0; i < renderItems.value.length; i++) {
      const item = renderItems.value[i]
      if (item.type === 'line') {
        if ((item as any).line.text.toLowerCase().includes(q)) matches.push(i)
      } else if (item.type === 'drain-block') {
        if ((item as DrainBlockItem).lines.some(l => l.text.toLowerCase().includes(q))) matches.push(i)
      } else if (item.type === 'group-block') {
        if ((item as GroupBlockItem).lines.some(l => l.text.toLowerCase().includes(q))) matches.push(i)
      }
    }
    return matches
  })

  watch(searchQuery, () => { searchIdx.value = 0 })

  function toggleSearch() {
    searchOpen.value = !searchOpen.value
    if (searchOpen.value) {
      nextTick(() => searchInput.value?.focus())
    } else {
      searchQuery.value = ''
    }
  }

  function closeSearch() {
    searchOpen.value = false
    searchQuery.value = ''
  }

  function scrollToRenderItem(renderIdx: number) {
    const container = logContainer.value
    if (!container) return
    const children = container.children
    // skip the "no log output" placeholder if present
    const offset = logLines.value.length === 0 ? 1 : 0
    const idx = renderIdx + offset
    if (idx < children.length) {
      children[idx].scrollIntoView({ behavior: 'smooth', block: 'center' })
    }
  }

  function searchNext() {
    if (searchMatches.value.length === 0) return
    searchIdx.value = (searchIdx.value + 1) % searchMatches.value.length
    scrollToRenderItem(searchMatches.value[searchIdx.value])
  }

  function searchPrev() {
    if (searchMatches.value.length === 0) return
    searchIdx.value = (searchIdx.value - 1 + searchMatches.value.length) % searchMatches.value.length
    scrollToRenderItem(searchMatches.value[searchIdx.value])
  }

  function isSearchHit(renderIdx: number): boolean {
    if (!searchQuery.value || searchMatches.value.length === 0) return false
    return searchMatches.value[searchIdx.value] === renderIdx
  }

  return {
    // fold / expand
    foldOverrides, expandedLines,
    isLong, isExpanded, toggleExpand, truncateText,
    // pipeline
    pipelineResult, foldState, effectiveHidden, renderItems,
    toggleFold, toggleGroup, scrollToGroup, lineClasses, getSegments, resetPipeline,
    // search
    searchOpen, searchQuery, searchInput, searchIdx, searchMatches,
    toggleSearch, closeSearch, scrollToRenderItem, searchNext, searchPrev, isSearchHit,
  }
}
