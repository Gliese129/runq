<template>
  <div class="chart-frame">
    <canvas
      ref="canvas"
      :role="hasChart ? 'img' : undefined"
      :aria-label="hasChart ? ariaLabel : undefined"
      :aria-hidden="hasChart ? undefined : 'true'"
    />
    <div v-if="!hasChart" class="chart-empty text-caption text-on-surface-variant">
      {{ t('results.no_points') }}
    </div>
  </div>
</template>

<script setup lang="ts">
// ResultsChart (RQ2-4 ②, kit ScreensC LineChart) — one series per result
// GROUP (identity key), true-x linear axis. Differs from MetricsChart
// (per-metric series of one task): comparing runs is the point here, so
// the group is the series and the metric is fixed. Kit owns the series
// palette (--project-N); Chart.js stays the app's chart medium.
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useTheme } from 'vuetify'
import { Chart, LineController, LineElement, PointElement, LinearScale, Legend, Tooltip } from 'chart.js'

Chart.register(LineController, LineElement, PointElement, LinearScale, Legend, Tooltip)

const props = defineProps<{
  /** Label already rendered through the row-label template. */
  series: { label: string; points: { x: number; y: number }[] }[]
  xTitle: string
  metric: string
}>()

const { t } = useI18n()
const theme = useTheme()
const canvas = ref<HTMLCanvasElement>()
let chart: Chart | null = null

const hasChart = computed(() => props.series.some(s => s.points.length > 0))
const ariaLabel = computed(() =>
  t('results.chart_label', { metric: props.metric, x: props.xTitle }))

// Kit's project palette. oklch() reaches canvas fine on our targets, but
// resolve through getComputedStyle with a hex fallback so a var rename
// degrades to visible lines, not invisible ones.
const FALLBACK = ['#3B82F6', '#22C55E', '#F59E0B', '#EF4444', '#8B5CF6', '#06B6D4', '#EC4899', '#84CC16']
function seriesColor(i: number): string {
  const v = getComputedStyle(document.documentElement)
    .getPropertyValue(`--project-${(i % 8) + 1}`).trim()
  return v || FALLBACK[i % FALLBACK.length]
}

function buildDatasets() {
  return props.series.map((s, i) => ({
    label: s.label,
    data: s.points,
    borderColor: seriesColor(i),
    backgroundColor: seriesColor(i),
    borderDash: i >= 8 ? [6, 3] : undefined,
    borderWidth: 1.5,
    pointRadius: s.points.length === 1 ? 3 : 0,
    tension: 0.2,
    fill: false,
  }))
}

function buildChart() {
  if (!canvas.value) return
  if (chart) { chart.destroy(); chart = null }
  if (!hasChart.value) return
  const dark = theme.global.current.value.dark
  chart = new Chart(canvas.value, {
    type: 'line',
    data: { datasets: buildDatasets() },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 0 },
      scales: {
        x: { type: 'linear', title: { display: true, text: props.xTitle, font: { size: 11 } }, grid: { display: false } },
        y: {
          title: { display: true, text: props.metric, font: { size: 11 } },
          grid: { color: dark ? 'rgba(255,255,255,0.08)' : 'rgba(0,0,0,0.06)' },
          ticks: { color: dark ? 'rgba(255,255,255,0.6)' : undefined },
        },
      },
      plugins: {
        legend: { position: 'top', labels: { boxWidth: 12, font: { size: 11 } } },
        tooltip: { mode: 'nearest', intersect: false },
      },
      interaction: { mode: 'nearest', axis: 'x', intersect: false },
    },
  })
}

// Series identity or axis change → rebuild; same shape → update in place
// (keeps legend hover state across live polls).
watch([() => props.series, () => props.xTitle, () => props.metric], () => {
  const datasets = buildDatasets()
  if (chart && hasChart.value && chart.data.datasets.length === datasets.length
      && chart.data.datasets.every((d, i) => d.label === datasets[i].label)
      && (chart.options.scales?.x as any)?.title?.text === props.xTitle
      && (chart.options.scales?.y as any)?.title?.text === props.metric) {
    chart.data.datasets.forEach((d, i) => { d.data = datasets[i].data })
    chart.update('none')
  } else {
    buildChart()
  }
})
watch(() => theme.global.name.value, () => { if (chart) buildChart() })

onMounted(buildChart)
onUnmounted(() => { if (chart) chart.destroy() })

/** PNG of the chart on an opaque surface background (transparent canvas
 *  pastes illegibly into docs). Returns false when clipboard is denied. */
async function copyPng(): Promise<boolean> {
  if (!canvas.value || !chart) return false
  const src = canvas.value
  const off = document.createElement('canvas')
  off.width = src.width
  off.height = src.height
  const ctx = off.getContext('2d')
  if (!ctx) return false
  ctx.fillStyle = theme.global.current.value.dark ? '#1E1E20' : '#FFFFFF'
  ctx.fillRect(0, 0, off.width, off.height)
  ctx.drawImage(src, 0, 0)
  try {
    const blob = await new Promise<Blob | null>(r => off.toBlob(r, 'image/png'))
    if (!blob) return false
    await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })])
    return true
  } catch {
    return false
  }
}
defineExpose({ copyPng })
</script>

<style scoped>
.chart-frame {
  position: relative;
  height: 320px;
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
