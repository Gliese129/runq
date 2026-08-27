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
          <!-- unknown (RQ-74): kill is the user's escape hatch for a
               submission whose outcome reconcile cannot settle -->
          <v-btn
            v-if="displayStatus === 'running' || displayStatus === 'unknown'"
            size="x-small"
            variant="tonal"
            color="error"
            :loading="killing"
            :disabled="killing"
            @click="killTask"
          >
            <v-icon start size="14">mdi-stop</v-icon> {{ t('job.kill') }}
          </v-btn>
          <v-btn
            v-if="config.caps.retry && (task.status === 'failed' || task.status === 'killed')"
            size="x-small"
            variant="tonal"
            color="primary"
            :loading="retrying"
            :disabled="retrying"
            @click="retryTask"
          >
            <v-icon start size="14">mdi-refresh</v-icon> {{ t('job.retry') }}
          </v-btn>
        </div>
      </div>

      <!-- Stats -->
      <v-row dense>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('job.elapsed') }}</div>
          <div class="text-body-2 font-weight-medium">
            {{ task.elapsed_seconds ? formatDuration(task.elapsed_seconds) : '—' }}
          </div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('job.step') }}</div>
          <div class="text-body-2 font-weight-medium">{{ task.current_step ?? '—' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('task.retries') }}</div>
          <!-- -1 = unlimited (explicit opt-in); 0 legitimately means "no retries" -->
          <div class="text-body-2 font-weight-medium">
            {{ task.retry_count || 0 }} / {{ task.max_retry < 0 ? '∞' : task.max_retry }}
          </div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('task.gpus') }}</div>
          <div class="text-body-2 font-weight-medium">{{ task.gpus || '—' }}</div>
        </v-col>
      </v-row>
      <!-- Params ARE the identity of a task — above the fold on both tabs;
           swept ones (the job's axes) carry the accent. -->
      <div class="d-flex ga-1 flex-wrap mt-3">
        <v-chip
          v-for="(val, key) in task.params" :key="key"
          size="x-small"
          :variant="sweptParams.has(String(key)) ? 'tonal' : 'outlined'"
          :color="sweptParams.has(String(key)) ? 'secondary' : undefined"
          class="font-mono"
        >{{ key }} = {{ val }}</v-chip>
      </div>
      <!-- HPC-specific stats (poll-model only) -->
      <v-row v-if="task.external_id" dense class="mt-1">
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('task.external_id') }}</div>
          <div class="text-body-2 font-weight-medium">
            <code>{{ task.external_id }}</code>
          </div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('task.scheduler_state') }}</div>
          <div class="text-body-2 font-weight-medium">{{ task.native_state || '—' }}</div>
        </v-col>
        <v-col cols="6" sm="3">
          <div class="text-caption text-on-surface-variant">{{ t('task.queue') }}</div>
          <div class="text-body-2 font-weight-medium">{{ task.queue || '—' }}</div>
        </v-col>
      </v-row>
    </v-card>

    <!-- RQ-74: submit-phase self-report — the task has no log yet, so its
         submit evidence explains the state: red = rejected (dead), amber =
         retrying (pending) or outcome unknown (awaiting reconcile). The
         evidence itself is always verbatim, never interpreted. -->
    <v-card v-if="task.failure_detail" variant="tonal" :color="failureCard.color" class="mb-4 pa-3">
      <div class="d-flex align-center ga-2 mb-2">
        <v-icon size="18">{{ failureCard.icon }}</v-icon>
        <span class="text-body-2 font-weight-medium">{{ failureCard.title }}</span>
        <v-spacer />
        <v-btn size="x-small" variant="tonal" @click="copyFailureDetail">
          <v-icon start size="12">mdi-content-copy</v-icon>
          {{ detailCopied ? t('task.copied') : t('task.copy_detail') }}
        </v-btn>
      </div>
      <pre class="failure-detail text-caption">{{ task.failure_detail }}</pre>
    </v-card>

    <!-- Run | Log tabs (RQ2-4 ①, kit ScreensTask): one task is a real
         destination — curves + identity on Run, the density curve + full
         log surface on Log. Deep-linkable via ?tab=. -->
    <v-tabs v-model="tab" density="compact" class="mb-3" color="primary">
      <v-tab value="run" size="small">{{ t('task.tab_run') }}</v-tab>
      <v-tab value="log" size="small">{{ t('task.tab_log') }}</v-tab>
    </v-tabs>

    <!-- ── Run tab ── -->
    <div v-if="tab === 'run'" class="run-grid">
      <v-card class="pa-0">
        <div class="d-flex align-center ga-1 px-3 py-2 border-b">
          <v-icon size="16">mdi-chart-line</v-icon>
          <span class="text-subtitle-2">{{ t('task.metrics') }}</span>
          <v-spacer />
          <v-btn
            v-if="task.wandb_run_id"
            size="x-small"
            variant="text"
            :href="wandbRunURL(task.wandb_run_id)"
            target="_blank"
          >
            <v-icon start size="14">mdi-open-in-new</v-icon> W&B
          </v-btn>
        </div>
        <div class="pa-3">
          <MetricsChart :points="metricPoints" />
        </div>
      </v-card>

      <v-card class="pa-4">
        <!-- Parameters: swept vs fixed is a property of the JOB — a param
             is an axis only if sibling tasks disagree about it. -->
        <div class="text-subtitle-2 mb-2">{{ t('submit.parameters') }}</div>
        <div class="param-list mb-2">
          <div
            v-for="(val, key) in task.params" :key="key"
            class="param-row font-mono"
            :class="{ 'param-swept': sweptParams.has(String(key)) }"
          >
            <span class="param-key">{{ key }}</span>
            <span class="param-val" :class="{ 'text-secondary font-weight-bold': sweptParams.has(String(key)) }">{{ val }}</span>
          </div>
        </div>
        <div class="d-flex align-center ga-3 text-caption text-on-surface-variant mb-4">
          <span class="d-inline-flex align-center ga-1"><span class="legend-box legend-swept" />{{ t('task.swept') }}</span>
          <span class="d-inline-flex align-center ga-1"><span class="legend-box legend-fixed" />{{ t('task.fixed_for_job') }}</span>
        </div>

        <template v-if="latestMetrics.length">
          <div class="text-subtitle-2 mb-2">
            {{ t('task.latest_metrics') }}
            <span class="text-caption text-on-surface-variant ml-1">{{ t('job.step') }} {{ task.current_step ?? '—' }}</span>
          </div>
          <div class="param-list mb-4">
            <div v-for="[k, v] in latestMetrics" :key="k" class="param-row font-mono">
              <span class="param-key">{{ k }}</span>
              <span class="param-val">{{ v }}</span>
            </div>
          </div>
        </template>

        <div class="text-subtitle-2 mb-2">{{ t('task.execution') }}</div>
        <div class="kv-row"><span class="kv-key">{{ t('task.exec_job') }}</span><code>{{ props.jobId.slice(0, 10) }}</code></div>
        <div v-if="jobTarget" class="kv-row"><span class="kv-key">{{ t('task.exec_target') }}</span><code>{{ jobTarget }}</code></div>
        <div v-if="task.command" class="kv-row"><span class="kv-key">{{ t('task.exec_command') }}</span><code>{{ task.command }}</code></div>
        <div v-if="task.working_dir" class="kv-row"><span class="kv-key">{{ t('task.exec_working_dir') }}</span><code>{{ task.working_dir }}</code></div>
        <div v-if="task.task_dir" class="kv-row"><span class="kv-key">{{ t('task.exec_task_dir') }}</span><code>{{ task.task_dir }}</code></div>
        <div class="kv-row"><span class="kv-key">{{ t('job.exit_code') }}</span><code>{{ task.exit_code ?? '—' }}</code></div>
        <div class="kv-row">
          <span class="kv-key">{{ t('job.elapsed') }}</span>
          <code>{{ task.elapsed_seconds ? formatDuration(task.elapsed_seconds) : '—' }}</code>
        </div>
      </v-card>
    </div>

    <!-- ── Log tab ── -->
    <v-card v-else class="pa-0">
      <!-- Density curve: shape → jump. Hidden until two samples exist
           (recordActivity samples every 60s). -->
      <div v-if="activityPoints.length >= 2" class="px-3 py-2 border-b">
        <TaskActivityCurve
          :points="activityPoints"
          :bucket-minutes="activityBucketMinutes"
          :seek-at="seekIndex"
          @seek="onSeek"
        />
      </div>

      <!-- Path + seek state + paging/follow controls -->
      <div class="d-flex align-center ga-2 px-3 py-2 border-b flex-wrap">
        <code v-if="task.log_path" class="text-caption text-on-surface-variant text-truncate">{{ task.log_path }}</code>
        <template v-if="seekIndex >= 0">
          <v-chip size="x-small" variant="tonal" color="primary">
            {{ t('activity.jumped_line', { n: seekLine.toLocaleString() }) }}
          </v-chip>
          <span
            class="text-caption text-primary cursor-pointer"
            role="button" tabindex="0"
            @click="backToTail"
            @keydown.enter="backToTail"
          >{{ t('activity.back_to_tail') }}</span>
        </template>
        <v-spacer />
        <v-chip v-if="totalBytes > 0" size="x-small" variant="tonal">{{ formatBytes(totalBytes) }}</v-chip>
        <v-btn
          v-if="canLoadEarlier"
          size="x-small"
          variant="text"
          :loading="loadingEarlier"
          @click="loadEarlier"
        >
          {{ t('log.load_earlier') }}
        </v-btn>
        <v-btn
          v-if="!following && endOffset < totalBytes"
          size="x-small"
          variant="text"
          :loading="loadingMore"
          @click="loadMore"
        >
          {{ t('log.load_more') }}
        </v-btn>
        <v-switch
          v-model="following"
          density="compact"
          hide-details
          inline
          :color="streamState === 'reconnecting' ? 'warning' : 'primary'"
          :label="streamState === 'reconnecting' ? t('log.reconnecting') : t('log.follow')"
        />
      </div>

      <!-- Ring-buffer notice: memory released, server file complete.
           "Reload from start" is the interim recovery path until the
           open-at-tail / scroll-up backfill work (RQ-22) lands. -->
      <div
        v-if="trimmedLines > 0"
        class="d-flex align-center text-caption text-on-surface-variant px-2 py-1"
      >
        <v-icon size="12" class="mr-1">mdi-history</v-icon
        >{{ t('log.trimmed', { n: trimmedLines }) }}
        <v-btn
          size="x-small"
          variant="text"
          color="primary"
          class="ml-2"
          @click="reloadFromStart"
        >
          {{ t('log.reload_start') }}
        </v-btn>
      </div>
      <div class="d-flex flex-wrap">
        <!-- Log content (virtualized shared surface, incl. search bar) -->
        <LogSurfaceView
          :surface="surface"
          :items="renderItems"
          :log-loading="logLoading"
          :line-number-base="lineNumberBase"
          :empty-text="task.status === 'pending' ? t('task.waiting_start') : t('log.no_output')"
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
    </v-card>
  </div>

  <!-- 404: stale link / cleaned task — a spinner forever was the old bug -->
  <v-card v-else-if="notFound" class="pa-8 text-center">
    <v-icon size="40" color="on-surface-variant" class="mb-3" style="opacity: 0.5"
      >mdi-file-question-outline</v-icon
    >
    <div class="text-h6 mb-1">{{ t('task.not_found') }}</div>
    <v-btn
      class="mt-3"
      variant="tonal"
      color="primary"
      :to="{ name: 'job-detail', params: { project: props.project, jobId: props.jobId } }"
    >
      {{ t('common.back') }}
    </v-btn>
  </v-card>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>

  <!-- RQ-75: cross-generation rerun confirmation -->
  <GenerationRerunDialog v-model="genRerun.open.value" @confirm="genRerun.confirmRerun" />
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { tasksApi, DEFAULT_LOG_MAX_BYTES } from '@/apis/tasks'
import { jobsApi } from '@/apis/jobs'
import { ApiError } from '@/apis/client'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfirm } from '@/composables/useConfirm'
import { useCancelling } from '@/composables/useCancelling'
import { useTaskQuery, useTaskMetricsQuery, useTaskActions } from '@/queries/useTaskQueries'
import { useGenerationRerun } from '@/composables/useGenerationRerun'
import GenerationRerunDialog from '@/components/GenerationRerunDialog.vue'
import { useConfigStore } from '@/stores/config'
import { useLogViewerStore } from '@/stores/logViewer'
import MetricsChart from '@/components/MetricsChart.vue'
import TaskActivityCurve from './TaskActivityCurve.vue'
import type { ActivityCell } from './activityMath'
import TaskStatusBadge from '@/components/TaskStatusBadge.vue'
import LogSidePanel from '@/components/LogSidePanel.vue'
import LogSurfaceView from '@/components/LogSurfaceView.vue'
import type { ActivityPoint, JobDetail, LogPage } from '@/types/api'
import { trimLogBuffer } from '@/utils/log/buffer'
import { applyPage as applyCursorPage } from '@/utils/log/cursor'
import { useLogSurface } from '@/composables/useLogSurface'
import { formatDuration } from '@/utils/relativeTime'

const props = defineProps<{ project: string; jobId: string; taskId: string }>()
const route = useRoute()
const router = useRouter()
const snack = useSnackbar()
const config = useConfigStore()
const logStore = useLogViewerStore()
const { t } = useI18n()
const { confirm: confirmDialog } = useConfirm()

// ── Server state: query cache owns task + metrics (polls while active,
// stops on terminal states, pauses in background tabs). ──
const taskQuery = useTaskQuery(() => props.taskId)
const task = computed(() => taskQuery.data.value ?? null)
// `unknown` counts as active (RQ-74): reconcile may settle it any moment —
// keep polling task/metrics and let the log follower arm (the log file
// appears the instant the cluster job turns out to be alive).
const isActive = computed(
  () => !!task.value && ['running', 'pending', 'unknown'].includes(task.value.status),
)
// client.ts mutes 404 snackbars by design — the page must render the
// absence itself instead of spinning forever.
const notFound = computed(
  () => taskQuery.error.value instanceof ApiError && taskQuery.error.value.status === 404,
)

const metricsQuery = useTaskMetricsQuery(() => props.taskId, isActive)
const metricPoints = computed(() => metricsQuery.data.value ?? [])

// Kill in flight — shared overlay, same state the job page renders.
const { cancelling, prune, displayStatus: overlayStatus } = useCancelling()
const displayStatus = computed(() => (task.value ? overlayStatus(task.value) : ''))
watch(task, (v) => {
  if (v) prune([v])
})
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
  !isActive.value && startLine.value >= 0 && trimmedLines.value === 0 ? startLine.value : -1,
)

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

// ── Tabs (RQ2-4 ①): ?tab= is the shareable truth; replace keeps the
// back button pointing at the job page, not at tab flips. ──
const tab = ref(route.query.tab === 'log' ? 'log' : 'run')
watch(tab, (v) => {
  router.replace({ query: { ...route.query, tab: v === 'run' ? undefined : v } })
})

// ── Job context: one fetch serves W&B base_url, the target for the
// Execution KV, and the sibling tasks that define swept-vs-fixed. ──
const jobDetail = ref<JobDetail | null>(null)
async function fetchJobContext() {
  try {
    jobDetail.value = await jobsApi.get(props.jobId, { silent: true })
  } catch {
    /* best effort */
  }
}
const jobTarget = computed(() => jobDetail.value?.job.target ?? '')

// Swept vs fixed is a property of the JOB, not the task: a param is an
// axis only if sibling tasks disagree about it.
const sweptParams = computed(() => {
  const s = new Set<string>()
  const siblings = jobDetail.value?.tasks ?? []
  if (siblings.length < 2 || !task.value) return s
  for (const k of Object.keys(task.value.params || {})) {
    if (new Set(siblings.map(sb => String((sb.params || {})[k]))).size > 1) s.add(k)
  }
  return s
})

const latestMetrics = computed(() => Object.entries(task.value?.metrics ?? {}))

function wandbRunURL(runId: string): string {
  return `${jobDetail.value?.wandb?.base_url ?? 'https://wandb.ai'}/runs/${runId}`
}

// ── Log activity (activity.tsv, cumulative columns): fetched when the
// Log tab first shows; while the task is live it re-fetches at the
// sampling interval (60s) — faster would poll ahead of the data. ──
const activityPoints = ref<ActivityPoint[]>([])
const activityBucketMinutes = ref(1)
let activityTimer: ReturnType<typeof setInterval> | null = null
async function fetchActivity() {
  try {
    const res = await jobsApi.activity(props.jobId)
    const mine = res.tasks.find(a => a.task_id === props.taskId)
    activityPoints.value = mine?.points ?? []
    activityBucketMinutes.value = mine?.bucket_minutes || 1
  } catch {
    /* endpoint optional — the curve simply stays hidden */
  }
}
watch([tab, isActive], ([tv, active]) => {
  if (tv === 'log') {
    void fetchActivity()
    if (active && !activityTimer) activityTimer = setInterval(fetchActivity, 60_000)
  }
  if ((tv !== 'log' || !active) && activityTimer) {
    clearInterval(activityTimer)
    activityTimer = null
  }
}, { immediate: true })

// ── Click-seek: the curve hands over the sample's CUMULATIVE position —
// activity.tsv counts complete lines, so cumBytes is a line boundary and
// cumLines is the absolute number of the first line after it. ──
const seekIndex = ref(-1)
const seekLine = ref(0)
async function onSeek(cell: ActivityCell & { index: number }) {
  if (seekIndex.value === cell.index) {
    // toggle off, kit behavior
    void backToTail()
    return
  }
  streamState.value = 'loading' // closes SSE; re-entry only via ready
  logLoading.value = true
  try {
    const page = await tasksApi.log(props.taskId, {
      offset: cell.cumBytes,
      maxBytes: DEFAULT_LOG_MAX_BYTES,
    })
    trimmedLines.value = 0
    logLines.value = (page.lines ?? []).slice()
    firstOffset.value = page.offset
    endOffset.value = page.next_offset
    totalBytes.value = page.size
    tailPartial.value = !!page.partial
    startLine.value = cell.cumLines
    seekIndex.value = cell.index
    seekLine.value = cell.cumLines + 1
  } catch {
    snack.error(t('common.error'))
  } finally {
    logLoading.value = false
  }
  streamState.value = 'ready'
}

async function backToTail() {
  seekIndex.value = -1
  logLoading.value = true
  try {
    await reloadTail()
  } catch {
    /* ignore */
  } finally {
    logLoading.value = false
  }
  streamState.value = 'ready'
  if (isActive.value) following.value = true
}

// ── Shared log surface (pipeline, fold state, search) — see useLogSurface.
// The whole object goes to LogSurfaceView; only the pieces the side panel
// and follow mode need are destructured here. ──
const surface = useLogSurface(logLines, logContainer)
const {
  pipelineResult,
  effectiveHidden,
  renderItems,
  toggleGroup,
  scrollToGroup,
  scrollToBottom,
  replaceTailLine,
} = surface

// ── Apply pages through the pure cursor state machine (utils/log/cursor).
// Returns false when the cursor invariant was violated — the caller must
// resync instead of applying the page. ──
function acceptPage(page: LogPage): boolean {
  const action = applyCursorPage(
    { endOffset: endOffset.value, tailPartial: tailPartial.value },
    page,
  )
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
  startLine.value =
    !active && page.start_line != null && page.start_line >= 0 ? page.start_line : -1
}

// ── Open / reload: live AND archive both open from the tail; only the
// archive first page asks for line counts (count_lines=1). ──
async function reloadTail() {
  const active = isActive.value
  const page = await tasksApi.log(props.taskId, {
    tail: true,
    maxBytes: DEFAULT_LOG_MAX_BYTES,
    countLines: !active,
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
  } catch {
    /* ignore */
  } finally {
    logLoading.value = false
  }
  streamState.value = 'ready'
}

// ── Forward paging (archive, buffer not yet at EOF) ──
async function loadMore() {
  if (endOffset.value >= totalBytes.value) return
  loadingMore.value = true
  try {
    const page = await tasksApi.log(props.taskId, {
      offset: endOffset.value,
      maxBytes: DEFAULT_LOG_MAX_BYTES,
    })
    if (!acceptPage(page)) void resync()
  } catch {
    /* ignore */
  } finally {
    loadingMore.value = false
  }
}

// ── Backward paging (archive "Load earlier") ──
// Requests [max(0, firstOffset − budget), firstOffset). firstOffset is a
// line boundary after tail-open, so the page's next_offset lands exactly
// on it; when the window START falls mid-line the first entry is a
// continuation fragment shown as its (head-truncated) line — the 1 entry
// = 1 line accounting still holds, so start_line just decrements by the
// prepended count. No auto-trim on backward paging (contract v2).
const loadingEarlier = ref(false)
const canLoadEarlier = computed(
  () => !isActive.value && !following.value && firstOffset.value > 0 && trimmedLines.value === 0,
)

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
  } catch {
    /* ignore */
  } finally {
    loadingEarlier.value = false
  }
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
    } catch {
      /* ignore parse errors */
    }
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
      const page = await tasksApi.log(props.taskId, {
        offset: from,
        maxBytes: DEFAULT_LOG_MAX_BYTES,
      })
      if (endOffset.value === from) acceptPage(page)
    } catch {
      /* backend still down — the reopened stream will error again */
    }
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
        offset: from,
        maxBytes: DEFAULT_LOG_MAX_BYTES,
      })
      // If something already advanced the cursor meanwhile, that path
      // owns the buffer now — this resync is settled.
      ok = endOffset.value !== from || acceptPage(page)
    } catch {
      ok = false
    }
    if (!ok) {
      try {
        await reloadTail()
      } catch {
        // Backend unreachable. If we were following, the intent survives:
        // hand recovery to the infinite reconnect loop instead of giving
        // up (tail -f semantics). Outside follow, surface it once.
        if (streamState.value === 'following') streamState.value = 'reconnecting'
        else if (streamState.value !== 'reconnecting') snack.warn(t('log.stream_lost'))
        return
      }
    }
    if (restartStream && streamState.value === 'following') startFollow()
  } finally {
    resyncing = false
  }
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
    if (reconnectTimer) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
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
          offset: tailFrom,
          maxBytes: DEFAULT_LOG_MAX_BYTES,
        })
        // Generation guard: if SSE advanced the offset while this request
        // was in flight, the same lines already rendered — drop the page.
        if (endOffset.value !== tailFrom) return
        if (!acceptPage(page)) void resync()
      } catch {
        /* best effort */
      }
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
    if (config.killAsync) snack.info(t('task.cancel_requested'))
    else snack.success(t('task.killed_ok'))
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  }
}

// RQ-75: a rerun of a task whose target config changed since submission
// goes through the confirmation dialog (or straight through, if the user
// checked "don't ask again").
const genRerun = useGenerationRerun(
  (p) => taskActions.retry.mutateAsync(p),
  () => snack.success(t('task.retried')),
  (e: any) => snack.error(e?.message || t('common.error')),
)

async function retryTask() {
  if (retrying.value) return
  await genRerun.run(props.taskId)
}

// RQ-74: the submit-evidence card's tone follows the task status — red only
// when the verdict is settled (rejected/failed), amber for live states
// (retrying, outcome unknown).
const failureCard = computed(() => {
  switch (task.value?.status) {
    case 'pending':
      return { color: 'warning', icon: 'mdi-autorenew', title: t('task.submit_retrying') }
    case 'unknown':
      return { color: 'warning', icon: 'mdi-help-circle-outline', title: t('task.outcome_unknown') }
    default:
      return {
        color: 'error',
        icon: 'mdi-alert-circle-outline',
        title: t('task.failed_before_running'),
      }
  }
})

// RQ-74: copy the pre-run failure evidence (scheduler stderr + rendered
// command) so the user can paste it into a ticket / terminal search.
const detailCopied = ref(false)
function copyFailureDetail() {
  if (!task.value?.failure_detail) return
  navigator.clipboard
    .writeText(task.value.failure_detail)
    .then(() => {
      detailCopied.value = true
      setTimeout(() => {
        detailCopied.value = false
      }, 2000)
    })
    .catch(() => snack.error(t('common.error')))
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
  fetchJobContext() // fire-and-forget — best effort, doesn't block render
  await waitForTask()
  if (notFound.value) return
  logLoading.value = true
  try {
    await reloadTail()
  } catch {
    /* ignore */
  } finally {
    logLoading.value = false
  }
  streamState.value = 'ready' // GET settled: offsets are now trustworthy
  // Query cache may already have the task (navigated from the job page);
  // the immediate isActive value seeds follow, the watch handles flips.
  if (isActive.value) following.value = true
  nextTick(() => scrollToBottom())
})

onUnmounted(() => {
  stopFollow()
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
  if (activityTimer) {
    clearInterval(activityTimer)
    activityTimer = null
  }
})
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.cursor-pointer { cursor: pointer; }

/* Run tab: chart takes the room, the identity column stays readable. */
.run-grid {
  display: grid;
  grid-template-columns: minmax(320px, 1.5fr) minmax(260px, 320px);
  gap: 12px;
  align-items: start;
}
@media (max-width: 959px) {
  .run-grid { grid-template-columns: 1fr; }
}

.param-list { display: grid; gap: 2px; }
.param-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 6px;
  border-radius: 4px;
  font-size: 13px;
}
.param-swept { background: rgb(var(--v-theme-secondary), 0.1); }
.param-key { flex: 1; color: rgb(var(--v-theme-on-surface-variant)); }
.param-val { word-break: break-all; }
.legend-box { width: 8px; height: 8px; border-radius: 2px; display: inline-block; }
.legend-swept { background: rgb(var(--v-theme-secondary), 0.4); }
.legend-fixed { border: 1px solid rgb(var(--v-theme-outline-variant)); }

.kv-row {
  display: flex;
  gap: 10px;
  padding: 3px 0;
  font-size: 13px;
}
.kv-row code { word-break: break-all; }
.kv-key {
  width: 100px;
  flex-shrink: 0;
  color: rgb(var(--v-theme-on-surface-variant));
  font-size: 12px;
}

/* RQ-74: verbatim scheduler output — monospace, wrapped, no reflow of the
   scheduler's own formatting. */
.failure-detail {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  white-space: pre-wrap;
  word-break: break-word;
  margin: 0;
  max-height: 260px;
  overflow-y: auto;
}
</style>
