<template>
  <v-chip :color="statusColor" size="small" variant="tonal">
    {{ label }}
  </v-chip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ status: string }>()
const { t } = useI18n()

const statusColor = computed(() => {
  switch (props.status) {
    case 'running': return 'warning'
    case 'done': case 'completed': return 'success'
    case 'failed': return 'error'
    case 'killed': return 'error'
    case 'pending': return 'default'
    default: return 'default'
  }
})

const label = computed(() => {
  const key = `job.status.${props.status}`
  const translated = t(key)
  // fallback to raw status if no translation
  return translated === key ? props.status : translated
})
</script>
