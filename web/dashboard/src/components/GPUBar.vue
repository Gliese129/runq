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
import type { GPUSlot } from '@/types/api'

const props = defineProps<{ slot: GPUSlot }>()

// Neutral-to-warning scale: normal utilization stays a calm grey-blue —
// a busy lab GPU is not an alarm. Only near-saturation (>80%) escalates,
// so a full cluster no longer renders as a wall of red/amber.
const barColor = computed(() => {
  if (props.slot.util_percent > 80) return 'warning'
  return 'blue-grey'
})
</script>
