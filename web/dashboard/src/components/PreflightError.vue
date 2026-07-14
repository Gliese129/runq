<template>
  <v-card variant="tonal" color="error" class="pa-3">
    <div class="d-flex align-center justify-space-between mb-2">
      <div class="d-flex align-center ga-2">
        <v-icon size="18">mdi-alert-circle-outline</v-icon>
        <span class="text-body-2 font-weight-medium">{{ t('submit.preflight_failed') }}</span>
      </div>
      <div class="d-flex ga-1">
        <v-btn size="x-small" variant="tonal" @click="$emit('skip-preflight')">
          <v-icon start size="12">mdi-skip-next</v-icon> {{ t('submit.submit_anyway') }}
        </v-btn>
        <v-btn v-if="closable" size="x-small" icon variant="text" :aria-label="t('common.close')" @click="$emit('close')">
          <v-icon size="14">mdi-close</v-icon>
        </v-btn>
      </div>
    </div>

    <!-- Grouped findings -->
    <div v-for="group in groups" :key="group.category" class="mb-2">
      <div class="d-flex align-center ga-1 mb-1">
        <v-icon size="14" :color="group.color">{{ group.icon }}</v-icon>
        <span class="text-caption font-weight-medium">{{ group.label }}</span>
        <v-chip size="x-small" variant="outlined">{{ group.items.length }}</v-chip>
      </div>
      <div v-for="(item, i) in group.items" :key="i" class="finding-row d-flex align-center ga-1 pl-5">
        <code class="text-caption flex-grow-1" style="word-break: break-word">{{ item }}</code>
      </div>
    </div>

    <!-- Raw fallback for lines we couldn't parse -->
    <div v-if="unparsed.length > 0" class="mt-2">
      <div v-for="(line, i) in unparsed" :key="i" class="text-caption pl-2" style="font-family: monospace">{{ line }}</div>
    </div>
  </v-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = defineProps<{
  message: string
  closable?: boolean
}>()

defineEmits<{
  'skip-preflight': []
  'close': []
}>()

interface FindingGroup {
  category: string
  label: string
  icon: string
  color: string
  items: string[]
}

// Labels are i18n keys — the category TOKENS come from the backend, but
// the human-readable group titles are frontend copy and must localize.
const CATEGORY_META: Record<string, { labelKey: string; icon: string; color: string }> = {
  import: { labelKey: 'preflight.cat_import', icon: 'mdi-package-variant-remove', color: 'error' },
  pip_check: { labelKey: 'preflight.cat_pip_check', icon: 'mdi-package-variant', color: 'warning' },
  env: { labelKey: 'preflight.cat_env', icon: 'mdi-console', color: 'error' },
  file: { labelKey: 'preflight.cat_file', icon: 'mdi-file-alert-outline', color: 'warning' },
}

const parsed = computed(() => {
  const groups: Record<string, string[]> = {}
  const unparsed: string[] = []

  for (const line of props.message.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    // Skip the "Run with --no-preflight" hint — we have the button
    if (trimmed.startsWith('Run with --no-preflight')) continue
    if (trimmed.startsWith('preflight failed:')) continue

    const match = trimmed.match(/^-\s*(\w+):\s*(.+)$/)
    if (match) {
      const [, category, msg] = match
      if (!groups[category]) groups[category] = []
      groups[category].push(msg)
    } else {
      unparsed.push(trimmed)
    }
  }

  return { groups, unparsed }
})

const groups = computed<FindingGroup[]>(() => {
  const result: FindingGroup[] = []
  for (const [cat, items] of Object.entries(parsed.value.groups)) {
    const meta = CATEGORY_META[cat]
    result.push({
      category: cat,
      // Unknown categories fall back to the raw backend token.
      label: meta ? t(meta.labelKey) : cat,
      icon: meta?.icon ?? 'mdi-alert-outline',
      color: meta?.color ?? 'warning',
      items,
    })
  }
  // Sort: errors first, then warnings
  return result.sort((a, b) => (a.color === 'error' ? 0 : 1) - (b.color === 'error' ? 0 : 1))
})

const unparsed = computed(() => parsed.value.unparsed)
</script>

<style scoped>
.finding-row {
  padding: 2px 0;
  border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant), 0.2);
}
.finding-row:last-child { border-bottom: none; }
</style>
