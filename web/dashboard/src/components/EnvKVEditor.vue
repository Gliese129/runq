<!--
  EnvKVEditor — key/value row editor over a "KEY=VALUE per line" string.

  The string (envText) stays the single source of truth in submit state —
  draft cache, parseEnvText and YAML import all keep working unchanged;
  this component is just a friendlier lens over it. A blank trailing row
  is always present so adding a variable never needs an explicit button.
-->
<template>
  <div class="mb-3">
    <div class="text-caption text-on-surface-variant mb-1">{{ labelText }}</div>
    <div
      v-for="(row, i) in rows"
      :key="i"
      class="d-flex align-center ga-2 mb-1"
    >
      <v-text-field
        v-model="row.key"
        class="font-mono env-key"
        variant="outlined" density="compact" hide-details
        :placeholder="i === rows.length - 1 ? keyPlaceholder : ''"
        autocapitalize="off" autocorrect="off" spellcheck="false"
        @update:model-value="onEdit"
      />
      <span class="text-on-surface-variant flex-shrink-0">=</span>
      <v-text-field
        v-model="row.value"
        class="font-mono env-value"
        variant="outlined" density="compact" hide-details
        :placeholder="i === rows.length - 1 ? valuePlaceholder : ''"
        autocapitalize="off" autocorrect="off" spellcheck="false"
        @update:model-value="onEdit"
      />
      <v-btn
        icon size="x-small" variant="text"
        :style="{ visibility: isBlankRow(row) ? 'hidden' : 'visible' }"
        :aria-label="t('common.remove_item', { name: row.key || '' })"
        :title="t('common.remove_item', { name: row.key || '' })"
        @click="removeRow(i)"
      >
        <v-icon size="14" color="on-surface-variant">mdi-close</v-icon>
      </v-btn>
    </div>
    <div v-if="hint" class="text-caption text-on-surface-variant mt-1">{{ hint }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'

interface EnvRow { key: string; value: string }

const props = withDefaults(defineProps<{
  /** "KEY=VALUE per line" text — the persisted form */
  modelValue: string
  label?: string
  hint?: string
  keyPlaceholder?: string
  valuePlaceholder?: string
}>(), {
  label: '',
  hint: '',
  keyPlaceholder: 'TSUBAME_GROUP',
  valuePlaceholder: 'tga-xxx',
})

const { t } = useI18n()
const labelText = computed(() => props.label || t('submit.environment'))

const emit = defineEmits<{ 'update:modelValue': [string] }>()

const rows = reactive<EnvRow[]>([])

function isBlankRow(r: EnvRow): boolean {
  return !r.key.trim() && !r.value.trim()
}

function parse(text: string): EnvRow[] {
  const out: EnvRow[] = []
  for (const line of (text || '').split('\n')) {
    const l = line.trim()
    if (!l || l.startsWith('#')) continue
    const i = l.indexOf('=')
    if (i <= 0) { out.push({ key: l, value: '' }); continue }
    out.push({ key: l.slice(0, i).trim(), value: l.slice(i + 1).trim() })
  }
  return out
}

function serialize(): string {
  return rows
    .filter(r => r.key.trim())
    .map(r => `${r.key.trim()}=${r.value.trim()}`)
    .join('\n')
}

function ensureTrailingBlank() {
  if (rows.length === 0 || !isBlankRow(rows[rows.length - 1])) {
    rows.push({ key: '', value: '' })
  }
}

// External writes (project selection, YAML import, draft restore) reset the
// rows — but only on real content change, so typing isn't disrupted by the
// round-trip through the parent.
watch(() => props.modelValue, (text) => {
  if ((text || '') === serialize()) return
  rows.splice(0, rows.length, ...parse(text))
  ensureTrailingBlank()
}, { immediate: true })

function onEdit() {
  ensureTrailingBlank()
  emit('update:modelValue', serialize())
}

function removeRow(i: number) {
  rows.splice(i, 1)
  ensureTrailingBlank()
  emit('update:modelValue', serialize())
}
</script>

<style scoped>
.env-key { max-width: 220px; }
.env-value { flex: 1; }
</style>
