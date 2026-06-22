<template>
  <div>
    <div class="d-flex align-center justify-space-between mb-4">
      <h2 class="text-h6">Log Viewer</h2>
      <div class="d-flex ga-2">
        <v-btn size="small" variant="tonal" prepend-icon="mdi-content-paste" @click="pasteFromClipboard">
          Paste
        </v-btn>
        <v-btn size="small" variant="tonal" prepend-icon="mdi-file-upload-outline" @click="fileInput?.click()">
          Open file
        </v-btn>
        <input ref="fileInput" type="file" accept=".log,.txt,.out,*" style="display:none" @change="onFileSelect" />
        <v-btn v-if="sessionId" size="small" variant="text" color="error" prepend-icon="mdi-close" @click="clear">
          Clear
        </v-btn>
      </div>
    </div>

    <!-- Input area (when no content loaded) -->
    <v-card v-if="!sessionId" class="pa-0" variant="outlined">
      <v-textarea
        v-model="inputText"
        placeholder="Paste log content here, or use the buttons above to paste / open a file..."
        variant="plain"
        rows="12"
        auto-grow
        hide-details
        class="log-input"
      />
      <v-card-actions class="pa-3 pt-0">
        <v-spacer />
        <v-btn :disabled="!inputText.trim()" :loading="uploading" color="primary" variant="tonal" @click="loadInput">
          Process
        </v-btn>
      </v-card-actions>
    </v-card>

    <!-- Processed log view -->
    <v-card v-else variant="outlined" class="log-viewer-card">
      <div class="d-flex align-center pa-3 pb-0 ga-2">
        <v-chip size="x-small" variant="tonal">{{ logLines.length }} lines</v-chip>
        <v-chip size="x-small" variant="tonal">{{ formatBytes(totalBytes) }}</v-chip>
        <v-btn
          v-if="endOffset < totalBytes"
          size="x-small" variant="text" :loading="loadingMore" @click="loadMore"
        >
          Load more
        </v-btn>
        <v-spacer />
        <!-- Search -->
        <div class="log-search-bar d-flex align-center" :class="{ 'log-search-bar--active': searchOpen }">
          <v-text-field
            v-if="searchOpen"
            ref="searchInput"
            v-model="searchQuery"
            density="compact"
            variant="plain"
            hide-details
            placeholder="Search..."
            class="log-search-input"
            @keydown.enter.exact="searchNext"
            @keydown.enter.shift="searchPrev"
            @keydown.escape="closeSearch"
          />
          <span v-if="searchOpen && searchQuery" class="text-caption text-no-wrap mx-1">
            {{ searchMatches.length > 0 ? `${searchIdx + 1}/${searchMatches.length}` : '0' }}
          </span>
          <v-btn v-if="searchOpen && searchMatches.length > 0" size="x-small" variant="text" icon="mdi-chevron-up" density="compact" @click="searchPrev" />
          <v-btn v-if="searchOpen && searchMatches.length > 0" size="x-small" variant="text" icon="mdi-chevron-down" density="compact" @click="searchNext" />
          <v-btn size="x-small" variant="text" :icon="searchOpen ? 'mdi-close' : 'mdi-magnify'" density="compact" @click="toggleSearch" />
        </div>
      </div>
      <div class="d-flex">
        <!-- Log content -->
        <div ref="logContainer" class="terminal-block" style="max-height: 70vh; overflow-y: auto; flex: 1; min-width: 0">
          <template v-for="(item, i) in renderItems" :key="i">
            <!-- Table block → v-table -->
            <div v-if="item.type === 'table-block'" class="log-table-wrap" :class="{ 'search-hit': isSearchHit(i) }">
              <v-table density="compact" class="log-table">
                <thead>
                  <tr>
                    <th v-for="(h, hi) in (item as TableBlockItem).headers" :key="hi">{{ h }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(row, ri) in (item as TableBlockItem).rows" :key="ri">
                    <td v-for="(cell, ci) in row" :key="ci">{{ cell }}</td>
                  </tr>
                </tbody>
              </v-table>
            </div>
            <!-- Unified fold summary (collapsed drain / motif / traceback) -->
            <div v-else-if="item.type === 'fold-summary'"
                 class="drain-fold-header"
                 :class="{ 'fold-traceback': (item as FoldSummaryItem).variant === 'traceback', 'search-hit': isSearchHit(i) }"
                 :data-fold-key="(item as FoldSummaryItem).foldKey"
                 @click="toggleFold((item as FoldSummaryItem).foldKey)">
              <span class="drain-fold-chevron">&#9654;</span>
              <span class="drain-fold-summary text-truncate">{{ (item as FoldSummaryItem).label }}</span>
              <v-chip size="x-small" variant="tonal"
                :color="(item as FoldSummaryItem).variant === 'traceback' ? 'error' : undefined"
                class="ml-auto flex-shrink-0"
              >
                {{ (item as FoldSummaryItem).lineCount }} lines<template v-if="(item as FoldSummaryItem).repeats"> ×{{ (item as FoldSummaryItem).repeats }}</template>
              </v-chip>
            </div>
            <!-- Drain block (expanded, diff-view aligned) -->
            <div v-else-if="item.type === 'drain-block'" class="drain-panel" :class="{ 'search-hit': isSearchHit(i) }" :data-fold-key="(item as DrainBlockItem).foldKey">
              <div class="drain-fold-header drain-fold-header--open" @click="toggleFold((item as DrainBlockItem).foldKey)">
                <span class="drain-fold-chevron">&#9660;</span>
                <span class="drain-fold-summary text-truncate">{{ (item as DrainBlockItem).template }}</span>
                <v-chip size="x-small" variant="tonal" class="ml-auto flex-shrink-0">{{ (item as DrainBlockItem).lines.length }} lines</v-chip>
              </div>
              <div class="drain-block-body">
                <div
                  v-for="(line, li) in (item as DrainBlockItem).lines" :key="li"
                  class="log-line"
                  :class="lineClasses(line)"
                >
                  <span v-if="line.timestamp" class="log-timestamp">{{ formatTimestamp(line.timestamp) }}</span>
                  <span v-else class="log-timestamp"></span>
                  <span class="log-lineno">{{ line.lineIdx + 1 }}</span>
                  <span class="log-line-content">
                    <span
                      v-for="(tok, ci) in (item as DrainBlockItem).tokens[li]" :key="ci"
                      class="drain-col"
                      :class="{ 'drain-static': !(item as DrainBlockItem).varMask[ci], 'drain-var': (item as DrainBlockItem).varMask[ci] }"
                      :style="{ minWidth: (item as DrainBlockItem).colWidths[ci] + 'ch' }"
                    >{{ tok }}</span>
                  </span>
                </div>
              </div>
              <div class="drain-fold-footer" @click="toggleFold((item as DrainBlockItem).foldKey)">
                <span class="drain-fold-chevron">&#9650;</span>
                <span>Collapse</span>
              </div>
            </div>
            <!-- Group block (expanded interleaved motif) -->
            <div v-else-if="item.type === 'group-block'" class="drain-panel" :class="{ 'search-hit': isSearchHit(i) }" :data-fold-key="(item as GroupBlockItem).foldKey">
              <div class="drain-fold-header drain-fold-header--open" @click="toggleFold((item as GroupBlockItem).foldKey)">
                <span class="drain-fold-chevron">&#9660;</span>
                <span class="drain-fold-summary text-truncate">{{ (item as GroupBlockItem).label }}</span>
                <v-chip size="x-small" variant="tonal" class="ml-auto flex-shrink-0">{{ (item as GroupBlockItem).lineCount }} lines ×{{ (item as GroupBlockItem).repeats }}</v-chip>
              </div>
              <div class="drain-block-body">
                <div
                  v-for="line in (item as GroupBlockItem).lines" :key="line.lineIdx"
                  class="log-line"
                  :class="lineClasses(line)"
                >
                  <span v-if="line.timestamp" class="log-timestamp">{{ formatTimestamp(line.timestamp) }}</span>
                  <span v-else class="log-timestamp"></span>
                  <span class="log-lineno">{{ line.lineIdx + 1 }}</span>
                  <span class="log-line-content">
                    <template v-for="(seg, si) in getSegments(line)" :key="si">
                      <span v-if="seg.cls" :class="seg.cls" :style="seg.style">{{ seg.text }}</span>
                      <template v-else>{{ seg.text }}</template>
                    </template>
                  </span>
                </div>
              </div>
              <div class="drain-fold-footer" @click="toggleFold((item as GroupBlockItem).foldKey)">
                <span class="drain-fold-chevron">&#9650;</span>
                <span>Collapse</span>
              </div>
            </div>
            <!-- Normal line -->
            <div
              v-else
              class="log-line"
              :class="{ ...lineClasses((item as any).line), 'search-hit': isSearchHit(i) }"
            >
              <span v-if="(item as any).line.timestamp" class="log-timestamp">{{ formatTimestamp((item as any).line.timestamp) }}</span>
              <span v-else class="log-timestamp"></span>
              <span class="log-lineno">{{ (item as any).line.lineIdx + 1 }}</span>
              <span class="log-line-content">
                <v-chip
                  v-if="(item as any).line.tqdmFolded > 0"
                  size="x-small" variant="tonal" color="secondary" class="mr-1 log-tqdm-badge"
                >
                  {{ (item as any).line.tqdmFolded }} folded
                </v-chip>
                <!-- Long line: truncated by default -->
                <template v-if="isLong((item as any).line) && !isExpanded((item as any).line)">
                  {{ truncateText((item as any).line.text) }}<span
                    class="log-expand-btn"
                    @click="toggleExpand((item as any).line)"
                  >… +{{ ((item as any).line.text.length - LONG_LINE_THRESHOLD).toLocaleString() }} chars</span>
                </template>
                <template v-else>
                  <template v-for="(seg, si) in getSegments((item as any).line)" :key="si">
                    <span v-if="seg.cls" :class="seg.cls" :style="seg.style">{{ seg.text }}</span>
                    <template v-else>{{ seg.text }}</template>
                  </template>
                  <span
                    v-if="isLong((item as any).line)"
                    class="log-collapse-btn"
                    @click="toggleExpand((item as any).line)"
                  >collapse</span>
                </template>
              </span>
            </div>
          </template>
        </div>
        <!-- Side panel -->
        <LogSidePanel
          :toggles="logStore.processors"
          :motif-groups="pipelineResult.motifGroups"
          :hidden-group-ids="effectiveHidden"
          :rules="logStore.preDrainRules"
          @toggle-processor="logStore.toggleProcessor"
          @toggle-cluster="toggleGroup"
          @scroll-to-group="scrollToGroup"
          @toggle-rule="logStore.toggleRule"
          @add-rule="logStore.addRule"
          @update-rule="logStore.updateRule"
          @remove-rule="logStore.removeRule"
        />
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { useLogViewerStore } from '@/stores/logViewer'
import { utilsApi } from '@/apis/utils'
import LogSidePanel from '@/components/LogSidePanel.vue'
import type { DisplayLine, DrainBlockItem, GroupBlockItem, FoldSummaryItem, TableBlockItem } from '@/utils/logProcessors'
import { formatTimestamp, LONG_LINE_THRESHOLD } from '@/utils/logProcessors'
import { useLogSurface } from '@/composables/useLogSurface'

const logStore = useLogViewerStore()

const inputText = ref('')
const fileInput = ref<HTMLInputElement>()
const logContainer = ref<HTMLElement>()

// ── Backend session state ──
const sessionId = ref('')
const logLines = ref<string[]>([])
const endOffset = ref(0)
const totalBytes = ref(0)
const uploading = ref(false)
const loadingMore = ref(false)

// ── Shared log surface (pipeline, fold state, search) — see useLogSurface ──
const {
  isLong, isExpanded, toggleExpand, truncateText,
  pipelineResult, foldState, effectiveHidden, renderItems,
  toggleFold, toggleGroup, scrollToGroup, lineClasses, getSegments, resetPipeline,
  searchOpen, searchQuery, searchInput, searchIdx, searchMatches,
  toggleSearch, closeSearch, searchNext, searchPrev, isSearchHit,
} = useLogSurface(logLines, logContainer)

// ── Upload + read via backend ──
async function upload(content: string | Blob) {
  uploading.value = true
  try {
    const sess = await utilsApi.uploadLog(content)
    sessionId.value = sess.id
    totalBytes.value = sess.total_bytes
    resetPipeline()
    // Load all lines (up to 5000)
    await fetchPage(0, 5000, true)
  } catch { /* snackbar handled by api client */ }
  finally { uploading.value = false }
}

async function fetchPage(offset: number, lines: number, replace: boolean) {
  try {
    const page = await utilsApi.readLog(sessionId.value, { offset, lines })
    if (replace) {
      logLines.value = page.lines
    } else {
      logLines.value.push(...page.lines)
    }
    endOffset.value = page.end_offset
    totalBytes.value = page.total_bytes
  } catch { /* ignore */ }
}

async function loadMore() {
  if (endOffset.value >= totalBytes.value) return
  loadingMore.value = true
  try { await fetchPage(endOffset.value, 2000, false) }
  finally { loadingMore.value = false }
}

// ── Input handling ──
function loadInput() {
  if (inputText.value.trim()) upload(inputText.value)
}

async function pasteFromClipboard() {
  try {
    const text = await navigator.clipboard.readText()
    if (text.trim()) {
      inputText.value = ''
      upload(text)
    }
  } catch { /* clipboard access denied */ }
}

function onFileSelect(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  inputText.value = ''
  upload(file)
  if (fileInput.value) fileInput.value.value = ''
}

function clear() {
  if (sessionId.value) {
    utilsApi.deleteLog(sessionId.value).catch(() => {})
  }
  sessionId.value = ''
  logLines.value = []
  endOffset.value = 0
  totalBytes.value = 0
  inputText.value = ''
  resetPipeline()
}

function onKeydown(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
    if (sessionId.value) {
      e.preventDefault()
      searchOpen.value = true
      nextTick(() => searchInput.value?.focus())
    }
  }
}

onMounted(() => document.addEventListener('keydown', onKeydown))
onUnmounted(() => document.removeEventListener('keydown', onKeydown))

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`
  return `${(b / (1024 * 1024 * 1024)).toFixed(1)} GB`
}

// Cleanup session on unmount
onUnmounted(() => {
  if (sessionId.value) utilsApi.deleteLog(sessionId.value).catch(() => {})
})
</script>

<style scoped>
.log-input :deep(textarea) {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 0.8rem;
  line-height: 1.5;
  padding: 16px;
}
.log-viewer-card {
  overflow: hidden;
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
  color: #64748B;
  user-select: none;
  flex-shrink: 0;
}
.log-timestamp {
  width: 8ch;
  min-width: 8ch;
  padding-right: 8px;
  color: #64748B;
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

/* ── Drain / Motif fold ── */
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
.drain-panel {
  margin: 2px 0;
}
.drain-block-body {
  border-left: 1px solid rgb(var(--v-theme-outline-variant));
  border-right: 1px solid rgb(var(--v-theme-outline-variant));
  max-height: 50vh;
  overflow-y: auto;
}
.drain-fold-footer {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 16px;
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
.drain-static { color: #475569; }
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
