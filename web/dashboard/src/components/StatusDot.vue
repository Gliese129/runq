<template>
  <span
    class="status-dot"
    :class="{ 'status-dot--pulse': s.animated }"
    :style="dotStyle"
    :title="status"
  />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { statusStyle, type StatusKind } from './statusGrammar'

const props = withDefaults(defineProps<{
  status: string
  kind?: StatusKind
  size?: number
}>(), { kind: 'task', size: 8 })

const s = computed(() => statusStyle(props.kind, props.status))

const dotStyle = computed(() => ({
  width: `${props.size}px`,
  height: `${props.size}px`,
  ...(s.value.hollow
    ? { background: 'transparent', border: `2px solid ${s.value.css}` }
    : { background: s.value.css }),
}))
</script>
