<template>
  <div>
    <div v-if="metricKeys.length > 1" class="d-flex flex-wrap ga-1 mb-2">
      <v-chip
        v-for="key in metricKeys" :key="key"
        size="x-small"
        :variant="visibleKeys.has(key) ? 'flat' : 'outlined'"
        :color="visibleKeys.has(key) ? 'primary' : undefined"
        @click="toggleKey(key)"
      >{{ key }}</v-chip>
    </div>
    <canvas ref="canvas" style="width: 100%; max-height: 300px" />
    <div v-if="points.length === 0" class="text-center text-caption text-on-surface-variant pa-4">
      No metrics recorded yet
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
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

const canvas = ref<HTMLCanvasElement>()
let chart: Chart | null = null

const metricKeys = computed(() => {
  const keys = new Set<string>()
  for (const p of props.points) keys.add(p.key)
  return [...keys].sort()
})

const visibleKeys = ref(new Set<string>())

// Auto-show first 3 keys
watch(metricKeys, (keys) => {
  if (visibleKeys.value.size === 0 && keys.length > 0) {
    for (const k of keys.slice(0, 3)) visibleKeys.value.add(k)
  }
}, { immediate: true })

function toggleKey(key: string) {
  const s = new Set(visibleKeys.value)
  if (s.has(key)) s.delete(key)
  else s.add(key)
  visibleKeys.value = s
}

const COLORS = ['#1E40AF', '#16A34A', '#D97706', '#DC2626', '#7C3AED', '#0891B2']

function buildChart() {
  if (!canvas.value || props.points.length === 0) return

  const datasets = [...visibleKeys.value].map((key, i) => {
    const keyPoints = props.points.filter(p => p.key === key)
    return {
      label: key,
      data: keyPoints.map(p => ({ x: p.step ?? keyPoints.indexOf(p), y: p.value })),
      borderColor: COLORS[i % COLORS.length],
      backgroundColor: COLORS[i % COLORS.length] + '20',
      borderWidth: 1.5,
      pointRadius: 0,
      tension: 0.3,
      fill: false,
    }
  })

  if (chart) { chart.destroy(); chart = null }
  if (datasets.length === 0) return

  chart = new Chart(canvas.value, {
    type: 'line',
    data: { datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 0 },
      scales: {
        x: { type: 'linear', title: { display: true, text: 'Step', font: { size: 11 } }, grid: { display: false } },
        y: { grid: { color: 'rgba(0,0,0,0.06)' } },
      },
      plugins: {
        legend: { position: 'top', labels: { boxWidth: 12, font: { size: 11 } } },
        tooltip: { mode: 'index', intersect: false },
      },
      interaction: { mode: 'nearest', axis: 'x', intersect: false },
    },
  })
}

watch([() => props.points, visibleKeys], () => buildChart(), { deep: true })

onMounted(() => { if (props.points.length > 0) buildChart() })
onUnmounted(() => { if (chart) chart.destroy() })
</script>
