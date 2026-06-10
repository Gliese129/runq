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
          <v-btn v-if="displayStatus === 'running'" size="x-small" variant="tonal" color="error" @click="killTask">
            <v-icon start size="14">mdi-stop</v-icon> Kill
          </v-btn>
          <v-btn v-if="task.status === 'failed'" size="x-small" variant="tonal" color="primary" @click="retryTask">
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
    </v-card>

    <!-- Params -->
    <v-card class="mb-4 pa-4">
      <div class="text-subtitle-2 mb-2">Parameters</div>
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
    </v-card>

    <!-- Metrics chart -->
    <v-card v-if="task.status !== 'pending'" class="mb-4 pa-4">
      <div class="d-flex align-center justify-space-between mb-2">
        <div class="text-subtitle-2 d-flex align-center ga-1">
          <v-icon size="16">mdi-chart-line</v-icon> Metrics
        </div>
        <v-btn v-if="task.wandb_run_id" size="x-small" variant="text" :href="`https://wandb.ai/runs/${task.wandb_run_id}`" target="_blank">
          <v-icon start size="14">mdi-open-in-new</v-icon> W&B
        </v-btn>
      </div>
      <MetricsChart :points="metricPoints" />
    </v-card>

    <!-- Log viewer -->
    <v-card class="pa-0">
      <div class="d-flex align-center justify-space-between pa-3" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))">
        <div class="d-flex align-center ga-2">
          <div class="text-subtitle-2">Log</div>
          <v-chip size="x-small" variant="tonal">{{ logData.total_lines }} lines</v-chip>
        </div>
        <div class="d-flex align-center ga-1">
          <v-btn v-if="logData.start > 0" size="x-small" variant="text" :loading="loadingMore" @click="loadEarlier">
            <v-icon start size="12">mdi-arrow-up</v-icon> Load earlier
          </v-btn>
          <v-switch
            v-model="autoScroll"
            density="compact" hide-details color="primary" inline
            label="Follow"
          />
        </div>
      </div>

      <div ref="logContainer" class="terminal-block" style="max-height: 600px; overflow-y: auto">
        <div v-if="logData.lines.length === 0 && !logLoading" class="text-center pa-4" style="color: #64748B">
          {{ task.status === 'pending' ? 'Waiting to start...' : 'No log output yet' }}
        </div>
        <div v-for="(line, i) in logData.lines" :key="logData.start + i" class="log-line">
          <span class="log-lineno">{{ logData.start + i + 1 }}</span>{{ line }}
        </div>
      </div>
    </v-card>
  </div>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { tasksApi } from '@/apis/tasks'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfigStore } from '@/stores/config'
import MetricsChart from '@/components/MetricsChart.vue'
import TaskStatusBadge from '@/components/TaskStatusBadge.vue'
import type { TaskLogResponse, TaskView } from '@/types/api'

const props = defineProps<{ project: string; jobId: string; taskId: string }>()
const snack = useSnackbar()
const config = useConfigStore()

const task = ref<TaskView | null>(null)

// Kill in flight (kill_async backends): frontend-local transient state,
// cleared as soon as a fetch shows the task left "running".
const cancelPending = ref(false)
const displayStatus = computed(() =>
  cancelPending.value && task.value?.status === 'running' ? 'cancelling' : task.value?.status ?? '',
)
watch(() => task.value?.status, (s) => {
  if (s && s !== 'running') cancelPending.value = false
})
const logData = ref<TaskLogResponse>({
  lines: [], total_lines: 0, start: 0, end: 0,
})
const metricPoints = ref<any[]>([])
const logLoading = ref(false)
const loadingMore = ref(false)
const autoScroll = ref(true)
const logContainer = ref<HTMLElement>()

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

// ── Fetch log ──
async function fetchLog(tail = 500, offset = 0) {
  logLoading.value = true
  try {
    const res = await tasksApi.log(props.taskId, { tail, offset })
    logData.value = res
  } catch { /* ignore */ }
  finally { logLoading.value = false }
}

async function loadEarlier() {
  if (logData.value.start <= 0) return
  loadingMore.value = true
  try {
    const newOffset = logData.value.total_lines - logData.value.start
    const res = await tasksApi.log(props.taskId, { tail: 200, offset: newOffset })
    // Prepend earlier lines
    logData.value.lines = [...res.lines, ...logData.value.lines]
    logData.value.start = res.start
  } catch { /* ignore */ }
  finally { loadingMore.value = false }
}

// ── Polling for active tasks ──
const isActive = computed(() => task.value && ['running', 'pending'].includes(task.value.status))
let pollTimer: ReturnType<typeof setInterval> | null = null

function startPolling() {
  stopPolling()
  pollTimer = setInterval(async () => {
    await fetchTask()
    const [res] = await Promise.all([
      tasksApi.log(props.taskId, { tail: 500, offset: 0 }),
      fetchMetrics(),
    ])
    logData.value = res
    if (autoScroll.value) {
      nextTick(() => {
        const el = logContainer.value
        if (el) el.scrollTop = el.scrollHeight
      })
    }
  }, 3000)
}

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
}

watch(isActive, (active) => {
  if (active) startPolling()
  else stopPolling()
})

// ── Actions ──
async function killTask() {
  try {
    await tasksApi.kill(props.taskId)
    if (config.killAsync) {
      // The cancel was forwarded to the cluster; honesty: don't claim
      // "killed" until a poll confirms it.
      cancelPending.value = true
      snack.info('Cancel requested')
    } else {
      snack.success('Task killed')
    }
    fetchTask()
  } catch (e: any) { snack.error(e?.message || 'Kill failed') }
}

async function retryTask() {
  try {
    await tasksApi.retry(props.taskId)
    snack.success('Task retried')
    fetchTask()
  } catch (e: any) { snack.error(e?.message || 'Retry failed') }
}

function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}

onMounted(async () => {
  await fetchTask()
  await Promise.all([fetchLog(), fetchMetrics()])
  if (isActive.value) startPolling()
  nextTick(() => {
    const el = logContainer.value
    if (el) el.scrollTop = el.scrollHeight
  })
})

onUnmounted(() => stopPolling())
</script>

<style scoped>
.log-line {
  white-space: pre-wrap;
  word-break: break-all;
  padding: 0 16px 0 0;
  line-height: 1.5;
}
.log-lineno {
  display: inline-block;
  width: 48px;
  text-align: right;
  padding-right: 12px;
  color: #64748B;
  user-select: none;
  flex-shrink: 0;
}
</style>
