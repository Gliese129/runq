<template>
  <div class="seg-progress" role="img" :aria-label="label" :style="{ height: `${height}px` }">
    <div
      v-for="seg in segments"
      :key="seg.key"
      :style="{ width: `${seg.pct}%`, background: seg.css }"
    />
  </div>
</template>

<script setup lang="ts">
// Segmented job progress — one bar, four truths. Colors come from
// statusGrammar (single source, U3); no palette of its own. Note the
// backend folds killed into `failed`, so the red segment covers both.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { TaskCountGroup } from '@/types/api'
import { statusStyle, TRACK_CSS } from './statusGrammar'

const { t } = useI18n()

const props = withDefaults(defineProps<{ counts: TaskCountGroup; height?: number }>(), {
  height: 4,
})

const ORDER = [
  { key: 'success', count: (c: TaskCountGroup) => c.completed },
  { key: 'failed', count: (c: TaskCountGroup) => c.failed },
  { key: 'running', count: (c: TaskCountGroup) => c.running },
  // RQ-74: outcome-unknown submissions — rendered between the live and
  // waiting buckets so a job with unknowns never reads as silently done.
  { key: 'unknown', count: (c: TaskCountGroup) => c.unknown ?? 0 },
  { key: 'pending', count: (c: TaskCountGroup) => c.pending },
] as const

// 0 tasks → no segments: the bare track renders as an empty grey bar.
const segments = computed(() => {
  const total = props.counts.total
  if (total <= 0) return []
  return ORDER.map((o) => ({
    key: o.key,
    n: o.count(props.counts),
    css: statusStyle('task', o.key).css,
  }))
    .filter((s) => s.n > 0)
    .map((s) => ({ ...s, pct: (s.n / total) * 100 }))
})

// Accessible name goes through i18n (plural-aware). The backend folds
// killed into `failed`, so the red bucket honestly reads "failed or
// killed" — the label must not claim more precision than the wire gives.
const label = computed(() => {
  const c = props.counts
  return t(
    'progress.label',
    {
      completed: c.completed,
      failed: c.failed,
      running: c.running,
      unknown: c.unknown ?? 0,
      pending: c.pending,
      total: c.total,
    },
    c.total,
  )
})

const trackCss = TRACK_CSS
</script>

<style scoped>
.seg-progress {
  display: flex;
  width: 100%;
  border-radius: 9999px;
  overflow: hidden;
  background: v-bind(trackCss);
}
.seg-progress > div {
  height: 100%;
}
</style>
