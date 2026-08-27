<template>
  <div>
    <!-- Caption row: what the curve is + live hover readout -->
    <div class="d-flex align-baseline ga-2 mb-1">
      <span class="text-caption text-on-surface-variant text-no-wrap">{{ t('activity.title') }}</span>
      <span class="text-caption text-on-surface-variant font-mono text-no-wrap">{{ t('activity.lines_unit', { unit: unitLabel }) }}</span>
      <v-spacer />
      <span class="text-caption text-on-surface-variant font-mono text-no-wrap readout">
        {{ hoverText }}
      </span>
      <span
        v-if="zoomed"
        class="text-caption text-primary cursor-pointer text-no-wrap"
        role="button" tabindex="0"
        @click="win = null"
        @keydown.enter="win = null"
      >{{ t('activity.reset_zoom') }}</span>
    </div>

    <div class="d-flex align-stretch">
      <!-- Y axis labels -->
      <div class="y-axis font-mono" :style="{ height: H + 'px' }">
        <span
          v-for="v in axis.ticks" :key="v"
          class="y-tick"
          :style="{ top: (yOf(v) / H) * 100 + '%' }"
        >{{ v }}</span>
      </div>
      <!-- Plot: drag = zoom brush, click = seek, wheel = scale -->
      <div
        ref="wrap"
        class="plot-wrap"
        @mousemove="onMove"
        @mouseleave="hoverAt = -1; drag = null"
        @mousedown="onDown"
        @mouseup="onUp"
        @wheel.prevent="onWheel"
      >
        <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" class="plot-svg" :style="{ height: H + 'px' }">
          <defs>
            <linearGradient id="rq-act-grad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="rgb(var(--v-theme-primary))" stop-opacity="0.55" />
              <stop offset="100%" stop-color="rgb(var(--v-theme-primary))" stop-opacity="0.06" />
            </linearGradient>
          </defs>
          <line
            v-for="v in axis.ticks" :key="'g' + v"
            x1="0" :x2="W" :y1="yOf(v)" :y2="yOf(v)"
            stroke="rgb(var(--v-theme-outline-variant))" stroke-width="0.5" vector-effect="non-scaling-stroke"
          />
          <path :d="areaPath" fill="url(#rq-act-grad)" />
          <path :d="linePath" fill="none" stroke="rgb(var(--v-theme-primary))" stroke-width="1.5" vector-effect="non-scaling-stroke" />
          <rect
            v-if="drag && Math.abs(drag.b - drag.a) >= step"
            :x="xOf(slotOf(Math.min(drag.a, drag.b)))" y="0"
            :width="Math.max(2, Math.abs(xOf(slotOf(drag.b)) - xOf(slotOf(drag.a))))" :height="H"
            class="brush-rect" stroke-width="1" vector-effect="non-scaling-stroke"
          />
          <line
            v-if="seekX !== null"
            :x1="seekX" :x2="seekX" y1="0" :y2="H"
            stroke="rgb(var(--v-theme-primary))" stroke-width="2" vector-effect="non-scaling-stroke"
          />
          <line
            v-if="hover"
            :x1="hoverX" :x2="hoverX" y1="0" :y2="H"
            stroke="rgb(var(--v-theme-on-surface-variant))" stroke-width="1" stroke-dasharray="3 3" vector-effect="non-scaling-stroke"
          />
        </svg>
      </div>
    </div>

    <!-- Time axis -->
    <div class="x-axis d-flex justify-space-between font-mono">
      <span v-for="(ti, k) in xTicks" :key="k" class="text-no-wrap">{{ hhmm(cells[ti]?.ts) }}</span>
    </div>
    <!-- Playhead pill: reserved band so no seek position covers a tick -->
    <div class="seek-band">
      <span v-if="seekX !== null && seekCell" class="seek-pill font-mono" :style="{ left: (seekX / W) * 100 + '%' }">
        {{ hhmm(seekCell.ts) }}
      </span>
    </div>
    <div class="text-caption text-on-surface-variant mt-1">
      {{ t('activity.hint') }}<template v-if="step > 1"> — {{ t('activity.samples_per_point', { n: step }) }}</template>
    </div>
  </div>
</template>

<script setup lang="ts">
// TaskActivityCurve (RQ2-4 ①, kit ScreensTask ActivityCurve) — log
// density over the run, the way a video player shows comment density.
// People scan for SHAPE (ramp, plateau, the hole where it stalled) and
// jump there: click = seek (emits the sample's cumulative byte/line
// position), drag = zoom, wheel = scale. Resolution is capped by
// activity.tsv's 60s sampling; bucket math lives in activityMath.ts.
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ActivityPoint } from '@/types/api'
import { toCells, pickStep, bucketize, niceAxis, type ActivityCell } from './activityMath'

const props = defineProps<{
  points: ActivityPoint[]
  /** Minutes per point after owning-side decimation (1 = raw). */
  bucketMinutes?: number
  /** Seeked sample index (into points), -1 = none. */
  seekAt: number
}>()

const emit = defineEmits<{ seek: [cell: ActivityCell & { index: number }] }>()

const { t, locale } = useI18n()
const W = 900
const H = 96

const wrap = ref<HTMLElement>()
const win = ref<[number, number] | null>(null)
const hoverAt = ref(-1)
const drag = ref<{ a: number; b: number } | null>(null)

const cells = computed(() => toCells(props.points))
const i0 = computed(() => win.value?.[0] ?? 0)
const i1 = computed(() => win.value?.[1] ?? cells.value.length - 1)
const zoomed = computed(() => !!win.value && i1.value - i0.value < cells.value.length - 1)

const step = computed(() => pickStep(i1.value - i0.value + 1, W))
const view = computed(() => bucketize(cells.value, i0.value, i1.value, step.value))

// Minutes per plotted point = owning-side decimation × client bucketing.
const unitLabel = computed(() => {
  const mins = (props.bucketMinutes || 1) * step.value
  return mins === 1 ? '/min' : `/${mins} min`
})

const axis = computed(() => niceAxis(Math.max(1, ...view.value.map(c => c.lines))))

function xOf(k: number): number {
  return view.value.length < 2 ? 0 : (k / (view.value.length - 1)) * W
}
function yOf(n: number): number {
  return H - (n / axis.value.axisMax) * (H - 6)
}

const areaPath = computed(() => {
  if (!view.value.length) return ''
  const pts = view.value.map((c, k) => `${xOf(k).toFixed(1)},${yOf(c.lines).toFixed(1)}`)
  return `M0,${H} L` + pts.join(' L') + ` L${W},${H} Z`
})
const linePath = computed(() =>
  view.value.map((c, k) => `${k ? 'L' : 'M'}${xOf(k).toFixed(1)},${yOf(c.lines).toFixed(1)}`).join(' '))

function hhmm(ts?: number): string {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleTimeString(locale.value, { hour: '2-digit', minute: '2-digit' })
}
function kb(b: number): string {
  return b > 1024 ? (b / 1024).toFixed(1) + ' KB' : b + ' B'
}

// Screen x → sample index, via the bucket under the cursor.
function idxAt(clientX: number): number {
  const r = wrap.value?.getBoundingClientRect()
  if (!r) return i0.value
  const f = Math.min(1, Math.max(0, (clientX - r.left) / r.width))
  const k = Math.round(f * (view.value.length - 1))
  return view.value[k]?.idx ?? i0.value
}
// Sample index → bucket slot, for drawing the cursors.
function slotOf(i: number): number {
  if (step.value === 1) return i - i0.value
  return Math.min(view.value.length - 1, Math.max(0, Math.floor((i - i0.value) / step.value)))
}

function onMove(e: MouseEvent) {
  const i = idxAt(e.clientX)
  hoverAt.value = i
  if (drag.value) drag.value = { ...drag.value, b: i }
}
function onDown(e: MouseEvent) {
  const i = idxAt(e.clientX)
  drag.value = { a: i, b: i }
}
function onUp() {
  if (!drag.value) return
  const [a, b] = [Math.min(drag.value.a, drag.value.b), Math.max(drag.value.a, drag.value.b)]
  if (b - a >= 2) {
    win.value = [a, b]
  } else {
    // Click = seek: hand the caller the exact cumulative position.
    const cell = cells.value[a]
    if (cell) emit('seek', { ...cell, index: a })
  }
  drag.value = null
}
function onWheel(e: WheelEvent) {
  const at = idxAt(e.clientX)
  const span = i1.value - i0.value
  const next = Math.max(4, Math.round(span * (e.deltaY > 0 ? 1.35 : 0.74)))
  let a = Math.max(0, at - Math.round(((at - i0.value) / Math.max(1, span)) * next))
  const b = Math.min(cells.value.length - 1, a + next)
  a = Math.max(0, b - next)
  win.value = b - a >= cells.value.length - 1 ? null : [a, b]
}

const hover = computed(() =>
  hoverAt.value >= i0.value && hoverAt.value <= i1.value ? view.value[slotOf(hoverAt.value)] : null)
const hoverX = computed(() => (hover.value ? xOf(slotOf(hoverAt.value)) : 0))
const hoverText = computed(() =>
  hover.value ? `${hhmm(hover.value.ts)} · ${hover.value.lines} ${t('activity.lines_word')}${unitLabel.value} · ${kb(hover.value.bytes)}` : '')

const seekX = computed(() =>
  props.seekAt >= i0.value && props.seekAt <= i1.value ? xOf(slotOf(props.seekAt)) : null)
const seekCell = computed(() => cells.value[props.seekAt])

const xTicks = computed(() =>
  [0, 0.25, 0.5, 0.75, 1].map(f => i0.value + Math.round(f * (i1.value - i0.value))))
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.text-no-wrap { white-space: nowrap; }
.readout { min-height: 14px; flex-shrink: 0; }
.y-axis {
  position: relative;
  width: 34px;
  flex-shrink: 0;
  font-size: 10px;
  color: rgb(var(--v-theme-on-surface-variant));
}
.y-tick {
  position: absolute;
  right: 5px;
  transform: translateY(-50%);
  white-space: nowrap;
}
.plot-wrap {
  position: relative;
  flex: 1;
  min-width: 0;
  cursor: crosshair;
  user-select: none;
}
.plot-svg { display: block; width: 100%; }
.brush-rect {
  fill: rgb(var(--v-theme-primary), 0.14);
  stroke: rgb(var(--v-theme-primary));
}
.x-axis {
  margin-top: 6px;
  margin-left: 34px;
  font-size: 10.5px;
  color: rgb(var(--v-theme-on-surface-variant));
}
.seek-band {
  position: relative;
  height: 15px;
  margin-left: 34px;
}
.seek-pill {
  position: absolute;
  top: 0;
  transform: translateX(-50%);
  pointer-events: none;
  padding: 1px 5px;
  border-radius: 4px;
  background: rgb(var(--v-theme-primary));
  color: #fff;
  font-size: 10px;
  white-space: nowrap;
}
</style>
