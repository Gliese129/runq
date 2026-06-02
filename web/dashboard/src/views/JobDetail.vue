<template>
  <div v-if="store.detail">
    <!-- ====== Summary card ====== -->
    <v-card class="mb-6 pa-5">
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
      <div class="d-flex flex-wrap ga-4">
        <div class="text-center">
          <div class="text-h5 font-weight-bold text-success">{{ store.detail.tasks.filter(t => t.status === 'completed' || t.status === 'done').length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('common.done') }}</div>
        </div>
        <div class="text-center">
          <div class="text-h5 font-weight-bold" :class="store.detail.job.status === 'running' ? 'text-warning pulse-dot' : ''">
            {{ store.detail.tasks.filter(t => t.status === 'running').length }}
          </div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.running') }}</div>
        </div>
        <div class="text-center">
          <div class="text-h5 font-weight-bold text-error">{{ failedTasks.length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.failed') }}</div>
        </div>
        <div class="text-center">
          <div class="text-h5 font-weight-bold text-info">{{ store.detail.tasks.filter(t => t.status === 'pending').length }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('overview.pending') }}</div>
        </div>

        <v-spacer />

        <!-- Best result (auto-selected metric) -->
        <div v-if="bestRun" class="text-right pa-3 rounded-xl" style="background: rgba(var(--v-theme-success), 0.08)">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('job.best') }} · {{ selectedMetric }}</div>
          <div class="d-flex align-center ga-2">
            <v-icon size="16" color="success">mdi-trophy</v-icon>
            <span class="text-h6 font-weight-bold text-success">{{ bestRun.best?.toPrecision(4) }}</span>
            <code class="text-caption text-on-surface-variant">{{ bestRun.task_id.slice(0, 8) }}</code>
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
      <v-tab value="compare">{{ t('job.compare') }}</v-tab>
      <v-tab value="matrix">{{ t('job.matrix') }}</v-tab>
    </v-tabs>

    <v-tabs-window v-model="tab">
      <!-- ====== Tasks tab ====== -->
      <v-tabs-window-item value="tasks">
        <div class="d-flex align-center ga-2 mb-3">
          <v-chip-group v-model="statusFilter">
            <v-chip filter value="">{{ t('common.all') }}</v-chip>
            <v-chip filter value="running" color="warning">{{ t('job.status.running') }}</v-chip>
            <v-chip filter value="pending">{{ t('job.status.pending') }}</v-chip>
            <v-chip filter value="failed" color="error">{{ t('job.status.failed') }}</v-chip>
            <v-chip filter value="done">{{ t('job.status.done') }}</v-chip>
          </v-chip-group>
        </div>

        <v-card>
          <v-data-table
            :headers="taskHeaders"
            :items="filteredTasks"
            item-value="id"
          >
            <template #item.id="{ value }">
              <code>{{ value.slice(0, 8) }}</code>
            </template>
            <template #item.status="{ value }">
              <StatusBadge :status="value" />
            </template>
            <template #item.params="{ value }">
              <code class="text-caption">{{ compactParams(value) }}</code>
            </template>
            <template #item.elapsed_seconds="{ value }">
              <span class="text-caption">{{ value ? formatDuration(value) : '—' }}</span>
            </template>
            <template #item.wandb_run_id="{ value }">
              <v-btn
                v-if="value"
                size="x-small"
                variant="text"
                icon
                :href="`https://wandb.ai/run/${value}`"
                target="_blank"
              >
                <v-icon size="14">mdi-open-in-new</v-icon>
              </v-btn>
            </template>
            <template #item.actions="{ item }">
              <div class="d-flex ga-1">
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

      <!-- ====== Compare tab ====== -->
      <v-tabs-window-item value="compare">
        <div class="d-flex align-center ga-3 mb-3">
          <v-select
            v-model="selectedMetric"
            :items="store.detail.metric_keys"
            :label="t('job.metric')"
            hide-details
            style="max-width: 240px"
          />
          <v-btn-toggle v-model="compareDesc" mandatory density="compact" variant="outlined">
            <v-btn :value="true" size="small">
              <v-icon size="16">mdi-sort-descending</v-icon>
            </v-btn>
            <v-btn :value="false" size="small">
              <v-icon size="16">mdi-sort-ascending</v-icon>
            </v-btn>
          </v-btn-toggle>
        </div>

        <v-card v-if="store.compare.length > 0">
          <v-data-table
            :headers="compareHeaders"
            :items="store.compare"
            item-value="task_id"
          >
            <template #item.rank="{ value, index }">
              <div class="d-flex align-center ga-1">
                <v-icon v-if="index === 0" size="16" color="warning">mdi-trophy</v-icon>
                <v-icon v-else-if="index < 3" size="16" color="on-surface-variant">mdi-medal</v-icon>
                <span :class="{ 'font-weight-bold': index < 3 }">{{ value }}</span>
              </div>
            </template>
            <template #item.task_id="{ value }">
              <code>{{ value.slice(0, 8) }}</code>
            </template>
            <template #item.params="{ value }">
              <code class="text-caption">{{ compactParams(value) }}</code>
            </template>
            <template #item.best="{ value }">
              <span class="font-weight-medium">{{ typeof value === 'number' ? value.toPrecision(4) : '—' }}</span>
            </template>
          </v-data-table>
        </v-card>

        <v-card v-else-if="!selectedMetric" class="pa-8 text-center">
          <v-icon size="32" color="on-surface-variant" class="mb-2" style="opacity: 0.4">mdi-chart-bar</v-icon>
          <div class="text-body-2 text-on-surface-variant">{{ t('job.select_metric') }}</div>
        </v-card>
      </v-tabs-window-item>

      <!-- ====== Matrix tab ====== -->
      <v-tabs-window-item value="matrix">
        <div class="d-flex align-center ga-3 mb-3">
          <v-select
            v-model="matrixRow"
            :items="paramKeys"
            :label="t('job.row_param')"
            hide-details
            style="max-width: 200px"
          />
          <v-select
            v-model="matrixCol"
            :items="paramKeys"
            :label="t('job.col_param')"
            hide-details
            style="max-width: 200px"
          />
          <v-select
            v-model="matrixValue"
            :items="store.detail.metric_keys"
            :label="t('job.value_metric')"
            hide-details
            style="max-width: 200px"
          />
        </div>
        <v-card v-if="store.matrix" class="pa-4 overflow-x-auto">
          <table class="matrix-table">
            <thead>
              <tr>
                <th></th>
                <th v-for="col in store.matrix.cols" :key="col" class="text-caption pa-2 text-center">{{ col }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(row, ri) in store.matrix.rows" :key="row">
                <td class="text-caption font-weight-medium pa-2">{{ row }}</td>
                <td
                  v-for="(col, ci) in store.matrix.cols"
                  :key="col"
                  class="text-caption text-center pa-2 rounded"
                  :style="heatStyle(store.matrix.cells[ri][ci])"
                >
                  {{ store.matrix.cells[ri][ci] != null ? (store.matrix.cells[ri][ci] as number).toPrecision(3) : '—' }}
                </td>
              </tr>
            </tbody>
          </table>
        </v-card>
        <v-card v-else class="pa-8 text-center">
          <v-icon size="32" color="on-surface-variant" class="mb-2" style="opacity: 0.4">mdi-grid</v-icon>
          <div class="text-body-2 text-on-surface-variant">{{ t('job.select_matrix') }}</div>
        </v-card>
      </v-tabs-window-item>
    </v-tabs-window>
  </div>

  <!-- Loading -->
  <div v-else-if="store.loading" class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useJobDetailStore } from '@/stores/jobDetail'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { usePolling } from '@/composables/usePolling'
import { useSnackbar } from '@/composables/useSnackbar'
import StatusBadge from '@/components/StatusBadge.vue'
import type { TaskView, CompareRow } from '@/api/types'

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
const matrixRow = ref('')
const matrixCol = ref('')
const matrixValue = ref('')

const isActive = computed(() => {
  const s = store.detail?.job.status
  return s === 'running' || s === 'pending' || s === 'paused'
})

const failedTasks = computed(() =>
  store.detail?.tasks.filter(t => t.status === 'failed') ?? []
)

const progressPercent = computed(() => {
  if (!store.detail) return 0
  const total = store.detail.tasks.length
  if (total === 0) return 0
  const done = store.detail.tasks.filter(t => t.status === 'completed' || t.status === 'done').length
  return (done / total) * 100
})

const bestRun = ref<CompareRow | null>(null)

const filteredTasks = computed(() => {
  if (!store.detail) return []
  if (!statusFilter.value) return store.detail.tasks
  return store.detail.tasks.filter(t => t.status === statusFilter.value)
})

const paramKeys = computed(() => {
  if (!store.detail || store.detail.tasks.length === 0) return []
  return Object.keys(store.detail.tasks[0].params)
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

const compareHeaders = [
  { title: '#', key: 'rank', width: '60px' },
  { title: 'Task', key: 'task_id', width: '100px' },
  { title: t('job.params'), key: 'params', sortable: false },
  { title: t('job.best'), key: 'best', width: '120px' },
]

// Auto-select first metric key
watch(() => store.detail?.metric_keys, (keys) => {
  if (keys && keys.length > 0 && !selectedMetric.value) {
    // Use preferred metric or first available
    const preferred = prefs.preferredMetrics.value[props.jobId]
    selectedMetric.value = preferred && keys.includes(preferred) ? preferred : keys[0]
  }
}, { immediate: true })

// Fetch compare on metric change
watch([selectedMetric, compareDesc], ([key, desc]) => {
  if (key) {
    prefs.setPreferredMetric(props.jobId, key)
    prefs.compareSortDesc.value = desc
    store.fetchCompare(props.jobId, key, desc).then(() => {
      if (store.compare.length > 0) bestRun.value = store.compare[0]
    })
  }
})

// Save status filter preference
watch(statusFilter, (v) => { prefs.lastStatusFilter.value = v })

// Fetch matrix
watch([matrixRow, matrixCol, matrixValue], ([row, col, val]) => {
  if (row && col && val) store.fetchMatrix(props.jobId, row, col, val)
})

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
  if (entries.length > 3) parts.push('…')
  return parts.join(', ')
}

function formatDuration(sec: number): string {
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`
}

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// Heat map styling for matrix cells
function heatStyle(val: number | null) {
  if (val == null) return { opacity: 0.3 }
  // Simple green intensity based on value relative to other cells
  return { background: 'rgba(var(--v-theme-success), 0.1)' }
}

// Polling (3s when active)
usePolling(refresh, 3000, isActive)

// Also fetch immediately on mount
onMounted(() => { store.fetchDetail(props.jobId) })
onUnmounted(() => { store.$reset() })
</script>

<style scoped>
.matrix-table {
  border-collapse: separate;
  border-spacing: 2px;
  width: 100%;
}
.matrix-table th,
.matrix-table td {
  padding: 6px 10px;
}
</style>
