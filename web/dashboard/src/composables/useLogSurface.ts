import { ref, computed, watch, nextTick, onScopeDispose, type Ref } from 'vue'
import { useLogViewerStore } from '@/stores/logViewer'
import type { DisplayLine } from '@/utils/logProcessors'
import {
  buildRenderItems,
  computeDefaultFoldState,
  segmentLine,
  LONG_LINE_THRESHOLD,
} from '@/utils/logProcessors'
import { IncrementalLogPipeline, MOTIF_THROTTLE_MS } from '@/utils/log/incremental'

/** Client-side search caps: a 1-char query on a 20k buffer must not build
 *  an unbounded match array. */
const SEARCH_MATCH_CAP = 5000
const SEARCH_DEBOUNCE_MS = 250

function isLong(line: DisplayLine): boolean {
  return line.text.length > LONG_LINE_THRESHOLD
}

function truncateText(text: string): string {
  return text.slice(0, LONG_LINE_THRESHOLD)
}

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

  function isExpanded(line: DisplayLine): boolean {
    return expandedLines.value.has(line.lineIdx)
  }

  function toggleExpand(line: DisplayLine) {
    if (expandedLines.value.has(line.lineIdx)) expandedLines.value.delete(line.lineIdx)
    else expandedLines.value.add(line.lineIdx)
  }

  // ── Incremental pipeline ──
  // Appends feed the incremental engine (O(batch), not O(total)); a new
  // array reference (reload / trim / paging replace) or a toggle/rule
  // change rebuilds. pipelineVersion is the reactivity bridge: the engine
  // itself is deliberately non-reactive (deep-reactive DisplayLines would
  // cost more than the pipeline).
  const engine = new IncrementalLogPipeline(logStore.processors, logStore.preDrainRules)
  const pipelineVersion = ref(0)
  let fedRef: string[] | null = null
  let fedCount = 0

  watch([logLines, () => logLines.value.length], () => {
    const arr = logLines.value
    if (arr !== fedRef || arr.length < fedCount) {
      engine.reset(arr.slice())
    } else if (arr.length > fedCount) {
      engine.push(arr.slice(fedCount))
    } else {
      return
    }
    fedRef = arr
    fedCount = arr.length
    pipelineVersion.value++
    scheduleMotifRefresh()
  }, { immediate: true })

  watch(
    [() => ({ ...logStore.processors }), () => logStore.preDrainRules.map(r => ({ ...r }))],
    () => {
      engine.reset(logLines.value.slice(), logStore.processors, logStore.preDrainRules)
      fedRef = logLines.value
      fedCount = logLines.value.length
      pipelineVersion.value++
    },
    { deep: true },
  )

  /** Notify the engine that the LAST line's text changed IN PLACE
   *  (continues-fragment merge, log contract v2). The [ref, length] watch
   *  above cannot detect a same-length content change, so the caller
   *  mutates logLines[last] AND calls this; fedRef/fedCount stay valid
   *  because the array reference and length are unchanged. */
  function replaceTailLine(text: string) {
    engine.replaceTailLine(text)
    pipelineVersion.value++
    scheduleMotifRefresh()
  }

  // Motifs are throttled inside the engine; make sure the LAST batch's
  // groups still surface once the quiet period ends.
  let motifTimer: ReturnType<typeof setTimeout> | null = null
  function scheduleMotifRefresh() {
    if (!engine.motifsStale || motifTimer) return
    motifTimer = setTimeout(() => {
      motifTimer = null
      if (engine.motifsStale) {
        engine.recomputeMotifs()
        pipelineVersion.value++
      }
    }, MOTIF_THROTTLE_MS)
  }
  onScopeDispose(() => { if (motifTimer) clearTimeout(motifTimer) })

  const pipelineResult = computed(() => {
    void pipelineVersion.value
    return engine.result()
  })

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
    const group = pipelineResult.value.motifGroups.find(candidate => candidate.id === groupId)
    if (!group) return
    const allCollapsed = group.instances.every((_, idx) =>
      foldState.value.get(`m:${group.id}:${idx}`) ?? false,
    )
    const newVal = !allCollapsed
    for (let idx = 0; idx < group.instances.length; idx++) {
      foldOverrides.value.set(`m:${group.id}:${idx}`, newVal)
    }
  }

  // ── Virtualized scrolling ──
  // The render surface is virtualized (LogSurfaceView), so DOM queries no
  // longer see off-screen items. LogSurfaceView injects its virtualizer's
  // scrollToIndex here on mount; before injection scrolling is a no-op
  // (scrollToBottom falls back to raw scrollTop on logContainer).
  type ScrollAlign = 'start' | 'center' | 'end'
  const scrollToIndexRef = ref<((idx: number, align?: ScrollAlign) => void) | null>(null)

  function scrollToRenderItem(renderIdx: number) {
    scrollToIndexRef.value?.(renderIdx, 'center')
  }

  function scrollToGroup(groupId: number) {
    // Motif group foldKeys are `m:${groupId}:${instIdx}`; scroll to the
    // first render item of the first instance.
    const prefix = `m:${groupId}:`
    const items = renderItems.value
    for (let i = 0; i < items.length; i++) {
      const item = items[i]
      if ('foldKey' in item && item.foldKey.startsWith(prefix)) {
        scrollToRenderItem(i)
        return
      }
    }
  }

  /** Pin the view to the last render item (follow mode). */
  function scrollToBottom() {
    const count = renderItems.value.length
    if (scrollToIndexRef.value && count > 0) {
      scrollToIndexRef.value(count - 1, 'end')
    } else {
      const el = logContainer.value
      if (el) el.scrollTop = el.scrollHeight
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
  /** Interpret the query as a regular expression (case-insensitive). */
  const searchRegex = ref(false)
  /** Set when regex mode is on and the pattern doesn't compile. */
  const searchError = ref('')
  /** True when the match list hit SEARCH_MATCH_CAP (display as "5000+"). */
  const searchTruncated = ref(false)

  // Debounced: a short query over a 20k buffer re-scans on every keystroke
  // otherwise. The scan itself is capped so pathological queries stay flat.
  const debouncedQuery = ref('')
  let searchTimer: ReturnType<typeof setTimeout> | null = null
  watch([searchQuery, searchRegex], () => {
    searchIdx.value = 0
    if (searchTimer) clearTimeout(searchTimer)
    searchTimer = setTimeout(() => {
      searchTimer = null
      debouncedQuery.value = searchQuery.value
    }, SEARCH_DEBOUNCE_MS)
  })
  onScopeDispose(() => { if (searchTimer) clearTimeout(searchTimer) })

  const searchMatches = computed(() => {
    searchTruncated.value = false
    searchError.value = ''
    const q = debouncedQuery.value
    if (!q) return [] as number[]

    let test: (text: string) => boolean
    if (searchRegex.value) {
      try {
        const re = new RegExp(q, 'i')
        test = (text) => re.test(text)
      } catch (e: any) {
        searchError.value = e?.message || 'invalid regex'
        return []
      }
    } else {
      const lq = q.toLowerCase()
      test = (text) => text.toLowerCase().includes(lq)
    }

    const matches: number[] = []
    for (let i = 0; i < renderItems.value.length && matches.length < SEARCH_MATCH_CAP; i++) {
      const item = renderItems.value[i]
      // Expanded blocks are flattened, so block lines match individually.
      if (item.type === 'line' || item.type === 'block-line') {
        if (test(item.line.text)) matches.push(i)
      }
    }
    if (matches.length >= SEARCH_MATCH_CAP) searchTruncated.value = true
    return matches
  })

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
    replaceTailLine,
    // scrolling (LogSurfaceView injects scrollToIndexRef and binds logContainer)
    logContainer, scrollToIndexRef, scrollToBottom,
    // search
    searchOpen, searchQuery, searchInput, searchIdx, searchMatches,
    searchRegex, searchError, searchTruncated,
    toggleSearch, closeSearch, scrollToRenderItem, searchNext, searchPrev, isSearchHit,
  }
}

/** The full surface object, passed as a single prop to LogSurfaceView. */
export type LogSurface = ReturnType<typeof useLogSurface>
