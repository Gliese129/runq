<template>
  <div class="log-surface">
    <!-- Search bar -->
    <div v-if="items.length > 0 || searchOpen" class="d-flex align-center mb-1 ga-2">
      <v-spacer />
      <div class="log-search-bar d-flex align-center" :class="{ 'log-search-bar--active': searchOpen }">
        <v-text-field
          v-if="searchOpen"
          ref="searchInput"
          v-model="searchQuery"
          density="compact"
          variant="plain"
          hide-details
          :placeholder="t('log.search')"
          class="log-search-input"
          @keydown.enter.exact="searchNext"
          @keydown.enter.shift="searchPrev"
          @keydown.escape="closeSearch"
        />
        <span v-if="searchOpen && searchError" class="text-caption text-error text-no-wrap mx-1">
          {{ searchError }}
        </span>
        <span v-else-if="searchOpen && searchQuery" class="text-caption text-no-wrap mx-1">
          {{ searchMatches.length > 0 ? `${searchIdx + 1}/${searchTruncated ? '5000+' : searchMatches.length}` : '0' }}
        </span>
        <v-btn
          v-if="searchOpen"
          size="x-small" variant="text" icon="mdi-regex" density="compact"
          :color="searchRegex ? 'primary' : undefined"
          :aria-label="t('log.regex_mode')" :title="t('log.regex_mode')"
          @click="searchRegex = !searchRegex"
        />
        <v-btn v-if="searchOpen && searchMatches.length > 0" size="x-small" variant="text" icon="mdi-chevron-up" density="compact" :aria-label="t('log.search_prev')" @click="searchPrev" />
        <v-btn v-if="searchOpen && searchMatches.length > 0" size="x-small" variant="text" icon="mdi-chevron-down" density="compact" :aria-label="t('log.search_next')" @click="searchNext" />
        <v-btn size="x-small" variant="text" :icon="searchOpen ? 'mdi-close' : 'mdi-magnify'" density="compact" :aria-label="searchOpen ? t('log.search_close') : t('log.search_open')" @click="toggleSearch" />
      </div>
    </div>

    <!-- Virtualized log content -->
    <div ref="containerEl" class="terminal-block log-scroll" :style="{ maxHeight }">
      <div v-if="items.length === 0 && !logLoading" class="log-empty text-center pa-4">
        {{ emptyText }}
      </div>
      <div v-else class="log-virtual-space" :style="{ height: totalSize + 'px' }">
        <div
          v-for="{ vr, item } in virtualRows"
          :key="vr.key as string | number"
          :ref="measureRow"
          :data-index="vr.index"
          class="log-virtual-row"
          :style="{ transform: `translateY(${vr.start}px)` }"
        >
          <!-- Table block → v-table -->
          <div v-if="item.type === 'table-block'" class="log-table-wrap" :class="{ 'search-hit': isSearchHit(vr.index) }">
            <v-table density="compact" class="log-table">
              <thead>
                <tr>
                  <th v-for="(h, hi) in item.headers" :key="hi">{{ h }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(row, ri) in item.rows" :key="ri">
                  <td v-for="(cell, ci) in row" :key="ci">{{ cell }}</td>
                </tr>
              </tbody>
            </v-table>
          </div>
          <!-- Unified fold summary (collapsed drain / motif / traceback) -->
          <div
            v-else-if="item.type === 'fold-summary'"
            class="drain-fold-header"
            :class="{ 'fold-traceback': item.variant === 'traceback', 'search-hit': isSearchHit(vr.index) }"
            :data-fold-key="item.foldKey"
            @click="toggleFold(item.foldKey)"
          >
            <span class="drain-fold-chevron">&#9654;</span>
            <span class="drain-fold-summary text-truncate">{{ item.label }}</span>
            <v-chip
              size="x-small" variant="tonal"
              :color="item.variant === 'traceback' ? 'error' : undefined"
              class="ml-auto flex-shrink-0"
            >
              {{ t('log.n_lines', { n: item.lineCount }) }}<template v-if="item.repeats"> ×{{ item.repeats }}</template>
            </v-chip>
          </div>
          <!-- Expanded block header (drain diff view / interleaved motif) -->
          <div
            v-else-if="item.type === 'block-head'"
            class="drain-fold-header drain-fold-header--open"
            :class="{ 'search-hit': isSearchHit(vr.index) }"
            :data-fold-key="item.foldKey"
            @click="toggleFold(item.foldKey)"
          >
            <span class="drain-fold-chevron">&#9660;</span>
            <span class="drain-fold-summary text-truncate">{{ item.label }}</span>
            <v-chip size="x-small" variant="tonal" class="ml-auto flex-shrink-0">
              {{ t('log.n_lines', { n: item.lineCount }) }}<template v-if="item.repeats"> ×{{ item.repeats }}</template>
            </v-chip>
          </div>
          <!-- Expanded block line (left/right borders continue the panel) -->
          <div
            v-else-if="item.type === 'block-line'"
            class="log-line block-line"
            :class="{ ...lineClasses(item.line), 'search-hit': isSearchHit(vr.index) }"
          >
            <span v-if="item.line.timestamp" class="log-timestamp">{{ formatTimestamp(item.line.timestamp) }}</span>
            <span v-else class="log-timestamp"></span>
            <span v-if="lineNumberBase >= 0" class="log-lineno">{{ lineNumberBase + item.line.lineIdx + 1 }}</span>
            <span class="log-line-content">
              <!-- Drain diff view: dim static tokens, highlight variables -->
              <template v-if="item.tokens && item.varMask && item.colWidths">
                <span
                  v-for="(tok, ci) in item.tokens" :key="ci"
                  class="drain-col"
                  :class="{ 'drain-static': !item.varMask[ci], 'drain-var': item.varMask[ci] }"
                  :style="{ minWidth: item.colWidths[ci] + 'ch' }"
                >{{ tok }}</span>
              </template>
              <template v-else>
                <template v-for="(seg, si) in getSegments(item.line)" :key="si">
                  <span v-if="seg.cls" :class="seg.cls" :style="seg.style">{{ seg.text }}</span>
                  <template v-else>{{ seg.text }}</template>
                </template>
              </template>
            </span>
          </div>
          <!-- Expanded block footer -->
          <div
            v-else-if="item.type === 'block-tail'"
            class="drain-fold-footer"
            :data-fold-key="item.foldKey"
            @click="toggleFold(item.foldKey)"
          >
            <span class="drain-fold-chevron">&#9650;</span>
            <span>{{ t('common.collapse') }}</span>
          </div>
          <!-- Normal line -->
          <div
            v-else
            class="log-line"
            :class="{ ...lineClasses(item.line), 'search-hit': isSearchHit(vr.index) }"
          >
            <span v-if="item.line.timestamp" class="log-timestamp">{{ formatTimestamp(item.line.timestamp) }}</span>
            <span v-else class="log-timestamp"></span>
            <span v-if="lineNumberBase >= 0" class="log-lineno">{{ lineNumberBase + item.line.lineIdx + 1 }}</span>
            <span class="log-line-content">
              <!-- tqdm fold badge -->
              <v-chip
                v-if="item.line.tqdmFolded > 0"
                size="x-small" variant="tonal" color="secondary" class="mr-1 log-tqdm-badge"
              >
                {{ t('log.n_folded', { n: item.line.tqdmFolded }) }}
              </v-chip>
              <!-- Long line: truncated by default -->
              <template v-if="isLong(item.line) && !isExpanded(item.line)">
                {{ truncateText(item.line.text) }}<span
                  class="log-expand-btn"
                  @click="toggleExpand(item.line)"
                >… {{ t('log.expand_chars', { n: (item.line.text.length - LONG_LINE_THRESHOLD).toLocaleString() }) }}</span>
              </template>
              <template v-else>
                <template v-for="(seg, si) in getSegments(item.line)" :key="si">
                  <span v-if="seg.cls" :class="seg.cls" :style="seg.style">{{ seg.text }}</span>
                  <template v-else>{{ seg.text }}</template>
                </template>
                <span
                  v-if="isLong(item.line)"
                  class="log-collapse-btn"
                  @click="toggleExpand(item.line)"
                >{{ t('common.collapse').toLocaleLowerCase() }}</span>
              </template>
            </span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
// Shared virtualized log surface (RQ-54 layer E). Renders the FLATTENED
// RenderItem list — expanded blocks arrive as block-head / block-line /
// block-tail top-level items — through @tanstack/vue-virtual so a 22k-line
// expanded block costs only the visible rows. The in-pane search bar lives
// here too; surrounding chrome (trimmed banner, panel titles, Load more,
// Follow switch, side panel) stays in the parent views.
//
// The parent owns useLogSurface and passes the whole returned object as
// `surface` — this component must NOT invoke useLogSurface itself.
import { ref, computed, watch, onMounted, onBeforeUnmount, type ComponentPublicInstance } from 'vue'
import { useI18n } from 'vue-i18n'
import { useVirtualizer } from '@tanstack/vue-virtual'
import type { RenderItem } from '@/utils/logProcessors'
import { formatTimestamp, LONG_LINE_THRESHOLD } from '@/utils/logProcessors'
import type { LogSurface } from '@/composables/useLogSurface'

const props = withDefaults(defineProps<{
  surface: LogSurface
  items: RenderItem[]
  logLoading?: boolean
  emptyText?: string
  maxHeight?: string
  /** Absolute line number of the buffer's first line (0-based). Rendered
   *  numbers are lineNumberBase + lineIdx + 1; -1 hides the column
   *  entirely (live view / unknown base — log contract v2). */
  lineNumberBase?: number
}>(), {
  logLoading: false,
  emptyText: '',
  maxHeight: '600px',
  lineNumberBase: -1,
})

const { t } = useI18n()

// The surface object is created once by the parent; destructuring keeps
// the refs intact (auto-unwrapped in the template) without invoking the
// composable a second time here.
const {
  isLong, isExpanded, toggleExpand, truncateText,
  toggleFold, lineClasses, getSegments,
  logContainer, scrollToIndexRef,
  searchOpen, searchQuery, searchInput, searchIdx, searchMatches,
  searchRegex, searchError, searchTruncated,
  toggleSearch, closeSearch, searchNext, searchPrev, isSearchHit,
} = props.surface

// ── Virtualization: tanstack standard mode — total-height spacer +
//    absolutely positioned translateY rows; measureElement handles the
//    dynamic heights (wrapped lines, tables, fold headers). ──
const containerEl = ref<HTMLElement | null>(null)

const virtualizer = useVirtualizer(computed(() => ({
  count: props.items.length,
  getScrollElement: () => containerEl.value,
  estimateSize: () => 21,
  overscan: 20,
})))

const totalSize = computed(() => virtualizer.value.getTotalSize())
// Pair each virtual row with its item; skip rows whose index outruns a
// just-shrunk items array (the virtualizer catches up on the next tick).
const virtualRows = computed(() =>
  virtualizer.value.getVirtualItems().flatMap((vr) => {
    const item = props.items[vr.index]
    return item ? [{ vr, item }] : []
  }),
)

/** Row ref callback: feeds measured heights back to the virtualizer. */
function measureRow(el: Element | ComponentPublicInstance | null) {
  if (el) virtualizer.value.measureElement(el as Element)
}

// Expose the scroll element to the surface (scrollToBottom fallback) and
// inject virtualized scrolling (scrollToRenderItem / scrollToGroup /
// scrollToBottom all route through scrollToIndexRef).
watch(containerEl, (el) => { logContainer.value = el ?? undefined })
onMounted(() => {
  scrollToIndexRef.value = (idx, align = 'center') =>
    virtualizer.value.scrollToIndex(idx, { align })
})
onBeforeUnmount(() => {
  scrollToIndexRef.value = null
  logContainer.value = undefined
})
</script>

<style scoped>
.log-surface {
  flex: 1 1 auto;
  min-width: 0;
}
.log-scroll {
  overflow-y: auto;
}
/* Muted-but-readable: slate-400 keeps ≥4.5:1 on the #0F172A terminal
   background (slate-500 was ~3.7:1 — below WCAG AA). */
.log-empty {
  color: #94A3B8;
}

/* ── Virtual rows (tanstack standard mode) ── */
.log-virtual-space {
  position: relative;
  width: 100%;
}
.log-virtual-row {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
}

/* ── Line layout ── */
.log-line {
  display: flex;
  white-space: pre-wrap;
  overflow-wrap: break-word;
  padding: 0 16px 0 0;
  line-height: 1.5;
}
.log-lineno {
  display: inline-block;
  min-width: 48px;
  width: 48px;
  text-align: right;
  padding-right: 12px;
  color: #94A3B8;
  user-select: none;
  flex-shrink: 0;
}
.log-timestamp {
  width: 8ch;
  min-width: 8ch;
  padding-right: 8px;
  color: #94A3B8;
  font-size: 0.9em;
  user-select: none;
  flex-shrink: 0;
  white-space: nowrap;
}
.log-line-content {
  flex: 1;
  min-width: 0;
}

/* ── Level coloring ── */
.log-error { color: rgb(var(--v-theme-error)); background: rgba(var(--v-theme-error), 0.06); }
.log-warning { color: rgb(var(--v-theme-warning)); }
.log-info { opacity: 0.75; }
.log-debug { opacity: 0.5; }

/* ── Traceback ── */
.log-user-code { font-weight: 600; background: rgba(var(--v-theme-error), 0.1); }
.fold-traceback {
  border-left: 3px solid rgb(var(--v-theme-error));
  color: rgb(var(--v-theme-error));
}

/* ── Drain / Motif fold — the expanded panel is rebuilt from flattened
      items: head carries top border + radius, each block-line carries the
      side borders, tail closes with the bottom border + radius. ── */
.drain-fold-header {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 16px;
  cursor: pointer;
  border: 1px solid rgb(var(--v-theme-outline-variant));
  border-radius: 4px;
  margin: 2px 0;
  font-size: 0.82em;
  color: rgb(var(--v-theme-on-surface-variant));
  user-select: none;
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
}
.drain-fold-header:hover {
  background: rgba(var(--v-theme-primary), 0.06);
}
.drain-fold-header--open {
  border-bottom-left-radius: 0;
  border-bottom-right-radius: 0;
  margin-bottom: 0;
  border-bottom-color: transparent;
}
.drain-fold-chevron {
  font-size: 0.65em;
  flex-shrink: 0;
  width: 1em;
  text-align: center;
}
.drain-fold-summary {
  flex: 1;
  min-width: 0;
}
.block-line {
  border-left: 1px solid rgb(var(--v-theme-outline-variant));
  border-right: 1px solid rgb(var(--v-theme-outline-variant));
}
.drain-fold-footer {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 16px;
  margin-bottom: 2px;
  cursor: pointer;
  font-size: 0.82em;
  color: rgb(var(--v-theme-on-surface-variant));
  user-select: none;
  border: 1px solid rgb(var(--v-theme-outline-variant));
  border-top-color: transparent;
  border-bottom-left-radius: 4px;
  border-bottom-right-radius: 4px;
}
.drain-fold-footer:hover {
  background: rgba(var(--v-theme-primary), 0.06);
}
.drain-col { display: inline-block; padding-right: 1ch; }
/* Static tokens stay visually secondary via weight, but the color itself
   clears 4.5:1 on the terminal background (was #475569 ≈ 2.4:1). */
.drain-static { color: #94A3B8; }
.drain-var { color: #e2e8f0; font-weight: 500; }

/* ── Inline highlights ── */
.log-metric { background: rgba(var(--v-theme-success), 0.15); border-radius: 2px; padding: 0 2px; }
.log-rank { font-weight: 600; }
.log-tqdm-badge { vertical-align: text-bottom; }

/* ── Table block ── */
.log-table-wrap {
  margin: 4px 0;
  padding-left: 48px;
  overflow-x: auto;
}
.log-table {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 0.8rem;
  background: transparent !important;
}
.log-table :deep(th) {
  font-weight: 600 !important;
  white-space: nowrap;
  padding: 2px 12px !important;
  height: auto !important;
  font-size: 0.8rem !important;
}
.log-table :deep(td) {
  white-space: nowrap;
  padding: 2px 12px !important;
  height: auto !important;
  font-size: 0.8rem !important;
}

/* ── Long-line truncation ── */
.log-expand-btn, .log-collapse-btn {
  cursor: pointer;
  color: rgb(var(--v-theme-primary));
  font-size: 0.85em;
  padding: 0 4px;
  opacity: 0.8;
}
.log-expand-btn:hover, .log-collapse-btn:hover { opacity: 1; text-decoration: underline; }

/* ── Search ── */
.log-search-bar {
  border-radius: 4px;
  transition: all 0.15s ease;
}
.log-search-bar--active {
  background: rgba(var(--v-theme-surface-variant), 0.5);
  padding: 0 4px;
}
.log-search-input {
  max-width: 180px;
  font-size: 0.8rem;
}
.log-search-input :deep(input) {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 0.8rem;
  padding: 2px 4px;
}
.search-hit {
  outline: 2px solid rgb(var(--v-theme-warning));
  outline-offset: -1px;
  border-radius: 2px;
}
</style>
