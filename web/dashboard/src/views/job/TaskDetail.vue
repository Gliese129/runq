<template>
  <div v-if="task">
    <!-- Header -->
    <v-card class="mb-4 pa-4">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="d-flex align-center ga-2">
          <code class="text-h6">{{ task.id.slice(0, 8) }}</code>
          <TaskStatusBadge :status="displayStatus" />
        </div>
        <div class="d-flex ga-1">
          <v-btn v-if="displayStatus === 'running'" size="x-small" variant="tonal" color="error"
            :loading="killing" :disabled="killing" @click="killTask">
            <v-icon start size="14">mdi-stop</v-icon> Kill
          </v-btn>
          <v-btn v-if="config.caps.retry && (task.status === 'failed' || task.status === 'killed')" size="x-small" variant="tonal" color="primary"
            :loading="retrying" :disabled="retrying" @click="retryTask">
            <v-icon start size="14">mdi-refresh</v-icon> Retry
          </v-btn>
        </div>
      </div>

      <!-- Stats -->
      <v-row dense>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">Elapsed</div>
          <div class="text-body-2 font-weight-medium">{{ task.elapsed_seconds ? formatDuration(task.elapsed_seconds) : '—' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">Step</div>
          <div class="text-body-2 font-weight-medium">{{ task.current_step ?? '—' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">Retries</div>
          <div class="text-body-2 font-weight-medium">{{ task.retry_count || 0 }} / {{ task.max_retry || '∞' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">GPUs</div>
          <div class="text-body-2 font-weight-medium">{{ task.gpus || '—' }}</div>
        </v-col>
      </v-row>
      <!-- HPC-specific stats (poll-model only) -->
      <v-row v-if="task.external_id" dense class="mt-1">
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">External ID</div>
          <div class="text-body-2 font-weight-medium"><code>{{ task.external_id }}</code></div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">Scheduler State</div>
          <div class="text-body-2 font-weight-medium">{{ task.native_state || '—' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">Queue</div>
          <div class="text-body-2 font-weight-medium">{{ task.queue || '—' }}</div>
        </v-col>
      </v-row>
    </v-card>

    <!-- Parameters / Metrics / Log -->
    <v-expansion-panels v-model="openPanels" multiple>
      <v-expansion-panel value="params">
        <v-expansion-panel-title>
          <span class="text-subtitle-2">Parameters</span>
          <v-chip size="x-small" variant="tonal" class="ml-2">{{ Object.keys(task.params || {}).length }}</v-chip>
        </v-expansion-panel-title>
        <v-expansion-panel-text>
          <div class="overflow-x-auto">
            <table class="data-mono" style="width: 100%">
              <thead><tr><th>Name</th><th>Value</th></tr></thead>
              <tbody>
                <tr v-for="(val, key) in task.params" :key="key">
                  <td class="font-weight-medium">{{ key }}</td>
                  <td>{{ val }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </v-expansion-panel-text>
      </v-expansion-panel>

      <v-expansion-panel v-if="task.status !== 'pending'" value="metrics">
        <v-expansion-panel-title>
          <span class="text-subtitle-2 d-flex align-center ga-1">
            <v-icon size="16">mdi-chart-line</v-icon> Metrics
          </span>
          <v-spacer />
          <v-btn
            v-if="task.wandb_run_id"
            size="x-small" variant="text" class="mr-2"
            :href="wandbRunURL(task.wandb_run_id)" target="_blank"
            @click.stop
          >
            <v-icon start size="14">mdi-open-in-new</v-icon> W&B
          </v-btn>
        </v-expansion-panel-title>
        <v-expansion-panel-text>
          <MetricsChart :points="metricPoints" />
        </v-expansion-panel-text>
      </v-expansion-panel>

      <v-expansion-panel value="log" class="log-panel">
        <v-expansion-panel-title>
          <span class="text-subtitle-2">Log</span>
          <v-chip v-if="totalBytes > 0" size="x-small" variant="tonal" class="ml-2">{{ formatBytes(totalBytes) }}</v-chip>
          <v-spacer />
          <div class="d-flex align-center ga-1 mr-2" @click.stop>
            <v-btn
              v-if="!following && endOffset < totalBytes"
              size="x-small" variant="text" :loading="loadingMore" @click="loadMore"
            >
              Load more
            </v-btn>
            <v-switch
              v-model="following"
              density="compact" hide-details color="primary" inline
              label="Follow"
            />
          </div>
        </v-expansion-panel-title>
        <v-expansion-panel-text>
          <!-- Search bar -->
          <div class="d-flex align-center mb-1 ga-2" v-if="logLines.length > 0">
            <v-spacer />
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
            <div ref="logContainer" class="terminal-block" style="max-height: 600px; overflow-y: auto; flex: 1; min-width: 0">
              <div v-if="logLines.length === 0 && !logLoading" class="text-center pa-4" style="color: #64748B">
                {{ task.status === 'pending' ? 'Waiting to start...' : 'No log output yet' }}
              </div>
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
                    <!-- tqdm fold badge -->
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
        </v-expansion-panel-text>
      </v-expansion-panel>
    </v-expansion-panels>
  </div>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { tasksApi } from '@/apis/tasks'
import { jobsApi } from '@/apis/jobs'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfirm } from '@/composables/useConfirm'
import { useConfigStore } from '@/stores/config'
import { useLogViewerStore } from '@/stores/logViewer'
import MetricsChart from '@/components/MetricsChart.vue'
import TaskStatusBadge from '@/components/TaskStatusBadge.vue'
import LogSidePanel from '@/components/LogSidePanel.vue'
import type { LogPage, TaskView } from '@/types/api'
import type { DisplayLine, RenderItem, DrainBlockItem, GroupBlockItem, FoldSummaryItem, TableBlockItem } from '@/utils/logProcessors'
import { formatTimestamp, LONG_LINE_THRESHOLD } from '@/utils/logProcessors'
import { useLogSurface } from '@/composables/useLogSurface'

const props = defineProps<{ project: string; jobId: string; taskId: string }>()
const snack = useSnackbar()
const config = useConfigStore()
const logStore = useLogViewerStore()
const { t } = useI18n()
const { confirm: confirmDialog } = useConfirm()

const task = ref<TaskView | null>(null)

// Kill in flight
const cancelPending = ref(false)
const displayStatus = computed(() =>
  cancelPending.value && task.value?.status === 'running' ? 'cancelling' : task.value?.status ?? '',
)
watch(() => task.value?.status, (s) => {
  if (s && s !== 'running') cancelPending.value = false
})

// ── Log state (byte-offset based) ──
const logLines = ref<string[]>([])
const endOffset = ref(0)
const totalBytes = ref(0)
const logLoading = ref(false)
const loadingMore = ref(false)
const following = ref(false)
const logContainer = ref<HTMLElement>()

const metricPoints = ref<any[]>([])
const openPanels = ref(['params', 'metrics', 'log'])

// W&B link: fetch base_url from job detail to avoid hardcoding wandb.ai.
const wandbBaseUrl = ref('https://wandb.ai')
async function fetchWandbInfo() {
  try {
    const detail = await jobsApi.get(props.jobId)
    if (detail.wandb?.base_url) {
      wandbBaseUrl.value = detail.wandb.base_url
    }
  } catch { /* best effort */ }
}
function wandbRunURL(runId: string): string {
  return `${wandbBaseUrl.value}/runs/${runId}`
}

// ── Shared log surface (pipeline, fold state, search) — see useLogSurface ──
const {
  isLong, isExpanded, toggleExpand, truncateText,
  pipelineResult, foldState, effectiveHidden, renderItems,
  toggleFold, toggleGroup, scrollToGroup, lineClasses, getSegments,
  searchOpen, searchQuery, searchInput, searchIdx, searchMatches,
  toggleSearch, closeSearch, searchNext, searchPrev, isSearchHit,
} = useLogSurface(logLines, logContainer)

// ── Fetch task info ──
async function fetchTask() {
  try {
    task.value = await tasksApi.get(props.taskId)
  } catch { /* ignore */ }
}

async function fetchMetrics() {
  try {
    metricPoints.value = await tasksApi.metrics(props.taskId)
  } catch { metricPoints.value = [] }
}

// ── Fetch log (GET — initial load + manual paging) ──
async function fetchLog(offset = 0, lines = 500) {
  logLoading.value = true
  try {
    const page = await tasksApi.log(props.taskId, { offset, lines })
    applyPage(page, offset === 0)
  } catch { /* ignore */ }
  finally { logLoading.value = false }
}

function applyPage(page: LogPage, replace: boolean) {
  if (replace) {
    logLines.value = page.lines
  } else {
    logLines.value.push(...page.lines)
  }
  endOffset.value = page.end_offset
  totalBytes.value = page.total_bytes
}

async function loadMore() {
  if (endOffset.value >= totalBytes.value) return
  loadingMore.value = true
  try {
    const page = await tasksApi.log(props.taskId, { offset: endOffset.value, lines: 200 })
    applyPage(page, false)
  } catch { /* ignore */ }
  finally { loadingMore.value = false }
}

// ── SSE follow mode ──
let eventSource: EventSource | null = null

function startFollow() {
  stopFollow()
  eventSource = tasksApi.logStream(props.taskId, endOffset.value)
  eventSource.addEventListener('lines', (e: MessageEvent) => {
    try {
      const page: LogPage = JSON.parse(e.data)
      logLines.value.push(...page.lines)
      endOffset.value = page.end_offset
      totalBytes.value = page.total_bytes
      nextTick(() => {
        const el = logContainer.value
        if (el) el.scrollTop = el.scrollHeight
      })
    } catch { /* ignore parse errors */ }
  })
  eventSource.onerror = () => {
    // SSE auto-reconnects; nothing to do unless we want to surface it.
  }
}

function stopFollow() {
  if (eventSource) {
    eventSource.close()
    eventSource = null
  }
}

watch(following, (on) => {
  if (on) {
    startFollow()
    nextTick(() => {
      const el = logContainer.value
      if (el) el.scrollTop = el.scrollHeight
    })
  } else {
    stopFollow()
  }
})

// ── Polling for task status + metrics ──
const isActive = computed(() => task.value && ['running', 'pending'].includes(task.value.status))
let pollTimer: ReturnType<typeof setInterval> | null = null

function startPolling() {
  stopPolling()
  pollTimer = setInterval(async () => {
    await Promise.all([fetchTask(), fetchMetrics()])
  }, 3000)
}

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
}

watch(isActive, (active, prev) => {
  if (active) {
    startPolling()
    if (!prev) following.value = true
  } else {
    stopPolling()
    following.value = false
  }
})

// ── Actions ──
const killing = ref(false)
async function killTask() {
  if (killing.value) return
  const ok = await confirmDialog({
    title: t('confirm.kill_task_title'),
    body: t('confirm.kill_task_body', { id: props.taskId.slice(0, 8) }),
    confirmText: t('job.kill'),
    danger: true,
  })
  if (!ok) return
  killing.value = true
  try {
    await tasksApi.kill(props.taskId)
    if (config.killAsync) {
      cancelPending.value = true
      snack.info('Cancel requested')
    } else {
      snack.success('Task killed')
    }
    fetchTask()
  } catch (e: any) { snack.error(e?.message || 'Kill failed') }
  finally { killing.value = false }
}

const retrying = ref(false)
async function retryTask() {
  if (retrying.value) return
  retrying.value = true
  try {
    await tasksApi.retry(props.taskId)
    snack.success('Task retried')
    fetchTask()
  } catch (e: any) { snack.error(e?.message || 'Retry failed') }
  finally { retrying.value = false }
}

function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}

function formatBytes(b: number): string {
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  if (b < 1024 * 1024 * 1024) return `${(b / (1024 * 1024)).toFixed(1)} MB`
  return `${(b / (1024 * 1024 * 1024)).toFixed(1)} GB`
}

onMounted(async () => {
  fetchWandbInfo() // fire-and-forget — best effort, doesn't block render
  await fetchTask()
  await Promise.all([fetchLog(), fetchMetrics()])
  if (isActive.value) {
    startPolling()
    following.value = true
  }
  nextTick(() => {
    const el = logContainer.value
    if (el) el.scrollTop = el.scrollHeight
  })
})

onUnmounted(() => {
  stopPolling()
  stopFollow()
})
</script>

<style scoped>
/* Log wants a full-bleed terminal block — strip the panel-text padding. */
.log-panel :deep(.v-expansion-panel-text__wrapper) {
  padding: 0;
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
.log-error {
  color: rgb(var(--v-theme-error));
  background: rgba(var(--v-theme-error), 0.06);
}
.log-warning {
  color: rgb(var(--v-theme-warning));
}
.log-info {
  opacity: 0.75;
}
.log-debug {
  opacity: 0.5;
}

/* ── Traceback ── */
.log-user-code {
  font-weight: 600;
  background: rgba(var(--v-theme-error), 0.1);
}
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

/* ── Inline highlights ── */
.log-metric {
  background: rgba(var(--v-theme-success), 0.15);
  border-radius: 2px;
  padding: 0 2px;
}
.log-rank {
  font-weight: 600;
}

/* ── tqdm badge ── */
.log-tqdm-badge {
  vertical-align: text-bottom;
}

/* ── Drain block (column-aligned) ── */
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
.drain-col {
  display: inline-block;
  padding-right: 1ch;
}
.drain-static { color: #475569; }
.drain-var { color: #e2e8f0; font-weight: 500; }

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
