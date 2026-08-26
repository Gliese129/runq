<template>
  <v-card class="pa-0">
    <!-- Header: view toggle + ingest health -->
    <div class="d-flex align-center ga-2 px-4 py-2 border-b flex-wrap">
      <v-btn-toggle v-model="view" density="compact" mandatory variant="outlined" divided>
        <v-btn value="table" size="small" :aria-label="t('results.view_table')">
          <v-icon size="16">mdi-table</v-icon>
        </v-btn>
        <v-btn value="chart" size="small" :aria-label="t('results.view_chart')">
          <v-icon size="16">mdi-chart-line</v-icon>
        </v-btn>
      </v-btn-toggle>
      <v-spacer />
      <span v-if="res" class="text-caption text-on-surface-variant font-mono">
        {{ t('results.parsed_n', { n: res.parsed }) }}
      </span>
      <v-progress-circular v-if="loading" indeterminate size="14" width="2" color="primary" />
    </div>

    <!-- Known-loss banner: skipped/truncated/nulled travel with the data -->
    <div v-if="lossNotes.length" class="px-4 py-2 text-caption text-warning border-b">
      <div v-for="(note, i) in lossNotes" :key="i">! {{ note }}</div>
    </div>

    <div v-if="error" class="pa-6 text-caption text-error">{{ error }}</div>

    <!-- Empty: the contract is the hint — this is how records appear -->
    <div v-else-if="res && res.n === 0" class="pa-8 text-center">
      <div class="text-body-2 text-on-surface-variant mb-2">{{ t('results.empty_title') }}</div>
      <code class="text-caption font-mono">runq.record({loss: 0.5}, model="base", step=100)</code>
      <div class="text-caption text-on-surface-variant mt-2">{{ t('results.empty_hint') }}</div>
    </div>

    <template v-else-if="res && rows.length">
      <!-- ── Table view ── -->
      <div v-if="view === 'table'">
        <div class="d-flex align-center ga-2 px-4 py-2 flex-wrap">
          <v-text-field
            v-model="labelTemplate"
            :label="t('results.label_template')"
            :placeholder="templatePlaceholder"
            density="compact" hide-details variant="outlined"
            class="font-mono template-input" clearable
          />
          <!-- Discoverability beats a caret popup: every usable key is one
               click away, inserted at the end of the template. -->
          <v-chip
            v-for="k in templateKeys" :key="k"
            size="x-small" variant="outlined" class="font-mono"
            @click="appendKey(k)"
          >{{ '{' + k + '}' }}</v-chip>
          <v-spacer />
          <v-btn size="small" variant="text" @click="copyMarkdown">
            <v-icon start size="14">mdi-language-markdown</v-icon>{{ t('results.copy_md') }}
          </v-btn>
        </div>

        <!-- Δ vs base: the base is a RECORD (group × step), not a row —
             comparing against a mid-training checkpoint is legitimate. -->
        <div class="d-flex align-center ga-3 px-4 pb-2 flex-wrap">
          <v-switch
            v-model="deltaOn" density="compact" hide-details color="primary"
            :label="t('results.delta_vs_base')"
          />
          <template v-if="deltaOn">
            <v-select
              v-model="baseGi" :items="baseRowItems" density="compact" hide-details
              variant="outlined" class="base-select font-mono" :label="t('results.base_row')"
            />
            <v-select
              v-model="baseIdx" :items="baseStepItems" density="compact" hide-details
              variant="outlined" class="base-select font-mono" :label="t('results.base_step')"
              :disabled="baseStepItems.length === 0"
            />
          </template>
        </div>

        <div class="overflow-x-auto">
          <table class="data-mono results-table">
            <thead>
              <tr>
                <th :style="colStyle('__run')">
                  {{ t('results.run_col') }}
                  <span class="resize-h" @pointerdown="startResize('__run', $event)" />
                </th>
                <th v-for="c in res.schema.metrics" :key="c" :style="colStyle(c)" class="num-col">
                  <button
                    type="button" class="th-sort-btn"
                    :title="t('results.flip_dir')"
                    @click="flipDir(c)"
                  >{{ c }} {{ dirOf(c) === 'min' ? '↓' : '↑' }}</button>
                  <span class="resize-h" @pointerdown="startResize(c, $event)" />
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(row, ri) in rows" :key="row.gi">
                <td class="text-primary">
                  {{ renderLabel(labelTemplate, row) }}
                  <v-chip v-if="deltaOn && row.gi === baseGi" size="x-small" variant="tonal" class="ml-1">
                    {{ t('results.base_chip') }}
                  </v-chip>
                  <span
                    v-if="row.behind && row.atX !== null"
                    class="text-warning text-caption ml-1"
                    :title="t('results.behind_at', { x: row.atX })"
                  >@{{ row.atX }}</span>
                </td>
                <td v-for="c in res.schema.metrics" :key="c" class="num-col" :class="cellClass(row, ri, c)">
                  {{ cellText(row, c) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- ── Chart view ── -->
      <div v-else class="px-4 py-2">
        <div class="d-flex align-center ga-2 flex-wrap mb-2">
          <v-chip
            v-for="m in res.schema.metrics" :key="m"
            size="x-small" role="button" :aria-pressed="chartMetric === m"
            :variant="chartMetric === m ? 'flat' : 'outlined'"
            :color="chartMetric === m ? 'primary' : undefined"
            @click="chartMetric = m"
          >{{ m }}</v-chip>
          <v-spacer />
          <v-btn-toggle
            v-if="res.schema.x_axes.length > 1"
            v-model="chartX" density="compact" mandatory variant="outlined" divided
          >
            <v-btn v-for="x in res.schema.x_axes" :key="x" :value="x" size="x-small" class="font-mono">
              {{ x }}
            </v-btn>
          </v-btn-toggle>
          <v-btn size="small" variant="text" @click="copyImage">
            <v-icon start size="14">mdi-image-outline</v-icon>{{ t('results.copy_png') }}
          </v-btn>
        </div>
        <ResultsChart
          ref="chartRef"
          :series="chartSeriesData"
          :x-title="chartX"
          :metric="chartMetric"
        />
      </div>

      <!-- Contract footnote (kit note reworded to the real event contract;
           archive moves nothing, so no archive caveat) -->
      <div class="px-4 py-2 text-caption text-on-surface-variant border-t">
        {{ t('results.contract_note') }} · {{ t('results.updated', { when: updatedText }) }}
      </div>
    </template>

    <div v-else-if="loading" class="d-flex justify-center pa-8">
      <v-progress-circular indeterminate color="primary" size="20" />
    </div>
  </v-card>
</template>

<script setup lang="ts">
// JobResultsCard (RQ2-4 ②, kit ScreensC JobResultsCard) — the ablation
// table over the columnar results wire. Rows are identity groups at their
// latest slice (lagging rows annotated "@x", never silently compared);
// per-column best direction is guessed from the name and flippable; Δ
// compares against a picked (group × step) RECORD. All slice logic lives
// in resultsView.ts — this file is rendering and interaction state.
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { usePreferences } from '@/composables/usePreferences'
import { useSnackbar } from '@/composables/useSnackbar'
import type { JobResultsResponse } from '@/types/api'
import ResultsChart from './ResultsChart.vue'
import {
  tableRows, renderLabel, labelKeys, guessDir, colDecimals, fmtNum,
  bestIndex, groupXOptions, metricsAt, resultsMarkdown, groupSeries, primaryX,
  type ResultRow,
} from './resultsView'

const props = defineProps<{
  res: JobResultsResponse | null
  loading: boolean
  error: string
  project: string
}>()

const { t, locale } = useI18n()
const prefs = usePreferences()
const snack = useSnackbar()

const res = computed(() => props.res)
const view = ref<'table' | 'chart'>('table')
const rows = computed(() => (res.value ? tableRows(res.value) : []))

// ── Known loss: the banner travels with the data (RQ2-1 honesty rule) ──
const lossNotes = computed(() => {
  const r = res.value
  if (!r) return []
  const notes: string[] = []
  if (r.skipped > 0) notes.push(t('results.skipped_n', { n: r.skipped }))
  if (r.truncated) notes.push(t('results.truncated'))
  for (const [name, ax] of Object.entries(r.schema.axes)) {
    if (ax.nulled) notes.push(t('results.nulled_axis', { axis: name, n: ax.nulled }))
  }
  return notes
})

const updatedText = computed(() => {
  const ts = res.value?.updated_at
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString(locale.value)
})

// ── Row label template (persisted per project) ──
const labelTemplate = ref(prefs.getResultLabelTemplate(props.project))
watch(labelTemplate, v => prefs.setResultLabelTemplate(props.project, v ?? ''))
const templateKeys = computed(() => (res.value ? labelKeys(res.value) : []))
const templatePlaceholder = computed(() => {
  const ks = templateKeys.value.filter(k => k !== 'group').slice(0, 2)
  return ks.length ? ks.map(k => `{${k}}`).join(' · ') : '{group}'
})
function appendKey(k: string) {
  const cur = labelTemplate.value ?? ''
  labelTemplate.value = cur ? `${cur} {${k}}` : `{${k}}`
}

// ── Per-column direction (guess + flip) and decimals ──
const flipped = ref<Set<string>>(new Set())
function dirOf(c: string): 'min' | 'max' {
  const g = guessDir(c)
  return flipped.value.has(c) ? (g === 'min' ? 'max' : 'min') : g
}
function flipDir(c: string) {
  const s = new Set(flipped.value)
  if (s.has(c)) s.delete(c)
  else s.add(c)
  flipped.value = s
}

const decimals = computed<Record<string, number>>(() => {
  const out: Record<string, number> = {}
  if (!res.value) return out
  for (const c of res.value.schema.metrics) {
    out[c] = colDecimals(res.value.cols.metrics[c] ?? [])
  }
  return out
})

const bests = computed<Record<string, number>>(() => {
  const out: Record<string, number> = {}
  if (!res.value) return out
  for (const c of res.value.schema.metrics) out[c] = bestIndex(rows.value, c, dirOf(c))
  return out
})

// ── Δ vs base: a (group × step) record ──
const deltaOn = ref(false)
const baseGi = ref<number | null>(null)
const baseIdx = ref<number | null>(null)

const baseRowItems = computed(() =>
  rows.value.map(r => ({ title: renderLabel(labelTemplate.value ?? '', r), value: r.gi })))

const baseStepItems = computed(() => {
  if (res.value == null || baseGi.value == null) return []
  const x = primaryX(res.value)
  return groupXOptions(res.value, baseGi.value, x).map(o => ({
    title: `${x || '#'} ${o.xv}`, value: o.idx,
  }))
})

// Default base: first row at its latest step. Re-validate when data moves
// under the picker (poll) — a vanished index resets, never dangles.
watch([deltaOn, rows], () => {
  if (!deltaOn.value || rows.value.length === 0) return
  if (baseGi.value == null || !rows.value.some(r => r.gi === baseGi.value)) {
    baseGi.value = rows.value[0].gi
    baseIdx.value = rows.value[0].idx
  }
})
watch(baseGi, (gi) => {
  if (gi == null) return
  const row = rows.value.find(r => r.gi === gi)
  baseIdx.value = row ? row.idx : null
})

const baseMetrics = computed(() =>
  res.value && baseIdx.value != null ? metricsAt(res.value, baseIdx.value) : null)

function cellText(row: ResultRow, c: string): string {
  const dec = decimals.value[c] ?? 0
  const v = row.metrics[c]
  if (!deltaOn.value || !baseMetrics.value || row.gi === baseGi.value) return fmtNum(v, dec)
  const b = baseMetrics.value[c]
  if (v === null || b === null) return '—'
  const d = v - b
  return `${d >= 0 ? '+' : '−'}${Math.abs(d).toFixed(dec)}`
}

function cellClass(row: ResultRow, ri: number, c: string): string {
  if (deltaOn.value && baseMetrics.value && row.gi !== baseGi.value) {
    const v = row.metrics[c]
    const b = baseMetrics.value[c]
    if (v === null || b === null || v === b) return 'text-on-surface-variant'
    const better = dirOf(c) === 'min' ? v < b : v > b
    return better ? 'text-success' : 'text-error'
  }
  return bests.value[c] === ri ? 'best-cell' : ''
}

// ── Column drag-resize (kit behavior; plain pointer tracking) ──
const colWidths = ref<Record<string, number>>({})
function colStyle(c: string) {
  const w = colWidths.value[c]
  return w ? { width: `${w}px`, minWidth: `${w}px` } : undefined
}
function startResize(c: string, e: PointerEvent) {
  e.preventDefault()
  const th = (e.target as HTMLElement).closest('th')
  const startW = colWidths.value[c] ?? th?.getBoundingClientRect().width ?? 120
  const startX = e.clientX
  const move = (ev: PointerEvent) => {
    colWidths.value = { ...colWidths.value, [c]: Math.max(64, startW + ev.clientX - startX) }
  }
  const up = () => {
    window.removeEventListener('pointermove', move)
    window.removeEventListener('pointerup', up)
  }
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up)
}

// ── Copy actions ──
async function copyMarkdown() {
  if (!res.value) return
  const dirs: Record<string, 'min' | 'max'> = {}
  for (const c of res.value.schema.metrics) dirs[c] = dirOf(c)
  const md = resultsMarkdown(rows.value, res.value.schema.metrics, labelTemplate.value ?? '', decimals.value, dirs)
  try {
    await navigator.clipboard.writeText(md)
    snack.success(t('common.copied'))
  } catch {
    snack.error(t('common.error'))
  }
}

const chartRef = ref<InstanceType<typeof ResultsChart>>()
async function copyImage() {
  const ok = await chartRef.value?.copyPng()
  if (ok) snack.success(t('common.copied'))
  else snack.error(t('common.error'))
}

// ── Chart state ──
const chartMetric = ref('')
const chartX = ref('')
watch(res, (r) => {
  if (!r) return
  if (!chartMetric.value || !r.schema.metrics.includes(chartMetric.value)) {
    chartMetric.value = r.schema.metrics[0] ?? ''
  }
  if (!chartX.value || !r.schema.x_axes.includes(chartX.value)) {
    chartX.value = r.schema.x_axes[0] ?? ''
  }
}, { immediate: true })

const chartSeriesData = computed(() => {
  if (!res.value || !chartMetric.value || !chartX.value) return []
  return groupSeries(res.value, chartMetric.value, chartX.value).map(s => {
    const row = rows.value.find(r => r.gi === s.gi)
    return {
      label: row ? renderLabel(labelTemplate.value ?? '', row) : s.key,
      points: s.points,
    }
  })
})
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.border-t { border-top: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.template-input { max-width: 320px; }
.base-select { max-width: 220px; }
.results-table { width: 100%; }
.results-table th { position: relative; white-space: nowrap; }
.results-table .num-col { text-align: right; font-variant-numeric: tabular-nums; }
.best-cell { color: rgb(var(--v-theme-primary)); font-weight: 600; }
.resize-h {
  position: absolute;
  right: 0;
  top: 0;
  bottom: 0;
  width: 6px;
  cursor: col-resize;
  user-select: none;
}
.resize-h:hover { background: rgb(var(--v-theme-primary), 0.3); }
.th-sort-btn {
  background: none;
  border: none;
  padding: 0;
  font: inherit;
  color: inherit;
  cursor: pointer;
}
</style>
