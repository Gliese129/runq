<template>
  <div v-if="store.detail">
    <JobHeader
      :detail="store.detail"
      :top-runs="topRuns"
      :metric-key="topMetricKey"
      :can-pause="config.features.pause_resume"
      @pause="togglePause"
      @resume="togglePause"
      @kill="killJob"
    />

    <!-- Filter bar -->
    <div class="d-flex align-center ga-2 mb-3 flex-wrap">
      <v-chip
        v-for="s in statusOptions"
        :key="s.value"
        :variant="statusFilter === s.value ? 'flat' : 'outlined'"
        :color="statusFilter === s.value ? 'primary' : undefined"
        size="small"
        @click="statusFilter = statusFilter === s.value ? '' : s.value"
      >
        <div v-if="s.dot" class="status-dot mr-1" :class="`status-dot--${s.dot}`" style="width:6px;height:6px" />
        {{ s.label }}
      </v-chip>
    </div>

    <!-- Unified task table with param columns + sorting -->
    <TaskTable
      :tasks="filteredTasks"
      :job-id="props.jobId"
      :wandb="store.detail.wandb"
      :metric-keys="store.detail.metric_keys"
      :swept-params="sweptParams"
      @kill-task="onKillTask"
      @retry-task="onRetryTask"
      @click-task="onClickTask"
    />

    <!-- W&B tab -->
    <div v-if="store.detail.wandb" class="mt-4">
      <v-card class="pa-4">
        <div class="d-flex align-center justify-space-between mb-2">
          <div class="text-subtitle-2 d-flex align-center ga-1">
            <v-icon size="16">mdi-chart-scatter-plot</v-icon> W&B
          </div>
          <v-btn size="x-small" variant="text" :href="store.detail.wandb.base_url" target="_blank">
            <v-icon start size="14">mdi-open-in-new</v-icon> Open
          </v-btn>
        </div>
        <iframe
          :src="store.detail.wandb.base_url + '?jupyter=true'"
          style="width: 100%; height: 500px; border: none; border-radius: 4px"
        />
      </v-card>
    </div>
  </div>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { useJobDetailStore } from '@/stores/jobDetail'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { usePolling } from '@/composables/usePolling'
import { useSnackbar } from '@/composables/useSnackbar'
import JobHeader from './JobHeader.vue'
import TaskTable from './TaskTable.vue'

const props = defineProps<{ project: string; jobId: string }>()
const router = useRouter()
const store = useJobDetailStore()
const config = useConfigStore()
const prefs = usePreferences()
const snack = useSnackbar()

const statusFilter = ref(prefs.lastStatusFilter.value)

const statusOptions = [
  { value: '', label: 'All', dot: '' },
  { value: 'running', label: 'Running', dot: 'running' },
  { value: 'done', label: 'Done', dot: 'completed' },
  { value: 'failed', label: 'Failed', dot: 'failed' },
  { value: 'pending', label: 'Pending', dot: 'pending' },
]

const isActive = computed(() => {
  const s = store.detail?.job.status
  return s === 'running' || s === 'pending' || s === 'paused'
})

// Detect swept params: params that differ across tasks
const sweptParams = computed(() => {
  if (!store.detail || store.detail.tasks.length < 2) return []
  const tasks = store.detail.tasks
  const first = tasks[0].params || {}
  const varying = new Set<string>()
  for (const t of tasks.slice(1)) {
    for (const [k, v] of Object.entries(t.params || {})) {
      if (first[k] !== v) varying.add(k)
    }
    for (const k of Object.keys(first)) {
      if (!(k in (t.params || {}))) varying.add(k)
    }
  }
  return [...varying]
})

// For JobHeader top runs — use first metric key
const topMetricKey = computed(() => store.detail?.metric_keys?.[0] || '')
const topRuns = computed(() => store.compare.slice(0, 3))

// Auto-fetch compare for top runs display
watch(() => store.detail?.metric_keys, (keys) => {
  if (keys && keys.length > 0) {
    const preferred = prefs.preferredMetrics.value[props.jobId]
    const key = preferred && keys.includes(preferred) ? preferred : keys[0]
    store.fetchCompare(props.jobId, key, true)
  }
}, { immediate: true })

const filteredTasks = computed(() => {
  if (!store.detail) return []
  if (!statusFilter.value) return store.detail.tasks
  if (statusFilter.value === 'done') {
    return store.detail.tasks.filter(t => ['success', 'completed', 'done'].includes(t.status))
  }
  return store.detail.tasks.filter(t => t.status === statusFilter.value)
})

watch(statusFilter, (v) => { prefs.lastStatusFilter.value = v })

function refresh(silent = false) { store.fetchDetail(props.jobId, silent) }

function togglePause() {
  if (!store.detail) return
  const p = store.detail.job.status === 'paused'
    ? store.resumeJob(props.jobId)
    : store.pauseJob(props.jobId)
  p.then(() => { snack.success('Done'); refresh() })
}

function killJob() {
  store.killJob(props.jobId).then(() => { snack.success('Job killed'); refresh() })
}

function onKillTask(id: string) {
  store.killTask(id).then(() => refresh())
}

function onRetryTask(id: string) {
  store.retryTask(id).then(() => refresh())
}

function onClickTask(id: string) {
  router.push({ name: 'task-detail', params: { project: props.project, jobId: props.jobId, taskId: id } })
}

usePolling(refresh, 3000, isActive)
onUnmounted(() => { store.$reset() })
</script>
