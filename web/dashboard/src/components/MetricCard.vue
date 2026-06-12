<template>
  <v-card
    class="pa-4 cursor-pointer"
    :class="{ 'border-primary': active }"
    hover
    @click="$emit('click')"
  >
    <div class="d-flex align-center justify-space-between">
      <div>
        <div class="text-caption text-on-surface-variant mb-1">{{ label }}</div>
        <div class="text-h4 font-weight-bold" :style="{ color: `rgb(var(--v-theme-${color}))` }">
          {{ displayValue }}
        </div>
      </div>
      <div
        class="d-flex align-center justify-center rounded-xl"
        :style="{
          width: '44px',
          height: '44px',
          background: `rgba(var(--v-theme-${color}), 0.1)`,
        }"
      >
        <v-icon :color="color" size="22">{{ icon }}</v-icon>
      </div>
    </div>
    <div class="text-caption text-on-surface-variant mt-1" :style="{ visibility: subtitle ? 'visible' : 'hidden' }">
      {{ subtitle || ' ' }}
    </div>
  </v-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  label: string
  value: number | string
  icon: string
  color: string
  subtitle?: string
  active?: boolean
}>()

defineEmits<{ click: [] }>()

const displayValue = computed(() => {
  if (typeof props.value === 'number' && props.value === 0) return '0'
  return String(props.value)
})
</script>
