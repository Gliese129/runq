<template>
  <v-card class="mb-4 pa-4">
    <!-- flex-wrap: at narrow viewports (375px, or longer ja/zh labels) the
         action group wraps onto its own line instead of overflowing past
         the viewport edge with no scroll affordance. -->
    <div class="d-flex align-center justify-space-between mb-3 flex-wrap ga-2">
      <div class="d-flex align-center ga-2 flex-wrap">
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
      <div class="d-flex ga-1 flex-wrap">
        <v-btn size="x-small" variant="tonal" color="primary" @click="$emit('rerun')">
          <v-icon start size="14">mdi-restart</v-icon> {{ t('job.rerun') }}
        </v-btn>
        <v-btn v-if="canPause && isActive" size="x-small" variant="tonal"
          :color="detail.job.status === 'paused' ? 'success' : 'warning'"
          :loading="pausing" :disabled="pausing"
          @click="emitPauseResume"
        >
          <v-icon start size="14">{{ detail.job.status === 'paused' ? 'mdi-play' : 'mdi-pause' }}</v-icon>
          {{ detail.job.status === 'paused' ? t('job.resume') : t('job.pause') }}
        </v-btn>
        <v-btn v-if="isActive" size="x-small" variant="tonal" color="error"
          :loading="killing" :disabled="killing"
          @click="$emit('kill')"
        >
          <v-icon start size="14">mdi-stop</v-icon> {{ t('job.kill') }}
        </v-btn>
        <v-btn v-if="!isActive" size="x-small" variant="text"
          :loading="archiving" :disabled="archiving"
          :title="detail.job.archived ? undefined : t('archive.job_tooltip')"
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
        <div class="text-h6 font-weight-medium" :style="{ color: s.css }">{{ s.value }}</div>
        <div class="text-caption text-on-surface-variant">{{ s.label }}</div>
      </div>
      <v-spacer />
      <div v-if="topRuns.length > 0" class="pa-2 rounded" style="background: rgb(var(--v-theme-success), 0.08)">
        <div class="text-caption text-on-surface-variant mb-1">{{ t('job.top_n', { n: 3 }) }} · {{ metricKey }}</div>
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
      <div class="text-caption text-error">{{ t('job.n_failed_or_killed', { n: failedCount }) }}</div>
      <div v-for="ft in failedTasks.slice(0, 3)" :key="ft.id" class="text-caption d-flex align-center ga-1">
        <code>{{ ft.id.slice(0, 8) }}</code>
        <span class="text-on-surface-variant">{{ t('job.exit_code', { code: ft.exit_code ?? '?' }) }}</span>
      </div>
    </div>

    <!-- Progress -->
    <SegmentedProgress :counts="counts" :height="4" class="mt-3" />
  </v-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { JobDetail, CompareRow, TaskView } from '@/types/api'
import JobStatusBadge from '@/components/JobStatusBadge.vue'
import SegmentedProgress from '@/components/SegmentedProgress.vue'
import { statusStyle } from '@/components/statusGrammar'
import { relativeTime } from '@/utils/relativeTime'

const { t } = useI18n()

const props = defineProps<{
  detail: JobDetail
  topRuns: CompareRow[]
  metricKey: string
  canPause: boolean
  /** poll state model: render freshness + manual refresh */
  isPoll?: boolean
  refreshing?: boolean
  /** in-flight guards for async actions (loading + double-click protection) */
  pausing?: boolean
  killing?: boolean
  archiving?: boolean
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
  props.detail.tasks.filter(task => task.status === 'failed' || task.status === 'killed'),
)
// Stat colors come from statusGrammar (U3) — no hard-coded text-* classes.
const stats = computed(() => [
  { label: t('common.done'), value: counts.value.completed, css: statusStyle('task', 'success').css },
  { label: t('status.task.running'), value: counts.value.running, css: statusStyle('task', 'running').css },
  { label: t('job.failed_incl_killed'), value: failedCount.value, css: statusStyle('task', 'failed').css },
  { label: t('status.task.pending'), value: counts.value.pending, css: statusStyle('task', 'pending').css },
])

function emitPauseResume() {
  if (props.detail.job.status === 'paused') {
    emit('resume')
  } else {
    emit('pause')
  }
}
</script>
