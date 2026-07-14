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
              v-if="canLoadEarlier"
              size="x-small" variant="text" :loading="loadingEarlier" @click="loadEarlier"
            >
              {{ t('log.load_earlier') }}
            </v-btn>
            <v-btn
              v-if="!following && endOffset < totalBytes"
              size="x-small" variant="text" :loading="loadingMore" @click="loadMore"
            >
              Load more
            </v-btn>
            <v-switch
              v-model="following"
              density="compact" hide-details inline
              :color="streamState === 'reconnecting' ? 'warning' : 'primary'"
              :label="streamState === 'reconnecting' ? t('log.reconnecting') : 'Follow'"
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
              :line-number-base="lineNumberBase"
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
import { tasksApi, DEFAULT_LOG_MAX_BYTES } from '@/apis/tasks'
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
import { applyPage as applyCursorPage } from '@/utils/log/cursor'
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

// ── Log state (byte-offset based, log stream contract v2) ──
const logLines = ref<string[]>([])
/** Lines released from memory by the follow-mode ring buffer. */
const trimmedLines = ref(0)
/** Byte offset of logLines[0] — the backward-paging anchor. */
const firstOffset = ref(0)
const endOffset = ref(0)
const totalBytes = ref(0)
/** The buffer's last line is an unterminated fragment (page.partial). */
const tailPartial = ref(false)
/** Absolute 0-based line number of logLines[0]; -1 = unknown. */
const startLine = ref(-1)

// Absolute line numbers only in the archive view with a known base and an
// untrimmed buffer; the live view hides the column (contract v2).
const lineNumberBase = computed(() =>
  !isActive.value && startLine.value >= 0 && trimmedLines.value === 0 ? startLine.value : -1)

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
// `reconnecting` is follow INTENT with the stream down — tail -f
// semantics: the intent never expires, retries back off forever (1s→10s)
// until the stream returns, the user toggles off, or the task ends.
type LogStreamState = 'loading' | 'ready' | 'following' | 'reconnecting'
const streamState = ref<LogStreamState>('loading')

/** v-switch model: a view over streamState with transition legality.
 *  The switch stays ON through `reconnecting` — that IS still following. */
const following = computed({
  get: () => streamState.value === 'following' || streamState.value === 'reconnecting',
  set: (on: boolean) => {
    if (on) {
      if (streamState.value === 'ready') streamState.value = 'following'
      // from `loading`: ignored — illegal transition, not a race to patch
    } else if (streamState.value === 'following' || streamState.value === 'reconnecting') {
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
  toggleGroup, scrollToGroup, scrollToBottom, replaceTailLine,
} = surface

// ── Apply pages through the pure cursor state machine (utils/log/cursor).
// Returns false when the cursor invariant was violated — the caller must
// resync instead of applying the page. ──
function acceptPage(page: LogPage): boolean {
  const action = applyCursorPage(
    { endOffset: endOffset.value, tailPartial: tailPartial.value }, page)
  switch (action.kind) {
    case 'ignore':
      return true
    case 'reset':
      // Rotation: clear buffer + trim counter; the new array reference
      // makes the incremental pipeline rebuild from scratch.
      trimmedLines.value = 0
      logLines.value = action.lines.slice()
      firstOffset.value = 0
      endOffset.value = action.nextOffset
      totalBytes.value = action.size
      tailPartial.value = action.tailPartial
      startLine.value = -1
      return true
    case 'append': {
      const lines = action.lines
      if (action.mergeFirst && logLines.value.length > 0 && lines.length > 0) {
        // continues chain: glue the first fragment onto our tail line.
        // The watch in useLogSurface can't see a same-length mutation, so
        // the engine is notified explicitly via replaceTailLine.
        const last = logLines.value.length - 1
        const merged = logLines.value[last] + lines[0]
        logLines.value[last] = merged
        replaceTailLine(merged)
        if (lines.length > 1) logLines.value.push(...lines.slice(1))
      } else {
        logLines.value.push(...lines)
      }
      endOffset.value = action.nextOffset
      totalBytes.value = action.size
      tailPartial.value = action.tailPartial
      return true
    }
    case 'resync':
      return false
  }
}

/** Seed/replace the whole buffer from one tail-opened page. */
function seedFromTail(page: LogPage, active: boolean) {
  trimmedLines.value = 0
  logLines.value = (page.lines ?? []).slice()
  firstOffset.value = page.offset
  endOffset.value = page.next_offset
  totalBytes.value = page.size
  tailPartial.value = !!page.partial
  startLine.value = !active && page.start_line != null && page.start_line >= 0
    ? page.start_line : -1
}

// ── Open / reload: live AND archive both open from the tail; only the
// archive first page asks for line counts (count_lines=1). ──
async function reloadTail() {
  const active = isActive.value
  const page = await tasksApi.log(props.taskId, {
    tail: true, maxBytes: DEFAULT_LOG_MAX_BYTES, countLines: !active,
  })
  seedFromTail(page, active)
  nextTick(() => scrollToBottom())
}

/** Re-read the log from byte 0 (recovers ring-buffer-trimmed history). */
async function reloadFromStart() {
  streamState.value = 'loading' // closes SSE; re-entry only via ready
  trimmedLines.value = 0
  logLoading.value = true
  try {
    const page = await tasksApi.log(props.taskId, { offset: 0, maxBytes: DEFAULT_LOG_MAX_BYTES })
    logLines.value = (page.lines ?? []).slice()
    firstOffset.value = 0
    endOffset.value = page.next_offset
    totalBytes.value = page.size
    tailPartial.value = !!page.partial
    startLine.value = 0 // buffer head IS the file head
  } catch { /* ignore */ }
  finally { logLoading.value = false }
  streamState.value = 'ready'
}

// ── Forward paging (archive, buffer not yet at EOF) ──
async function loadMore() {
  if (endOffset.value >= totalBytes.value) return
  loadingMore.value = true
  try {
    const page = await tasksApi.log(props.taskId, {
      offset: endOffset.value, maxBytes: DEFAULT_LOG_MAX_BYTES,
    })
    if (!acceptPage(page)) void resync()
  } catch { /* ignore */ }
  finally { loadingMore.value = false }
}

// ── Backward paging (archive "Load earlier") ──
// Requests [max(0, firstOffset − budget), firstOffset). firstOffset is a
// line boundary after tail-open, so the page's next_offset lands exactly
// on it; when the window START falls mid-line the first entry is a
// continuation fragment shown as its (head-truncated) line — the 1 entry
// = 1 line accounting still holds, so start_line just decrements by the
// prepended count. No auto-trim on backward paging (contract v2).
const loadingEarlier = ref(false)
const canLoadEarlier = computed(() =>
  !isActive.value && !following.value && firstOffset.value > 0 && trimmedLines.value === 0)

async function loadEarlier() {
  if (loadingEarlier.value || firstOffset.value <= 0) return
  loadingEarlier.value = true
  try {
    const target = firstOffset.value
    const start = Math.max(0, target - DEFAULT_LOG_MAX_BYTES)
    const page = await tasksApi.log(props.taskId, { offset: start, maxBytes: target - start })
    if (firstOffset.value !== target) return // raced with rotation/reload: drop
    const lines = page.lines ?? []
    if (lines.length === 0) return
    // Prepend: the new array reference makes the pipeline reset (accepted).
    logLines.value = [...lines, ...logLines.value]
    firstOffset.value = page.offset
    if (startLine.value >= 0) {
      // page.partial here means a mega-line fragment that never reached a
      // newline — it belongs to the SAME line as our previous head, so the
      // per-entry accounting breaks: fall back to "base unknown".
      const base = startLine.value - lines.length
      startLine.value = page.partial || base < 0 ? -1 : base
    }
  } catch { /* ignore */ }
  finally { loadingEarlier.value = false }
}

// ── SSE follow mode ──
let eventSource: EventSource | null = null

function startFollow() {
  stopFollow()
  eventSource = tasksApi.logStream(props.taskId, endOffset.value, DEFAULT_LOG_MAX_BYTES)
  eventSource.addEventListener('lines', (e: MessageEvent) => {
    try {
      const page: LogPage = JSON.parse(e.data)
      // Cursor state machine: rotated → reset, continues → fragment merge,
      // duplicates → dropped. A violated offset assertion does NOT drop
      // the page silently anymore — it converts into a resync, which also
      // re-opens the stream from the repaired cursor (the current stream's
      // server-side cursor is out of sync with ours by definition here).
      if (!acceptPage(page)) {
        void resync(true)
        return
      }
      // Follow mode = pinned to tail: safe to release the oldest lines
      // (server file keeps everything; a banner explains the gap).
      trimmedLines.value += trimLogBuffer(logLines.value)
      nextTick(() => scrollToBottom())
    } catch { /* ignore parse errors */ }
  })
  eventSource.onopen = () => {
    // Stream (re)established: reconnecting → following, backoff resets.
    reconnectDelay = 1_000
    if (streamState.value === 'reconnecting') streamState.value = 'following'
  }
  eventSource.onerror = () => {
    // ONE recovery path owns every failure mode. Native auto-reconnect is
    // deliberately killed here (close()): it lingers in CONNECTING with
    // the ORIGINAL URL and — as observed on a real daemon restart — can
    // spin there forever without recovering (RQ-54). We reconnect
    // ourselves from the LATEST endOffset instead; onopen flips back to
    // `following`. tail -f semantics: the intent never expires.
    stopFollow()
    if (streamState.value === 'following') {
      streamState.value = 'reconnecting' // state watcher schedules the loop
    } else if (streamState.value === 'reconnecting') {
      scheduleReconnect() // a retry attempt failed — schedule the next one
    }
  }
}

// ── Infinite reconnect loop (event-driven): each attempt aligns the
// buffer with one GET, then reopens the stream; a failed attempt fires
// onerror again, which schedules the next one with doubled backoff. ──
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let reconnectDelay = 1_000
function scheduleReconnect() {
  if (reconnectTimer) return
  reconnectTimer = setTimeout(async () => {
    reconnectTimer = null
    if (streamState.value !== 'reconnecting') return
    reconnectDelay = Math.min(reconnectDelay * 2, 10_000)
    try {
      const from = endOffset.value
      const page = await tasksApi.log(props.taskId, { offset: from, maxBytes: DEFAULT_LOG_MAX_BYTES })
      if (endOffset.value === from) acceptPage(page)
    } catch { /* backend still down — the reopened stream will error again */ }
    if (streamState.value === 'reconnecting') startFollow() // onopen flips back
  }, reconnectDelay)
}

// ── Resync: the single recovery path (contract v2). One GET from our
// endOffset pulls the buffer level; if that GET fails (or its page still
// violates the cursor), reload the whole tail; only THAT failing surfaces
// the degraded snackbar. ──
let resyncing = false
async function resync(restartStream = false) {
  if (resyncing) return
  resyncing = true
  try {
    const from = endOffset.value
    let ok = false
    try {
      const page = await tasksApi.log(props.taskId, {
        offset: from, maxBytes: DEFAULT_LOG_MAX_BYTES,
      })
      // If something already advanced the cursor meanwhile, that path
      // owns the buffer now — this resync is settled.
      ok = endOffset.value !== from || acceptPage(page)
    } catch { ok = false }
    if (!ok) {
      try { await reloadTail() } catch {
        // Backend unreachable. If we were following, the intent survives:
        // hand recovery to the infinite reconnect loop instead of giving
        // up (tail -f semantics). Outside follow, surface it once.
        if (streamState.value === 'following') streamState.value = 'reconnecting'
        else if (streamState.value !== 'reconnecting') snack.warn(t('log.stream_lost'))
        return
      }
    }
    if (restartStream && streamState.value === 'following') startFollow()
  } finally { resyncing = false }
}

function stopFollow() {
  if (eventSource) {
    eventSource.close()
    eventSource = null
  }
}

// Side effects live on the STATE transition, not on scattered call sites.
watch(streamState, (state, prev) => {
  if (state === 'following') {
    // Coming back from `reconnecting`, the retry loop already opened the
    // stream (onopen brought us here) — restarting would kill it.
    if (prev !== 'reconnecting') {
      startFollow()
      nextTick(() => scrollToBottom())
    }
  } else if (state === 'reconnecting') {
    scheduleReconnect()
  } else {
    stopFollow()
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null }
    reconnectDelay = 1_000
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
      // once from our cursor so the exit message is never missing.
      try {
        const tailFrom = endOffset.value
        const page = await tasksApi.log(props.taskId, {
          offset: tailFrom, maxBytes: DEFAULT_LOG_MAX_BYTES,
        })
        // Generation guard: if SSE advanced the offset while this request
        // was in flight, the same lines already rendered — drop the page.
        if (endOffset.value !== tailFrom) return
        if (!acceptPage(page)) void resync()
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

/** The first log fetch needs to know live vs archive (count_lines only on
 *  archive) — wait for the task row; the query cache may already have it. */
function waitForTask(): Promise<void> {
  if (task.value || notFound.value) return Promise.resolve()
  return new Promise((resolve) => {
    const stop = watch([task, notFound], ([tv, nf]) => {
      if (tv || nf) {
        stop()
        resolve()
      }
    })
  })
}

onMounted(async () => {
  fetchWandbInfo() // fire-and-forget — best effort, doesn't block render
  await waitForTask()
  if (notFound.value) return
  logLoading.value = true
  try { await reloadTail() } catch { /* ignore */ }
  finally { logLoading.value = false }
  streamState.value = 'ready' // GET settled: offsets are now trustworthy
  // Query cache may already have the task (navigated from the job page);
  // the immediate isActive value seeds follow, the watch handles flips.
  if (isActive.value) following.value = true
  nextTick(() => scrollToBottom())
})

onUnmounted(() => {
  stopFollow()
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null }
})
</script>

<style scoped>
/* Log wants a full-bleed terminal block — strip the panel-text padding.
   The log surface itself (lines, folds, search) lives in LogSurfaceView. */
.log-panel :deep(.v-expansion-panel-text__wrapper) {
  padding: 0;
}
</style>
