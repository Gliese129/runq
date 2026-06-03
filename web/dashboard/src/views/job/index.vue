<template>
  <div v-if="store.detail">
    <JobHeader
      :detail="store.detail"
      :top-runs="topRuns"
      :metric-key="selectedMetric"
      :can-pause="config.features.pause_resume"
      @pause="togglePause"
      @resume="togglePause"
      @kill="killJob"
    />

    <!-- Filter + sort bar -->
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
      <v-spacer />
      <v-select
        v-if="store.detail.metric_keys.length > 0"
        v-model="selectedMetric"
        :items="store.detail.metric_keys"
        label="Sort by metric"
        hide-details density="compact" variant="outlined"
        clearable style="max-width: 180px"
      />
      <v-btn-toggle
        v-if="selectedMetric"
        v-model="compareDesc"
        mandatory density="compact" variant="outlined"
      >
        <v-btn :value="true" size="x-small"><v-icon size="14">mdi-sort-descending</v-icon></v-btn>
        <v-btn :value="false" size="x-small"><v-icon size="14">mdi-sort-ascending</v-icon></v-btn>
      </v-btn-toggle>
    </div>

    <!-- Table: tasks or compare -->
    <CompareTable v-if="isCompareMode" :rows="store.compare" :metric-key="selectedMetric" />
    <TaskTable
      v-else
      :tasks="filteredTasks"
      :wandb="store.detail.wandb"
      @kill-task="onKillTask"
      @retry-task="onRetryTask"
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
import { useJobDetailStore } from '@/stores/jobDetail'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { usePolling } from '@/composables/usePolling'
import { useSnackbar } from '@/composables/useSnackbar'
import JobHeader from './JobHeader.vue'
import TaskTable from './TaskTable.vue'
import CompareTable from './CompareTable.vue'

const props = defineProps<{ project: string; jobId: string }>()
const store = useJobDetailStore()
const config = useConfigStore()
const prefs = usePreferences()
const snack = useSnackbar()

const statusFilter = ref(prefs.lastStatusFilter.value)
const selectedMetric = ref('')
const compareDesc = ref(prefs.compareSortDesc.value)

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

const isCompareMode = computed(() => !!selectedMetric.value)
const topRuns = computed(() => store.compare.slice(0, 3))

const filteredTasks = computed(() => {
  if (!store.detail) return []
  if (!statusFilter.value) return store.detail.tasks
  if (statusFilter.value === 'done') {
    return store.detail.tasks.filter(t => ['success', 'completed', 'done'].includes(t.status))
  }
  return store.detail.tasks.filter(t => t.status === statusFilter.value)
})

// Auto-select first metric
watch(() => store.detail?.metric_keys, (keys) => {
  if (keys && keys.length > 0 && !selectedMetric.value) {
    const preferred = prefs.preferredMetrics.value[props.jobId]
    selectedMetric.value = preferred && keys.includes(preferred) ? preferred : keys[0]
  }
}, { immediate: true })

// Fetch compare on metric/sort change
watch([selectedMetric, compareDesc], ([key, desc]) => {
  if (key) {
    prefs.setPreferredMetric(props.jobId, key)
    prefs.compareSortDesc.value = desc
    store.fetchCompare(props.jobId, key, desc)
  }
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

usePolling(refresh, 3000, isActive)
onUnmounted(() => { store.$reset() })
</script>
