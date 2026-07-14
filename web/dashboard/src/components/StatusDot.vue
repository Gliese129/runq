<template>
  <!-- Icon channel: from 10px up the grammar icon IS the indicator, so the
       amber-shared trio (killed/paused/partial) reads by shape, not color.
       Below 10px a glyph is illegible — keep the plain dot; call sites that
       rely on the shape channel must pass size >= 10 (list rows use 14). -->
  <svg
    v-if="iconMode"
    class="status-glyph"
    :class="{ 'icon-spin': s.animated }"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    role="img"
    :aria-label="label"
  >
    <title>{{ label }}</title>
    <path :d="iconPath" :fill="s.css" />
  </svg>
  <span
    v-else
    class="status-dot"
    :class="{ 'status-dot--pulse': s.animated }"
    :style="dotStyle"
    :title="label"
    role="img"
    :aria-label="label"
  />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { statusStyle, type StatusKind } from './statusGrammar'
import { MDI_PATHS } from '@/plugins/mdiPaths'

const props = withDefaults(defineProps<{
  status: string
  kind?: StatusKind
  size?: number
}>(), { kind: 'task', size: 8 })

const { t, te } = useI18n()

const s = computed(() => statusStyle(props.kind, props.status))

// Localized accessible name — raw enum tokens would leak English to
// zh-CN/ja screen-reader users. Unknown statuses fall back to the token.
const label = computed(() => {
  const key = `status.${props.kind}.${props.status}`
  return te(key) ? t(key) : props.status
})

const iconPath = computed(() => MDI_PATHS[s.value.icon] ?? '')
const iconMode = computed(() => props.size >= 10 && iconPath.value !== '')

const dotStyle = computed(() => ({
  width: `${props.size}px`,
  height: `${props.size}px`,
  ...(s.value.hollow
    ? { background: 'transparent', border: `2px solid ${s.value.css}` }
    : { background: s.value.css }),
}))
</script>

<style scoped>
.status-glyph {
  display: inline-block;
  flex-shrink: 0;
  vertical-align: middle;
}
</style>
