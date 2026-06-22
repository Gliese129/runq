<template>
  <div v-if="store.detail">
    <JobHeader
      :detail="store.detail"
      :top-runs="topRuns"
      :metric-key="topMetricKey"
      :can-pause="config.caps.pause_resume"
      :is-poll="config.isPoll"
      :refreshing="refreshing"
      @pause="togglePause"
      @resume="togglePause"
      @kill="killJob"
      @refresh="onRefresh"
      @archive="archiveJob"
      @unarchive="unarchiveJob"
      @rerun="router.push({ name: 'submit', query: { fromJob: props.jobId } })"
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
        <StatusDot v-if="s.dot" :status="s.dot" :size="6" class="mr-1" />
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

    <!-- W&B external link -->
    <div v-if="store.detail.wandb" class="mt-4">
      <v-btn size="small" variant="tonal" :href="store.detail.wandb.base_url" target="_blank">
        <v-icon start size="16">mdi-chart-scatter-plot</v-icon>
        W&B
        <v-icon end size="14">mdi-open-in-new</v-icon>
      </v-btn>
    </div>
  </div>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useJobDetailStore } from '@/stores/jobDetail'
import { useJobsStore } from '@/stores/jobs'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { usePolling } from '@/composables/usePolling'
import { useSnackbar } from '@/composables/useSnackbar'
import JobHeader from './JobHeader.vue'
import TaskTable from './TaskTable.vue'
import StatusDot from '@/components/StatusDot.vue'

const props = defineProps<{ project: string; jobId: string }>()
const router = useRouter()
const store = useJobDetailStore()
const config = useConfigStore()
const prefs = usePreferences()
const snack = useSnackbar()
const jobsStore = useJobsStore()
const { t } = useI18n()

const statusFilter = ref(prefs.lastStatusFilter.value)

// Task-level filter — the "done" option matches success tasks, so it uses
// the task success dot from statusGrammar.
const statusOptions = [
  { value: '', label: 'All', dot: '' },
  { value: 'running', label: 'Running', dot: 'running' },
  { value: 'done', label: 'Done', dot: 'success' },
  { value: 'failed', label: 'Failed', dot: 'failed' },
  { value: 'killed', label: 'Killed', dot: 'killed' },
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
  let tasks = store.detail.tasks
  if (statusFilter.value === 'done') {
    tasks = tasks.filter(t => t.status === 'success')
  } else if (statusFilter.value) {
    tasks = tasks.filter(t => t.status === statusFilter.value)
  }
  // Overlay the frontend-local cancelling state (kill_async backends).
  if (store.cancelling.size === 0) return tasks
  return tasks.map(t => ({ ...t, status: store.displayStatus(t) }))
})

watch(statusFilter, (v) => { prefs.lastStatusFilter.value = v })

function refresh(silent = false) { store.fetchDetail(props.jobId, silent) }

async function archiveJob() {
  try {
    await jobsStore.archiveJob(props.jobId) // store action refreshes lists
    snack.success(t('archive.job_done'))
    refresh(true)
  } catch (e: any) { snack.error(e?.message || 'Archive failed') }
}

async function unarchiveJob() {
  try {
    await jobsStore.unarchiveJob(props.jobId)
    snack.success(t('archive.job_back'))
    refresh(true)
  } catch (e: any) { snack.error(e?.message || 'Unarchive failed') }
}

// Manual reconcile (poll-model backends): forces the backend to re-read
// external sources, then re-fetches. The button's loading state doubles as
// a double-click guard against hammering the cluster scheduler.
const refreshing = ref(false)
function onRefresh() {
  if (refreshing.value) return
  refreshing.value = true
  store.refreshJob(props.jobId)
    .catch(() => snack.error('Refresh failed'))
    .finally(() => { refreshing.value = false })
}

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

// Poll-model backends reconcile via the scheduler (qstat) — be a polite
// login-node citizen: 30s. Push-model (daemon) data is free: keep 3s live.
usePolling(refresh, () => (config.isPoll ? 30000 : 3000), isActive)
onUnmounted(() => { store.$reset() })
</script>
