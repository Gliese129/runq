<template>
  <v-card class="pa-0">
    <!-- Column visibility + sort controls -->
    <div class="d-flex align-center ga-2 pa-3 flex-wrap" style="border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant))">
      <div class="text-caption text-on-surface-variant">Columns:</div>
      <v-chip
        v-for="col in availableParamCols" :key="col"
        size="x-small"
        :variant="visibleCols.has(col) ? 'flat' : 'outlined'"
        :color="visibleCols.has(col) ? 'primary' : undefined"
        @click="toggleCol(col)"
      >{{ col }}</v-chip>
      <v-spacer />
      <v-chip
        v-for="col in availableMetricCols" :key="col"
        size="x-small"
        :variant="visibleCols.has(col) ? 'flat' : 'outlined'"
        :color="visibleCols.has(col) ? 'success' : undefined"
        @click="toggleCol(col)"
      >
        <v-icon start size="10">mdi-chart-line</v-icon>{{ col }}
      </v-chip>
    </div>

    <div class="overflow-x-auto">
      <table class="data-mono" style="width: 100%">
        <thead>
          <tr>
            <th style="width:24px"></th>
            <th class="cursor-pointer" @click="setSort('id')">
              ID {{ sortIndicator('id') }}
            </th>
            <th
              v-for="col in shownCols" :key="col"
              class="cursor-pointer"
              @click="setSort(col)"
            >
              {{ col }} {{ sortIndicator(col) }}
            </th>
            <th class="cursor-pointer" @click="setSort('step')">Step {{ sortIndicator('step') }}</th>
            <th class="cursor-pointer" @click="setSort('elapsed')">Elapsed {{ sortIndicator('elapsed') }}</th>
            <th v-if="hasWandb" style="width:36px"></th>
            <th style="width:70px"></th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="task in sortedTasks" :key="task.id"
            class="cursor-pointer"
            @click="$emit('click-task', task.id)"
          >
            <td><div class="status-dot" :class="`status-dot--${task.status}`" /></td>
            <td><code>{{ task.id.slice(0, 8) }}</code></td>
            <td v-for="col in shownCols" :key="col" :class="paramColClass(col)">
              {{ task.params[col] ?? task.metrics?.[col] ?? '—' }}
            </td>
            <td>{{ task.current_step ?? '—' }}</td>
            <td class="text-on-surface-variant">{{ task.elapsed_seconds ? formatDuration(task.elapsed_seconds) : '—' }}</td>
            <td v-if="hasWandb">
              <v-btn v-if="task.wandb_run_id" size="x-small" variant="text" icon
                :href="wandbRunURL(task.wandb_run_id)" target="_blank" @click.stop
                :aria-label="t('job.wandb_open')" :title="t('job.wandb_open')"
              ><v-icon size="14">mdi-open-in-new</v-icon></v-btn>
            </td>
            <td>
              <div class="d-flex ga-1">
                <v-btn v-if="task.status === 'running'" icon size="x-small" variant="text" color="error"
                  @click.stop="$emit('kill-task', task.id)"
                  :aria-label="t('job.kill')" :title="t('job.kill')"
                ><v-icon size="14">mdi-stop</v-icon></v-btn>
                <v-btn v-if="task.status === 'failed'" icon size="x-small" variant="text" color="primary"
                  @click.stop="$emit('retry-task', task.id)"
                  :aria-label="t('job.retry')" :title="t('job.retry')"
                ><v-icon size="14">mdi-refresh</v-icon></v-btn>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="tasks.length === 0" class="text-center text-on-surface-variant pa-6">
      No tasks match the filter
    </div>
  </v-card>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { TaskView, WandbInfo } from '@/types/api'
import { usePreferences } from '@/composables/usePreferences'

const { t } = useI18n()
const prefs = usePreferences()

const props = defineProps<{
  tasks: TaskView[]
  jobId: string
  wandb?: WandbInfo | null
  metricKeys?: string[]
  sweptParams?: string[]  // params that vary across tasks
}>()

defineEmits<{ 'kill-task': [id: string]; 'retry-task': [id: string]; 'click-task': [id: string] }>()

const hasWandb = computed(() => !!props.wandb)

// ── Column visibility (persisted per job) ──
const visibleCols = ref(new Set<string>())

const availableParamCols = computed(() => {
  if (props.tasks.length === 0) return []
  const all = new Set<string>()
  for (const t of props.tasks) {
    for (const k of Object.keys(t.params || {})) all.add(k)
  }
  const swept = props.sweptParams || []
  return [...swept, ...[...all].filter(k => !swept.includes(k))]
})

const availableMetricCols = computed(() => props.metricKeys || [])

// Load saved cols or default to swept params
const saved = prefs.getJobVisibleCols(props.jobId)
if (saved) {
  visibleCols.value = new Set(saved)
} else if (props.sweptParams?.length) {
  for (const p of props.sweptParams) visibleCols.value.add(p)
}

// Persist on change
watch(visibleCols, (v) => {
  prefs.setJobVisibleCols(props.jobId, [...v])
}, { deep: true })

const shownCols = computed(() =>
  [...availableParamCols.value, ...availableMetricCols.value].filter(c => visibleCols.value.has(c))
)

function toggleCol(col: string) {
  const s = new Set(visibleCols.value)
  if (s.has(col)) s.delete(col)
  else s.add(col)
  visibleCols.value = s
}

function paramColClass(col: string): string {
  if (props.sweptParams?.includes(col)) return 'swept-col'
  if (availableMetricCols.value.includes(col)) return 'metric-col'
  return 'text-on-surface-variant'
}

// ── Sorting ──
const sortKey = ref('')
const sortDesc = ref(true)

function setSort(key: string) {
  if (sortKey.value === key) sortDesc.value = !sortDesc.value
  else { sortKey.value = key; sortDesc.value = true }
}

function sortIndicator(key: string): string {
  if (sortKey.value !== key) return ''
  return sortDesc.value ? '↓' : '↑'
}

const sortedTasks = computed(() => {
  if (!sortKey.value) return props.tasks
  const key = sortKey.value
  return [...props.tasks].sort((a, b) => {
    let va: any, vb: any
    if (key === 'id') { va = a.id; vb = b.id }
    else if (key === 'step') { va = a.current_step ?? -1; vb = b.current_step ?? -1 }
    else if (key === 'elapsed') { va = a.elapsed_seconds ?? 0; vb = b.elapsed_seconds ?? 0 }
    else {
      // Param or metric column
      va = a.params?.[key] ?? a.metrics?.[key] ?? ''
      vb = b.params?.[key] ?? b.metrics?.[key] ?? ''
    }
    // Numeric comparison if both are numbers
    const na = Number(va), nb = Number(vb)
    if (!isNaN(na) && !isNaN(nb)) {
      return sortDesc.value ? nb - na : na - nb
    }
    // String comparison
    const cmp = String(va).localeCompare(String(vb))
    return sortDesc.value ? -cmp : cmp
  })
})

// ── Helpers ──
function wandbRunURL(runId: string): string {
  if (props.wandb?.base_url) return `${props.wandb.base_url}/runs/${runId}`
  return `https://wandb.ai/runs/${runId}`
}

function formatDuration(sec: number): string {
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
</script>

<style scoped>
.swept-col { color: rgb(var(--v-theme-primary)); font-weight: 500; }
.metric-col { color: rgb(var(--v-theme-success)); font-weight: 500; }
</style>
