<template>
  <div>
    <div v-if="metricKeys.length > 1" class="d-flex flex-wrap ga-1 mb-2">
      <v-chip
        v-for="key in metricKeys" :key="key"
        size="x-small"
        role="button"
        :aria-pressed="visibleKeys.has(key)"
        :variant="visibleKeys.has(key) ? 'flat' : 'outlined'"
        :color="visibleKeys.has(key) ? 'primary' : undefined"
        @click="toggleKey(key)"
      >{{ key }}</v-chip>
    </div>
    <!-- One fixed-height frame hosts BOTH the canvas and the empty text:
         the two states share the same box, so neither data arrival nor an
         emptied selection can shift the layout. The canvas stays in flow
         (a chartless canvas is invisible); the empty message overlays it
         and carries the accessible text while there is nothing to show. -->
    <div class="chart-frame">
      <!-- role="img" only while a chart is actually drawn: an empty-named
           image is screen-reader noise. Two DISTINCT empty states: no data
           at all vs. data present but every series toggled off. -->
      <canvas
        ref="canvas"
        :role="hasChart ? 'img' : undefined"
        :aria-label="hasChart ? chartLabel : undefined"
        :aria-hidden="hasChart ? undefined : 'true'"
      />
      <div v-if="!hasChart" class="chart-empty text-caption text-on-surface-variant">
        {{ hasData ? t('task.all_series_hidden') : t('task.no_metrics') }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useTheme } from 'vuetify'
import { Chart, LineController, LineElement, PointElement, LinearScale, CategoryScale, Legend, Tooltip, Filler } from 'chart.js'

Chart.register(LineController, LineElement, PointElement, LinearScale, CategoryScale, Legend, Tooltip, Filler)

interface MetricPoint {
  key: string
  value: number
  step?: number
  ts: number
}

const props = defineProps<{
  points: MetricPoint[]
}>()

const { t } = useI18n()
const theme = useTheme()

const canvas = ref<HTMLCanvasElement>()
let chart: Chart | null = null

const metricKeys = computed(() => {
  const keys = new Set<string>()
  for (const p of props.points) keys.add(p.key)
  return [...keys].sort()
})

const visibleKeys = ref(new Set<string>())
// Sticks once the user empties the selection: without it, the next poll
// (new metricKeys array reference) re-ticked the first 3 keys and
// overrode an explicit "show nothing" choice.
let userCleared = false

// Auto-show first 3 keys (initial state only — never after a user clear)
watch(metricKeys, (keys) => {
  if (!userCleared && visibleKeys.value.size === 0 && keys.length > 0) {
    for (const k of keys.slice(0, 3)) visibleKeys.value.add(k)
  }
}, { immediate: true })

function toggleKey(key: string) {
  const s = new Set(visibleKeys.value)
  if (s.has(key)) s.delete(key)
  else s.add(key)
  userCleared = s.size === 0
  visibleKeys.value = s
}

const hasData = computed(() => props.points.length > 0)
// A chart exists only when there is data AND at least one visible series —
// the role/label/empty-text split above keys off this, not off hasData.
const hasChart = computed(() => hasData.value && visibleKeys.value.size > 0)

// Accessible name for the canvas — lists the currently VISIBLE series so
// screen-reader users know what the picture claims to show; it tracks the
// chip toggles automatically.
const chartLabel = computed(() =>
  t('task.metrics_chart', { keys: metricKeys.value.filter(k => visibleKeys.value.has(k)).join(', ') }))

// Mid-range hues that read on BOTH light and dark surfaces (the old
// palette reused the light theme's 800-level tones — near-invisible on
// dark). From the 7th series on, colors repeat with a dash pattern so
// same-hue lines stay distinguishable.
const COLORS = ['#3B82F6', '#22C55E', '#F59E0B', '#EF4444', '#8B5CF6', '#06B6D4']

function buildDatasets() {
  return [...visibleKeys.value].map((key, i) => {
    const keyPoints = props.points.filter(p => p.key === key)
    return {
      label: key,
      // idx from map, NOT keyPoints.indexOf(p): that was O(k²) per rebuild
      data: keyPoints.map((p, idx) => ({ x: p.step ?? idx, y: p.value })),
      borderColor: COLORS[i % COLORS.length],
      backgroundColor: COLORS[i % COLORS.length] + '20',
      borderDash: i >= COLORS.length ? [6, 3] : undefined,
      borderWidth: 1.5,
      pointRadius: 0,
      tension: 0.3,
      fill: false,
    }
  })
}

function buildChart() {
  if (!canvas.value) return
  if (chart) { chart.destroy(); chart = null }
  const datasets = buildDatasets()
  if (datasets.length === 0) return

  chart = new Chart(canvas.value, {
    type: 'line',
    data: { datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 0 },
      scales: {
        x: { type: 'linear', title: { display: true, text: t('job.step'), font: { size: 11 } }, grid: { display: false } },
        // Grid/tick colors adapt to the active theme (dark grids were invisible).
        y: {
          grid: { color: theme.global.current.value.dark ? 'rgba(255,255,255,0.08)' : 'rgba(0,0,0,0.06)' },
          ticks: { color: theme.global.current.value.dark ? 'rgba(255,255,255,0.6)' : undefined },
        },
      },
      plugins: {
        legend: { position: 'top', labels: { boxWidth: 12, font: { size: 11 } } },
        tooltip: { mode: 'index', intersect: false },
      },
      interaction: { mode: 'nearest', axis: 'x', intersect: false },
    },
  })
}

// Poll updates arrive as a NEW points array every few seconds. Rebuilding
// the chart each time destroyed canvas state (and legend hover) — update
// in place when the visible series set is unchanged; rebuild only when
// the series themselves change. Empty data destroys explicitly (a stale
// chart used to linger and show the previous task's curves).
watch([() => props.points, visibleKeys], () => {
  if (props.points.length === 0 || visibleKeys.value.size === 0) {
    if (chart) { chart.destroy(); chart = null }
    return
  }
  const datasets = buildDatasets()
  if (chart && chart.data.datasets.length === datasets.length
      && chart.data.datasets.every((d, i) => d.label === datasets[i].label)) {
    chart.data.datasets.forEach((d, i) => { d.data = datasets[i].data })
    chart.update('none')
  } else {
    buildChart()
  }
})

// Theme switch: buildChart() reads grid/tick colors at build time, so an
// existing chart must be rebuilt once for the new palette. Deliberately a
// separate watcher — the points/visibleKeys one above updates in place
// and its logic stays untouched.
watch(() => theme.global.name.value, () => {
  if (chart) buildChart()
})

onMounted(() => { if (props.points.length > 0) buildChart() })
onUnmounted(() => { if (chart) chart.destroy() })
</script>

<style scoped>
.chart-frame {
  position: relative;
  height: 300px;
}
.chart-frame canvas {
  display: block;
  width: 100%;
  height: 100%;
}
.chart-empty {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
</style>
