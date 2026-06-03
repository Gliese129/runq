<template>
  <v-card class="mb-4 pa-4">
    <div class="d-flex align-center justify-space-between mb-3">
      <div class="d-flex align-center ga-2">
        <div class="status-dot" :class="`status-dot--${detail.job.status}`" style="width:10px;height:10px" />
        <span v-if="detail.job.note" class="text-body-2 text-on-surface-variant">{{ detail.job.note }}</span>
        <span class="text-caption text-on-surface-variant">{{ relativeTime(detail.job.created_at) }}</span>
      </div>
      <div class="d-flex ga-1">
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
      <div class="text-caption text-error">{{ failedCount }} failed</div>
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
import type { JobDetail, CompareRow, TaskView } from '@/types/api'

const props = defineProps<{
  detail: JobDetail
  topRuns: CompareRow[]
  metricKey: string
  canPause: boolean
}>()

const emit = defineEmits<{
  pause: []
  resume: []
  kill: []
}>()

const isActive = computed(() => ['running', 'pending', 'paused'].includes(props.detail.job.status))
const failedTasks = computed(() => props.detail.tasks.filter(t => t.status === 'failed'))
const failedCount = computed(() => failedTasks.value.length)
const doneCount = computed(() => props.detail.tasks.filter(t => ['success', 'completed', 'done'].includes(t.status)).length)
const progress = computed(() => {
  const total = props.detail.tasks.length
  return total > 0 ? (doneCount.value / total) * 100 : 0
})

const stats = computed(() => [
  { label: 'Done', value: doneCount.value, color: 'text-success' },
  { label: 'Running', value: props.detail.tasks.filter(t => t.status === 'running').length, color: 'text-warning' },
  { label: 'Failed', value: failedCount.value, color: 'text-error' },
  { label: 'Pending', value: props.detail.tasks.filter(t => t.status === 'pending').length, color: 'text-info' },
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
