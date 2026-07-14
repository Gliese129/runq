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
          <!-- -1 = unlimited (explicit opt-in); 0 legitimately means "no retries" -->
          <div class="text-body-2 font-weight-medium">{{ task.retry_count || 0 }} / {{ task.max_retry < 0 ? '∞' : task.max_retry }}</div>
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
          <!-- Ring-buffer notice: memory released, server file complete.
               "Reload from start" is the interim recovery path until the
               open-at-tail / scroll-up backfill work (RQ-22) lands. -->
          <div v-if="trimmedLines > 0" class="d-flex align-center text-caption text-on-surface-variant px-2 pb-1">
            <v-icon size="12" class="mr-1">mdi-history</v-icon>{{ t('log.trimmed', { n: trimmedLines }) }}
            <v-btn size="x-small" variant="text" color="primary" class="ml-2" @click="reloadFromStart">
              {{ t('log.reload_start') }}
            </v-btn>
          </div>
          <div class="d-flex">
            <!-- Log content (virtualized shared surface, incl. search bar) -->
            <LogSurfaceView
              :surface="surface"
              :items="renderItems"
              :log-loading="logLoading"
              :empty-text="task.status === 'pending' ? 'Waiting to start...' : 'No log output yet'"
            />
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

  <!-- 404: stale link / cleaned task — a spinner forever was the old bug -->
  <v-card v-else-if="notFound" class="pa-8 text-center">
    <v-icon size="40" color="on-surface-variant" class="mb-3" style="opacity: 0.5">mdi-file-question-outline</v-icon>
    <div class="text-h6 mb-1">{{ t('task.not_found') }}</div>
    <v-btn class="mt-3" variant="tonal" color="primary"
      :to="{ name: 'job-detail', params: { project: props.project, jobId: props.jobId } }">
      {{ t('common.back') }}
    </v-btn>
  </v-card>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { tasksApi } from '@/apis/tasks'
import { jobsApi } from '@/apis/jobs'
import { ApiError } from '@/apis/client'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfirm } from '@/composables/useConfirm'
import { useCancelling } from '@/composables/useCancelling'
import { useTaskQuery, useTaskMetricsQuery, useTaskActions } from '@/queries/useTaskQueries'
import { useConfigStore } from '@/stores/config'
import { useLogViewerStore } from '@/stores/logViewer'
import MetricsChart from '@/components/MetricsChart.vue'
import TaskStatusBadge from '@/components/TaskStatusBadge.vue'
import LogSidePanel from '@/components/LogSidePanel.vue'
import LogSurfaceView from '@/components/LogSurfaceView.vue'
import type { LogPage } from '@/types/api'
import { trimLogBuffer } from '@/utils/log/buffer'
import { useLogSurface } from '@/composables/useLogSurface'

const props = defineProps<{ project: string; jobId: string; taskId: string }>()
const snack = useSnackbar()
const config = useConfigStore()
const logStore = useLogViewerStore()
const { t } = useI18n()
const { confirm: confirmDialog } = useConfirm()

// ── Server state: query cache owns task + metrics (polls while active,
// stops on terminal states, pauses in background tabs). ──
const taskQuery = useTaskQuery(() => props.taskId)
const task = computed(() => taskQuery.data.value ?? null)
const isActive = computed(() => !!task.value && ['running', 'pending'].includes(task.value.status))
// client.ts mutes 404 snackbars by design — the page must render the
// absence itself instead of spinning forever.
const notFound = computed(() =>
  taskQuery.error.value instanceof ApiError && taskQuery.error.value.status === 404)

const metricsQuery = useTaskMetricsQuery(() => props.taskId, isActive)
const metricPoints = computed(() => metricsQuery.data.value ?? [])

// Kill in flight — shared overlay, same state the job page renders.
const { cancelling, prune, displayStatus: overlayStatus } = useCancelling()
const displayStatus = computed(() => (task.value ? overlayStatus(task.value) : ''))
watch(task, (v) => { if (v) prune([v]) })
void cancelling

// ── Log state (byte-offset based) ──
const logLines = ref<string[]>([])
/** Lines released from memory by the follow-mode ring buffer. */
const trimmedLines = ref(0)
const endOffset = ref(0)
const totalBytes = ref(0)

// ── Log stream lifecycle ──
// One explicit state instead of independent booleans whose combinations
// were never defined (the GET×SSE offset race, RQ-54, lived exactly in an
// undefined combination):
//
//   loading ──GET settled──▶ ready ◀──toggle/terminal/stream-lost── following
//      ▲                                │
//      └────────── reload ◀─────────────┘
//
// The only legal entry into `following` is FROM `ready` — encoded in the
// `following` setter, so no call site can start SSE before offsets exist.
type LogStreamState = 'loading' | 'ready' | 'following'
const streamState = ref<LogStreamState>('loading')

/** v-switch model: a view over streamState with transition legality. */
const following = computed({
  get: () => streamState.value === 'following',
  set: (on: boolean) => {
    if (on) {
      if (streamState.value === 'ready') streamState.value = 'following'
      // from `loading`: ignored — illegal transition, not a race to patch
    } else if (streamState.value === 'following') {
      streamState.value = 'ready'
    }
  },
})
const logLoading = ref(false)
const loadingMore = ref(false)
const logContainer = ref<HTMLElement>()

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

// ── Shared log surface (pipeline, fold state, search) — see useLogSurface.
// The whole object goes to LogSurfaceView; only the pieces the side panel
// and follow mode need are destructured here. ──
const surface = useLogSurface(logLines, logContainer)
const {
  pipelineResult, effectiveHidden, renderItems,
  toggleGroup, scrollToGroup, scrollToBottom,
} = surface

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
  endOffset.value = page.next_offset
  totalBytes.value = page.size
}

/** Re-read the log from byte 0 (recovers ring-buffer-trimmed history). */
async function reloadFromStart() {
  streamState.value = 'loading' // closes SSE; re-entry only via ready
  trimmedLines.value = 0
  await fetchLog(0, 500)
  streamState.value = 'ready'
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
      // Cursor guard: only accept the page that continues our buffer —
      // drops duplicates from any GET/SSE race instead of appending them.
      if (page.offset !== endOffset.value) return
      logLines.value.push(...page.lines)
      // Follow mode = pinned to tail: safe to release the oldest lines
      // (server file keeps everything; a banner explains the gap).
      trimmedLines.value += trimLogBuffer(logLines.value)
      endOffset.value = page.next_offset
      totalBytes.value = page.size
      nextTick(() => scrollToBottom())
    } catch { /* ignore parse errors */ }
  })
  eventSource.onerror = () => {
    // EventSource only auto-reconnects on network-level errors. When the
    // SERVER closes the stream (task ended, daemon restart, proxy cut)
    // readyState goes CLOSED and no reconnect happens — surface it
    // instead of leaving the Follow toggle silently lying.
    if (eventSource?.readyState === EventSource.CLOSED) {
      following.value = false
      snack.warn(t('log.stream_lost'))
    }
  }
}

function stopFollow() {
  if (eventSource) {
    eventSource.close()
    eventSource = null
  }
}

// Side effects live on the STATE transition, not on scattered call sites.
watch(streamState, (state) => {
  if (state === 'following') {
    startFollow()
    nextTick(() => scrollToBottom())
  } else {
    stopFollow()
  }
})

// Status/metrics polling is owned by the queries above; this watch only
// drives the log-follow toggle across the active/terminal transition.
watch(isActive, async (active, prev) => {
  if (active) {
    // Legal only from `ready` — the setter enforces it, so a task query
    // that resolves before the initial GET simply no-ops here and
    // onMounted seeds follow after loading.
    if (!prev) following.value = true
  } else {
    following.value = false // closes SSE via the following watcher
    if (prev) {
      // Final metrics refetch: polling stops on terminal states, but the
      // last batch (often THE final score) lands right at the flip.
      metricsQuery.refetch()
      // The stream may die before the final buffered lines flush — fetch
      // the tail once so the exit message is never missing.
      try {
        const tailFrom = endOffset.value
        const page = await tasksApi.log(props.taskId, { offset: tailFrom, lines: 500 })
        // Generation guard: if SSE advanced the offset while this request
        // was in flight, the same lines already rendered — drop the page.
        if (endOffset.value !== tailFrom) return
        applyPage(page, false)
      } catch { /* best effort */ }
    }
  }
})

// ── Actions: mutations invalidate task + parent job + lists. ──
const taskActions = useTaskActions(() => props.jobId)
const killing = computed(() => taskActions.kill.isPending.value)
const retrying = computed(() => taskActions.retry.isPending.value)

async function killTask() {
  if (killing.value) return
  const ok = await confirmDialog({
    title: t('confirm.kill_task_title'),
    body: t('confirm.kill_task_body', { id: props.taskId.slice(0, 8) }),
    confirmText: t('job.kill'),
    danger: true,
  })
  if (!ok) return
  try {
    await taskActions.kill.mutateAsync(props.taskId)
    if (config.killAsync) snack.info('Cancel requested')
    else snack.success('Task killed')
  } catch (e: any) { snack.error(e?.message || 'Kill failed') }
}

async function retryTask() {
  if (retrying.value) return
  try {
    await taskActions.retry.mutateAsync(props.taskId)
    snack.success('Task retried')
  } catch (e: any) { snack.error(e?.message || 'Retry failed') }
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
  await fetchLog()
  streamState.value = 'ready' // GET settled: offsets are now trustworthy
  // Query cache may already have the task (navigated from the job page);
  // the immediate isActive value seeds follow, the watch handles flips.
  if (isActive.value) following.value = true
  nextTick(() => scrollToBottom())
})

onUnmounted(() => {
  stopFollow()
})
</script>

<style scoped>
/* Log wants a full-bleed terminal block — strip the panel-text padding.
   The log surface itself (lines, folds, search) lives in LogSurfaceView. */
.log-panel :deep(.v-expansion-panel-text__wrapper) {
  padding: 0;
}
</style>
