<template>
  <div class="d-flex align-center ga-1" style="min-width: 120px">
    <span class="text-caption text-medium-emphasis text-no-wrap">{{ slot.name || `GPU ${slot.index}` }}</span>
    <v-progress-linear
      :model-value="slot.util_percent"
      :color="barColor"
      height="6"
      rounded
      class="flex-grow-1"
    />
    <span class="text-caption" style="min-width: 30px; text-align: right">{{ slot.util_percent }}%</span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { GPUSlot } from '../api/types'

const props = defineProps<{ slot: GPUSlot }>()

const barColor = computed(() => {
  if (props.slot.util_percent > 80) return 'error'
  if (props.slot.util_percent > 40) return 'warning'
  return 'success'
})
</script>
