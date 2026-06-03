<template>
  <div v-if="store.detail">
    <!-- ====== Summary card ====== -->
    <v-card class="mb-6 pa-5 summary-card">
      <div class="d-flex align-center justify-space-between mb-4">
        <div class="d-flex align-center ga-3">
          <StatusBadge :status="store.detail.job.status" />
          <span v-if="store.detail.job.note" class="text-body-2 text-on-surface-variant">
            {{ store.detail.job.note }}
          </span>
          <span class="text-caption text-on-surface-variant">
            {{ relativeTime(store.detail.job.created_at) }}
          </span>
        </div>
        <div class="d-flex ga-1">
          <v-btn
            v-if="config.features.pause_resume && isActive"
            size="small"
            variant="tonal"
            :color="store.detail.job.status === 'paused' ? 'success' : 'warning'"
            @click="togglePause"
          >
            <v-icon start size="16">{{ store.detail.job.status === 'paused' ? 'mdi-play' : 'mdi-pause' }}</v-icon>
            {{ store.detail.job.status === 'paused' ? t('job.resume') : t('job.pause') }}
          </v-btn>
          <v-btn v-if="isActive" size="small" variant="tonal" color="error" @click="killJob">
            <v-icon start size="16">mdi-stop</v-icon>
            {{ t('job.kill') }}
          </v-btn>
        </div>
      </div>

      <!-- Quick stats row -->
      <div class="d-flex flex-wrap ga-4 align-center">
        <div class="text-center stat-block">
          <div class="text-h5 font-weight-bold text-success">{{ doneTasks.length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('common.done') }}</div>
        </div>
        <div class="text-center stat-block">
          <div class="text-h5 font-weight-bold" :class="store.detail.job.status === 'running' ? 'text-warning pulse-dot' : ''">
            {{ store.detail.tasks.filter(t => t.status === 'running').length }}
          </div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.running') }}</div>
        </div>
        <div class="text-center stat-block">
          <div class="text-h5 font-weight-bold text-error">{{ failedTasks.length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.failed') }}</div>
        </div>
        <div class="text-center stat-block">
          <div class="text-h5 font-weight-bold text-info">{{ store.detail.tasks.filter(t => t.status === 'pending').length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.pending') }}</div>
        </div>

        <v-spacer />

        <!-- Inline top-3 leaderboard -->
        <div
          v-if="topRuns.length > 0"
          class="pa-3 rounded-xl top-runs-card"
          style="background: rgba(var(--v-theme-success), 0.08)"
        >
          <div class="text-caption text-on-surface-variant mb-1">Top 3 · {{ selectedMetric }}</div>
          <div class="d-flex flex-wrap ga-3">
            <div v-for="(run, idx) in topRuns" :key="run.task_id" class="d-flex align-center ga-1">
              <v-icon v-if="idx === 0" size="14" color="warning">mdi-trophy</v-icon>
              <v-icon v-else-if="idx < 3" size="14" color="on-surface-variant">mdi-medal</v-icon>
              <span class="text-body-2 font-weight-bold" :class="idx === 0 ? 'text-success' : ''">
                {{ run.best?.toPrecision(4) }}
              </span>
              <code class="text-caption text-on-surface-variant">{{ compactParams(run.params) }}</code>
            </div>
          </div>
        </div>
      </div>

      <!-- Failed task hints -->
      <div v-if="failedTasks.length > 0" class="mt-3 pa-3 rounded-xl" style="background: rgba(var(--v-theme-error), 0.06)">
        <div class="text-caption font-weight-medium text-error mb-1">
          {{ failedTasks.length }} {{ t('overview.failed').toLowerCase() }}
        </div>
        <div v-for="ft in failedTasks.slice(0, 3)" :key="ft.id" class="d-flex align-center ga-2 text-caption">
          <code>{{ ft.id.slice(0, 8) }}</code>
          <span class="text-on-surface-variant">exit {{ ft.exit_code ?? '?' }}</span>
          <span class="text-on-surface-variant">{{ compactParams(ft.params) }}</span>
        </div>
        <div v-if="failedTasks.length > 3" class="text-caption text-on-surface-variant mt-1">
          +{{ failedTasks.length - 3 }} more
        </div>
      </div>

      <!-- Progress bar -->
      <v-progress-linear
        :model-value="progressPercent"
        color="success"
        height="6"
        rounded
        class="mt-4"
      />
    </v-card>

    <!-- ====== Tabs ====== -->
    <v-tabs v-model="tab" class="mb-4">
      <v-tab value="tasks">
        {{ t('job.tasks') }}
        <v-chip size="x-small" variant="tonal" class="ml-2">{{ store.detail.tasks.length }}</v-chip>
      </v-tab>
      <v-tab v-if="store.detail.wandb" value="wandb">
        <v-icon start size="16">mdi-chart-scatter-plot</v-icon>
        {{ t('job.wandb') }}
      </v-tab>
    </v-tabs>

    <v-tabs-window v-model="tab">
      <!-- ====== Tasks tab (with integrated metric sort) ====== -->
      <v-tabs-window-item value="tasks">
        <div class="d-flex align-center ga-2 mb-3 flex-wrap">
          <v-chip-group v-model="statusFilter">
            <v-chip filter value="">{{ t('common.all') }}</v-chip>
            <v-chip filter value="running" color="warning">{{ t('job.status.running') }}</v-chip>
            <v-chip filter value="pending">{{ t('job.status.pending') }}</v-chip>
            <v-chip filter value="failed" color="error">{{ t('job.status.failed') }}</v-chip>
            <v-chip filter value="done">{{ t('job.status.done') }}</v-chip>
          </v-chip-group>

          <v-spacer />

          <!-- Metric sort (replaces Compare tab) -->
          <v-select
            v-if="store.detail.metric_keys.length > 0"
            v-model="selectedMetric"
            :items="metricSortOptions"
            :label="t('job.sort_by')"
            hide-details
            density="compact"
            variant="outlined"
            clearable
            style="max-width: 200px"
          />
          <v-btn-toggle
            v-if="selectedMetric"
            v-model="compareDesc"
            mandatory
            density="compact"
            variant="outlined"
          >
            <v-btn :value="true" size="small">
              <v-icon size="16">mdi-sort-descending</v-icon>
            </v-btn>
            <v-btn :value="false" size="small">
              <v-icon size="16">mdi-sort-ascending</v-icon>
            </v-btn>
          </v-btn-toggle>
        </div>

        <v-card>
          <v-data-table
            :headers="currentHeaders"
            :items="displayedTasks"
            item-value="id"
          >
            <template #item.rank="{ value, index }">
              <div class="d-flex align-center ga-1">
                <v-icon v-if="index === 0 && selectedMetric" size="16" color="warning">mdi-trophy</v-icon>
                <v-icon v-else-if="index < 3 && selectedMetric" size="16" color="on-surface-variant">mdi-medal</v-icon>
                <span :class="{ 'font-weight-bold': index < 3 && selectedMetric }">{{ value ?? index + 1 }}</span>
              </div>
            </template>
            <template #item.id="{ value }">
              <code>{{ value.slice(0, 8) }}</code>
            </template>
            <template #item.task_id="{ value }">
              <code>{{ value.slice(0, 8) }}</code>
            </template>
            <template #item.status="{ value }">
              <StatusBadge :status="value" />
            </template>
            <template #item.params="{ value }">
              <code class="text-caption">{{ compactParams(value) }}</code>
            </template>
            <template #item.best="{ value }">
              <span class="font-weight-medium">{{ typeof value === 'number' ? value.toPrecision(4) : '-' }}</span>
            </template>
            <template #item.elapsed_seconds="{ value }">
              <span class="text-caption">{{ value ? formatDuration(value) : '-' }}</span>
            </template>
            <template #item.wandb_run_id="{ value }">
              <v-btn
                v-if="value"
                size="x-small"
                variant="text"
                icon
                :href="wandbRunURL(value)"
                target="_blank"
              >
                <v-icon size="14">mdi-open-in-new</v-icon>
              </v-btn>
            </template>
            <!-- actions slot: only rendered for task rows, not compare rows -->
            <template #item.actions="{ item }">
              <div v-if="!isCompareMode" class="d-flex ga-1">
                <v-btn
                  v-if="item.status === 'running'"
                  icon size="x-small" variant="text" color="error"
                  @click.stop="store.killTask(item.id).then(() => refresh())"
                >
                  <v-icon size="14">mdi-stop</v-icon>
                </v-btn>
                <v-btn
                  v-if="item.status === 'failed'"
                  icon size="x-small" variant="text" color="primary"
                  @click.stop="store.retryTask(item.id).then(() => refresh())"
                >
                  <v-icon size="14">mdi-refresh</v-icon>
                </v-btn>
              </div>
            </template>
          </v-data-table>
        </v-card>
      </v-tabs-window-item>

      <!-- ====== W&B iframe tab ====== -->
      <v-tabs-window-item v-if="store.detail.wandb" value="wandb">
        <v-card class="pa-0">
          <!-- Header bar -->
          <div class="d-flex align-center ga-2 pa-3" style="border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity))">
            <v-icon size="18" color="on-surface-variant">mdi-chart-scatter-plot</v-icon>
            <span class="text-body-2 font-weight-medium">
              {{ t('job.wandb_project') }}: <code>{{ wandbLabel }}</code>
            </span>
            <v-spacer />
            <v-btn
              size="small"
              variant="tonal"
              color="primary"
              :href="store.detail.wandb.base_url"
              target="_blank"
            >
              <v-icon start size="16">mdi-open-in-new</v-icon>
              {{ t('job.wandb_open') }}
            </v-btn>
            <v-btn
              size="small"
              variant="text"
              icon
              @click="wandbKey++"
            >
              <v-icon size="18">mdi-refresh</v-icon>
            </v-btn>
          </div>

          <!-- Iframe -->
          <div class="wandb-iframe-wrap">
            <iframe
              v-if="!wandbError"
              :key="wandbKey"
              :src="store.detail.wandb.base_url"
              class="wandb-iframe"
              frameborder="0"
              allow="clipboard-write"
              referrerpolicy="no-referrer"
              @error="wandbError = true"
            />
            <!-- Fallback if iframe fails -->
            <div v-if="wandbError" class="d-flex flex-column align-center justify-center pa-12 text-center" style="min-height: 400px">
              <v-icon size="48" color="on-surface-variant" style="opacity: 0.3" class="mb-4">mdi-chart-scatter-plot</v-icon>
              <div class="text-body-2 text-on-surface-variant mb-4">{{ t('job.wandb_fallback') }}</div>
              <v-btn
                variant="tonal"
                color="primary"
                :href="store.detail.wandb.base_url"
                target="_blank"
              >
                <v-icon start size="16">mdi-open-in-new</v-icon>
                {{ t('job.wandb_open') }}
              </v-btn>
            </div>
          </div>
        </v-card>
      </v-tabs-window-item>
    </v-tabs-window>
  </div>

  <!-- Loading -->
  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<!-- eslint-disable -->
<script setup lang="ts">
// @ts-nocheck — displayedTasks union (TaskView | CompareRow) causes template type errors in item slots
import { ref, computed, watch, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useJobDetailStore } from '@/stores/jobDetail'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { usePolling } from '@/composables/usePolling'
import { useSnackbar } from '@/composables/useSnackbar'
import StatusBadge from '@/components/StatusBadge.vue'
import type { CompareRow } from '@/api/types'

const props = defineProps<{ project: string; jobId: string }>()
const { t } = useI18n()
const store = useJobDetailStore()
const config = useConfigStore()
const prefs = usePreferences()
const snack = useSnackbar()

const tab = ref('tasks')
const statusFilter = ref(prefs.lastStatusFilter.value)
const selectedMetric = ref('')
const compareDesc = ref(prefs.compareSortDesc.value)
const wandbError = ref(false)
const wandbKey = ref(0)

const isActive = computed(() => {
  const s = store.detail?.job.status
  return s === 'running' || s === 'pending' || s === 'paused'
})

const failedTasks = computed(() =>
  store.detail?.tasks.filter(t => t.status === 'failed') ?? []
)

const doneTasks = computed(() =>
  store.detail?.tasks.filter(t => isDoneStatus(t.status)) ?? []
)

const progressPercent = computed(() => {
  if (!store.detail) return 0
  const total = store.detail.tasks.length
  if (total === 0) return 0
  return (doneTasks.value.length / total) * 100
})

const metricSortOptions = computed(() =>
  store.detail?.metric_keys ?? []
)

// Top 3 runs for the inline leaderboard
const topRuns = computed(() => store.compare.slice(0, 3))

// W&B label for display
const wandbLabel = computed(() => {
  const w = store.detail?.wandb
  if (!w) return ''
  return w.entity ? `${w.entity}/${w.project}` : w.project ?? ''
})

// Build correct W&B run URL
function wandbRunURL(runId: string): string {
  const w = store.detail?.wandb
  if (w?.base_url) return `${w.base_url}/runs/${runId}`
  return `https://wandb.ai/runs/${runId}`
}

// ---- Table logic: unified tasks + compare ----

// When no metric selected: show task list. When metric selected: show compare ranking.
const isCompareMode = computed(() => !!selectedMetric.value)

const filteredTasks = computed(() => {
  if (!store.detail) return []
  if (!statusFilter.value) return store.detail.tasks
  if (statusFilter.value === 'done') return store.detail.tasks.filter(t => isDoneStatus(t.status))
  return store.detail.tasks.filter(t => t.status === statusFilter.value)
})

const displayedTasks = computed(() => {
  if (isCompareMode.value) return store.compare
  return filteredTasks.value
})

const taskHeaders = [
  { title: 'ID', key: 'id', width: '100px' },
  { title: 'Status', key: 'status', width: '90px' },
  { title: t('job.params'), key: 'params', sortable: false },
  { title: t('job.step'), key: 'current_step', width: '70px' },
  { title: t('job.elapsed'), key: 'elapsed_seconds', width: '100px' },
  { title: 'W&B', key: 'wandb_run_id', width: '50px', sortable: false },
  { title: '', key: 'actions', sortable: false, width: '80px' },
]

const compareHeaders = computed(() => [
  { title: '#', key: 'rank', width: '60px' },
  { title: 'Task', key: 'task_id', width: '100px' },
  { title: t('job.params'), key: 'params', sortable: false },
  { title: selectedMetric.value || t('job.best'), key: 'best', width: '120px' },
])

const currentHeaders = computed(() =>
  isCompareMode.value ? compareHeaders.value : taskHeaders
)

// ---- Watchers ----

// Auto-select first metric key
watch(() => store.detail?.metric_keys, (keys) => {
  if (keys && keys.length > 0 && !selectedMetric.value) {
    const preferred = prefs.preferredMetrics.value[props.jobId]
    selectedMetric.value = preferred && keys.includes(preferred) ? preferred : keys[0]
  }
}, { immediate: true })

// Fetch compare when metric or sort changes
watch([selectedMetric, compareDesc], ([key, desc]) => {
  if (key) {
    prefs.setPreferredMetric(props.jobId, key)
    prefs.compareSortDesc.value = desc
    store.fetchCompare(props.jobId, key, desc)
  }
})

// Save status filter preference
watch(statusFilter, (v) => { prefs.lastStatusFilter.value = v })

function refresh(silent = false) { store.fetchDetail(props.jobId, silent) }

function togglePause() {
  if (!store.detail) return
  const p = store.detail.job.status === 'paused'
    ? store.resumeJob(props.jobId)
    : store.pauseJob(props.jobId)
  p.then(() => {
    snack.success(t('common.done'))
    refresh()
  })
}

function killJob() {
  store.killJob(props.jobId).then(() => {
    snack.success(t('job.killed'))
    refresh()
  })
}

function compactParams(params: Record<string, any>): string {
  const entries = Object.entries(params)
  if (entries.length === 0) return '{}'
  const parts = entries.slice(0, 3).map(([k, v]) => `${k}=${v}`)
  if (entries.length > 3) parts.push('...')
  return parts.join(', ')
}

function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}

function isDoneStatus(status: string): boolean {
  return status === 'success' || status === 'completed' || status === 'done'
}

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// Polling (3s when active; also fires initial fetch on mount)
usePolling(refresh, 3000, isActive)
onUnmounted(() => { store.$reset() })
</script>

<style scoped>
/* Summary card entrance */
.summary-card {
  animation: card-in 0.3s ease-out;
}

@keyframes card-in {
  from { opacity: 0; transform: translateY(8px); }
  to   { opacity: 1; transform: translateY(0); }
}

/* Stat numbers subtle pop */
.stat-block {
  animation: stat-pop 0.4s ease-out both;
}
.stat-block:nth-child(1) { animation-delay: 0.05s; }
.stat-block:nth-child(2) { animation-delay: 0.10s; }
.stat-block:nth-child(3) { animation-delay: 0.15s; }
.stat-block:nth-child(4) { animation-delay: 0.20s; }

@keyframes stat-pop {
  from { opacity: 0; transform: scale(0.85); }
  to   { opacity: 1; transform: scale(1); }
}

/* Top-3 card slide-in from right */
.top-runs-card {
  animation: slide-right 0.35s ease-out 0.25s both;
}

@keyframes slide-right {
  from { opacity: 0; transform: translateX(12px); }
  to   { opacity: 1; transform: translateX(0); }
}

/* Running pulse */
.pulse-dot {
  animation: pulse 1.5s ease-in-out infinite;
}

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}

/* Progress bar shimmer on active jobs */
.v-progress-linear--active .v-progress-linear__determinate {
  background-image: linear-gradient(
    90deg,
    transparent 0%,
    rgba(255, 255, 255, 0.15) 50%,
    transparent 100%
  );
  background-size: 200% 100%;
  animation: shimmer 2s linear infinite;
}

@keyframes shimmer {
  from { background-position: 200% 0; }
  to   { background-position: -200% 0; }
}

/* W&B iframe */
.wandb-iframe-wrap {
  position: relative;
  min-height: 500px;
}

.wandb-iframe {
  width: 100%;
  height: 600px;
  border: none;
  animation: fade-in 0.5s ease-out;
}

@keyframes fade-in {
  from { opacity: 0; }
  to   { opacity: 1; }
}
</style>
