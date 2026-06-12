<template>
  <v-card class="mb-4 pa-4">
    <div class="d-flex align-center justify-space-between mb-3">
      <div class="d-flex align-center ga-2">
        <JobStatusBadge :status="detail.job.status" />
        <span v-if="detail.job.note" class="text-body-2 text-on-surface-variant">{{ detail.job.note }}</span>
        <span class="text-caption text-on-surface-variant">{{ relativeTime(detail.job.created_at) }}</span>
        <!-- Poll-model honesty: state is a snapshot, say when it was taken -->
        <template v-if="isPoll">
          <v-chip size="x-small" variant="text" class="text-on-surface-variant">
            <v-icon start size="12">mdi-database-clock-outline</v-icon>
            {{ t('job.data_as_of', { time: detail.job.refreshed_at ? relativeTime(detail.job.refreshed_at) : '—' }) }}
          </v-chip>
          <v-btn
            size="x-small" variant="text" icon
            :loading="refreshing"
            :aria-label="t('job.refresh')" :title="t('job.refresh')"
            @click="$emit('refresh')"
          ><v-icon size="14">mdi-refresh</v-icon></v-btn>
        </template>
      </div>
      <div class="d-flex ga-1">
        <v-btn size="x-small" variant="tonal" color="primary" @click="$emit('rerun')">
          <v-icon start size="14">mdi-restart</v-icon> Re-run
        </v-btn>
        <v-btn v-if="canPause && isActive" size="x-small" variant="tonal"
          :color="detail.job.status === 'paused' ? 'success' : 'warning'"
          @click="emitPauseResume"
        >
          <v-icon start size="14">{{ detail.job.status === 'paused' ? 'mdi-play' : 'mdi-pause' }}</v-icon>
          {{ detail.job.status === 'paused' ? 'Resume' : 'Pause' }}
        </v-btn>
        <v-btn v-if="isActive" size="x-small" variant="tonal" color="error" @click="$emit('kill')">
          <v-icon start size="14">mdi-stop</v-icon> Kill
        </v-btn>
        <v-btn v-if="!isActive" size="x-small" variant="text"
          :title="detail.job.archived ? undefined : 'Hide from default lists — data and workspace untouched, reversible'"
          @click="toggleArchive"
        >
          <v-icon start size="14">{{ detail.job.archived ? 'mdi-archive-arrow-up-outline' : 'mdi-archive-arrow-down-outline' }}</v-icon>
          {{ detail.job.archived ? t('archive.unarchive') : t('archive.archive') }}
        </v-btn>
      </div>
    </div>

    <!-- Stats row -->
    <div class="d-flex flex-wrap ga-4 align-center">
      <div v-for="s in stats" :key="s.label" class="text-center">
        <div class="text-h6 font-weight-medium" :class="s.color">{{ s.value }}</div>
        <div class="text-caption text-on-surface-variant">{{ s.label }}</div>
      </div>
      <v-spacer />
      <div v-if="topRuns.length > 0" class="pa-2 rounded" style="background: rgb(var(--v-theme-success), 0.08)">
        <div class="text-caption text-on-surface-variant mb-1">Top 3 · {{ metricKey }}</div>
        <div class="d-flex flex-wrap ga-2">
          <div v-for="(run, i) in topRuns" :key="run.task_id" class="d-flex align-center ga-1">
            <v-icon v-if="i === 0" size="12" color="warning">mdi-trophy</v-icon>
            <span class="text-body-2 font-weight-medium" :class="i === 0 ? 'text-success' : ''">
              {{ run.best?.toPrecision(4) }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- Failed hints -->
    <div v-if="failedCount > 0" class="mt-2 pa-2 rounded" style="background: rgb(var(--v-theme-error), 0.06)">
      <div class="text-caption text-error">{{ failedCount }} failed or killed</div>
      <div v-for="ft in failedTasks.slice(0, 3)" :key="ft.id" class="text-caption d-flex align-center ga-1">
        <code>{{ ft.id.slice(0, 8) }}</code>
        <span class="text-on-surface-variant">exit {{ ft.exit_code ?? '?' }}</span>
      </div>
    </div>

    <!-- Progress -->
    <v-progress-linear :model-value="progress" color="success" height="4" rounded class="mt-3" />
  </v-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { JobDetail, CompareRow, TaskView } from '@/types/api'
import JobStatusBadge from '@/components/JobStatusBadge.vue'

const { t } = useI18n()

const props = defineProps<{
  detail: JobDetail
  topRuns: CompareRow[]
  metricKey: string
  canPause: boolean
  /** poll state model: render freshness + manual refresh */
  isPoll?: boolean
  refreshing?: boolean
}>()

const emit = defineEmits<{
  pause: []
  resume: []
  kill: []
  refresh: []
  rerun: []
  archive: []
  unarchive: []
}>()

function toggleArchive() {
  if (props.detail.job.archived) emit('unarchive')
  else emit('archive')
}

const isActive = computed(() => ['running', 'pending', 'paused'].includes(props.detail.job.status))

// Counts come from the backend summary (detail.job.tasks) — the single source
// of truth. Note the backend folds "killed" into "failed", so we don't recount
// from the task array here (that's what caused the headline / hint mismatch).
const counts = computed(() => props.detail.job.tasks)
const failedCount = computed(() => counts.value.failed)
// Per-task detail for the hint list still needs the task array; include killed
// so the listed tasks match the (failed-incl-killed) count above.
const failedTasks = computed(() =>
  props.detail.tasks.filter(t => t.status === 'failed' || t.status === 'killed'),
)
const progress = computed(() => {
  const total = counts.value.total
  return total > 0 ? (counts.value.completed / total) * 100 : 0
})

const stats = computed(() => [
  { label: 'Done', value: counts.value.completed, color: 'text-success' },
  { label: 'Running', value: counts.value.running, color: 'text-warning' },
  { label: 'Failed (incl. killed)', value: failedCount.value, color: 'text-error' },
  { label: 'Pending', value: counts.value.pending, color: 'text-info' },
])

function relativeTime(ts: number): string {
  const diff = Date.now() / 1000 - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

function emitPauseResume() {
  if (props.detail.job.status === 'paused') {
    emit('resume')
  } else {
    emit('pause')
  }
}
</script>
