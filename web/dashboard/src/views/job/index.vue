<template>
  <div v-if="detail">
    <JobHeader
      :detail="detail"
      :top-runs="topRuns"
      :metric-key="compareKey"
      :can-pause="config.caps.pause_resume"
      :is-poll="config.isPoll"
      :refreshing="refreshing"
      :pausing="pausing"
      :killing="killing"
      :archiving="archiving"
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
      <!-- role=button + aria-pressed: VChip is a focusable span and already
           handles Enter/Space itself; SR users just need the toggle semantics. -->
      <v-chip
        v-for="s in statusOptions"
        :key="s.value"
        role="button"
        :aria-pressed="statusFilter === s.value"
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
      :wandb="detail.wandb"
      :metric-keys="detail.metric_keys"
      :swept-params="sweptParams"
      :can-retry="config.caps.retry"
      @kill-task="onKillTask"
      @retry-task="onRetryTask"
      @click-task="onClickTask"
    />

    <!-- W&B external link -->
    <div v-if="detail.wandb" class="mt-4">
      <v-btn size="small" variant="tonal" :href="detail.wandb.base_url" target="_blank">
        <v-icon start size="16">mdi-chart-scatter-plot</v-icon>
        W&B
        <v-icon end size="14">mdi-open-in-new</v-icon>
      </v-btn>
    </div>
  </div>

  <div v-else class="d-flex justify-center pa-12">
    <v-progress-circular indeterminate color="primary" />
  </div>

  <!-- RQ-75: cross-generation rerun confirmation -->
  <GenerationRerunDialog v-model="genRerun.open.value" @confirm="genRerun.confirmRerun" />
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useConfigStore } from '@/stores/config'
import { usePreferences } from '@/composables/usePreferences'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfirm } from '@/composables/useConfirm'
import { useCancelling } from '@/composables/useCancelling'
import { useJobDetailQuery, useCompareQuery, useJobActions } from '@/queries/useJobQueries'
import { useTaskActions } from '@/queries/useTaskQueries'
import { useGenerationRerun } from '@/composables/useGenerationRerun'
import GenerationRerunDialog from '@/components/GenerationRerunDialog.vue'
import JobHeader from './JobHeader.vue'
import TaskTable from './TaskTable.vue'
import StatusDot from '@/components/StatusDot.vue'

const props = defineProps<{ project: string; jobId: string }>()
const router = useRouter()
const config = useConfigStore()
const prefs = usePreferences()
const snack = useSnackbar()
const { t } = useI18n()
const { confirm: confirmDialog } = useConfirm()

// ── Server state: the query cache is the single source of truth. ──
// Responses land under their own key — a slow response for job A can
// never overwrite job B, and navigation cancels in-flight fetches.
const detailQuery = useJobDetailQuery(() => props.jobId)
const detail = computed(() => detailQuery.data.value ?? null)

const jobActions = useJobActions()
const taskActions = useTaskActions(() => props.jobId)
const { cancelling, prune, displayStatus } = useCancelling()

// Clear transient cancelling entries as fresh polls land.
watch(() => detail.value?.tasks, (tasks) => { if (tasks) prune(tasks) })

const statusFilter = ref(prefs.lastStatusFilter.value)

// Task-level filter — the "done" option matches success tasks, so it uses
// the task success dot from statusGrammar. Computed so labels follow
// live locale switches.
const statusOptions = computed(() => [
  { value: '', label: t('common.all'), dot: '' },
  { value: 'running', label: t('status.task.running'), dot: 'running' },
  { value: 'done', label: t('common.done'), dot: 'success' },
  { value: 'failed', label: t('status.task.failed'), dot: 'failed' },
  { value: 'killed', label: t('status.task.killed'), dot: 'killed' },
  { value: 'pending', label: t('status.task.pending'), dot: 'pending' },
])

const isActiveJob = computed(() => {
  const s = detail.value?.job.status
  return s === 'running' || s === 'pending' || s === 'paused'
})

// Detect swept params: params that differ across tasks
const sweptParams = computed(() => {
  if (!detail.value || detail.value.tasks.length < 2) return []
  const tasks = detail.value.tasks
  const first = tasks[0].params || {}
  const varying = new Set<string>()
  for (const task of tasks.slice(1)) {
    for (const [k, v] of Object.entries(task.params || {})) {
      if (first[k] !== v) varying.add(k)
    }
    for (const k of Object.keys(first)) {
      if (!(k in (task.params || {}))) varying.add(k)
    }
  }
  return [...varying]
})

// ── Leaderboard: key selection is declarative; the query re-fetches when
// the key changes (this replaces the watch that fired /compare per poll).
const compareKey = computed(() => {
  const keys = detail.value?.metric_keys ?? []
  if (keys.length === 0) return ''
  const preferred = prefs.preferredMetrics.value[props.jobId]
  return preferred && keys.includes(preferred) ? preferred : keys[0]
})
const compareQuery = useCompareQuery(() => props.jobId, compareKey, () => true, isActiveJob)
const topRuns = computed(() => (compareQuery.data.value ?? []).slice(0, 3))

const filteredTasks = computed(() => {
  if (!detail.value) return []
  let tasks = detail.value.tasks
  if (statusFilter.value === 'done') {
    tasks = tasks.filter(task => task.status === 'success')
  } else if (statusFilter.value) {
    tasks = tasks.filter(task => task.status === statusFilter.value)
  }
  // Overlay the frontend-local cancelling state (kill_async backends).
  if (cancelling.value.size === 0) return tasks
  return tasks.map(task => ({ ...task, status: displayStatus(task) }))
})

watch(statusFilter, (v) => { prefs.lastStatusFilter.value = v })

// ── Actions: mutations own their invalidation; isPending drives the
// button loading/double-click guards. ──
const pausing = computed(() => jobActions.pause.isPending.value || jobActions.resume.isPending.value)
const killing = computed(() => jobActions.kill.isPending.value)
const archiving = computed(() => jobActions.archive.isPending.value || jobActions.unarchive.isPending.value)
const refreshing = computed(() => jobActions.refresh.isPending.value)

function togglePause() {
  if (!detail.value || pausing.value) return
  const m = detail.value.job.status === 'paused' ? jobActions.resume : jobActions.pause
  m.mutateAsync(props.jobId)
    .then(() => snack.success(t('common.done')))
    .catch((e: any) => snack.error(e?.message || t('common.error')))
}

async function killJob() {
  if (!detail.value || killing.value) return
  const counts = detail.value.job.tasks
  const ok = await confirmDialog({
    title: t('confirm.kill_job_title'),
    // unknown tasks are live work too (RQ-74) — the confirm must not
    // undercount what the kill will actually touch.
    body: t('confirm.kill_job_body', { n: counts.running + counts.pending + (counts.unknown ?? 0) }),
    confirmText: t('job.kill'),
    danger: true,
  })
  if (!ok) return
  jobActions.kill.mutateAsync(props.jobId)
    .then(() => snack.success(t('job.killed')))
    .catch((e: any) => snack.error(e?.message || t('common.error')))
}

async function onKillTask(id: string) {
  const ok = await confirmDialog({
    title: t('confirm.kill_task_title'),
    body: t('confirm.kill_task_body', { id: id.slice(0, 8) }),
    confirmText: t('job.kill'),
    danger: true,
  })
  if (!ok) return
  taskActions.kill.mutateAsync(id)
    .catch((e: any) => snack.error(e?.message || t('common.error')))
}

// RQ-75: cross-generation reruns confirm through the shared dialog.
const genRerun = useGenerationRerun(
  (p) => taskActions.retry.mutateAsync(p),
  () => {},
  (e: any) => snack.error(e?.message || t('common.error')),
)

function onRetryTask(id: string) {
  void genRerun.run(id)
}

async function archiveJob() {
  try {
    await jobActions.archive.mutateAsync({ id: props.jobId, project: props.project })
    snack.success(t('archive.job_done'))
  } catch (e: any) { snack.error(e?.message || t('common.error')) }
}

async function unarchiveJob() {
  try {
    await jobActions.unarchive.mutateAsync({ id: props.jobId, project: props.project })
    snack.success(t('archive.job_back'))
  } catch (e: any) { snack.error(e?.message || t('common.error')) }
}

// Manual reconcile (poll-model backends): forces the backend to re-read
// external sources; isPending doubles as the double-click guard.
function onRefresh() {
  if (refreshing.value) return
  jobActions.refresh.mutateAsync(props.jobId)
    .catch(() => snack.error(t('common.error')))
}

function onClickTask(id: string) {
  router.push({ name: 'task-detail', params: { project: props.project, jobId: props.jobId, taskId: id } })
}
</script>
