<template>
  <div>
    <div v-if="label" class="text-caption text-on-surface-variant mb-1">{{ label }}</div>
    <div
      class="d-flex flex-wrap align-center ga-1 pa-2 rounded chip-input-box"
      @click="focusInput"
    >
      <v-chip
        v-for="(val, i) in modelValue"
        :key="i"
        size="small"
        closable
        variant="tonal"
        :color="color"
        @click:close="remove(i)"
      >
        {{ val }}
      </v-chip>
      <input
        ref="input"
        v-model="draft"
        :placeholder="modelValue.length === 0 ? placeholder : ''"
        class="chip-input-field"
        @keydown.enter.prevent="add"
        @keydown.tab.prevent="add"
        @keydown.delete="onBackspace"
        @paste="onPaste"
      />
    </div>
    <div class="d-flex align-center justify-space-between mt-1" style="min-height: 16px">
      <span v-if="coerceWarning" class="text-caption text-warning">{{ coerceWarning }}</span>
      <span v-else />
      <span v-if="modelValue.length > 1" class="text-caption text-on-surface-variant">
        {{ modelValue.length }} values
      </span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'

const props = withDefaults(defineProps<{
  modelValue: string[]
  label?: string
  placeholder?: string
  color?: string
  paramType?: string
}>(), {
  label: '',
  placeholder: 'Type and press Enter',
  color: 'primary',
  paramType: '',
})

const emit = defineEmits<{
  'update:modelValue': [vals: string[]]
}>()

const draft = ref('')
const input = ref<HTMLInputElement>()
const coerceWarning = ref('')

const SPLIT_RE = /[,;\s]+/

/**
 * Coerce a single string value to the target type.
 * Returns null if the value cannot be converted (should be dropped).
 */
function coerceValue(raw: string, type: string): string | null {
  const t = type.toLowerCase()
  const trimmed = raw.trim()
  if (!trimmed) return null

  if (t === 'int') {
    const n = Number(trimmed)
    if (isNaN(n)) return null
    return String(Math.trunc(n)) // 53.2 → 53, "62" → 62
  }
  if (t === 'float') {
    const n = Number(trimmed)
    if (isNaN(n)) return null
    return String(n) // .1 → 0.1
  }
  if (t === 'bool') {
    const low = trimmed.toLowerCase()
    if (['true', '1', 'yes'].includes(low)) return 'true'
    if (['false', '0', 'no'].includes(low)) return 'false'
    return null
  }
  return trimmed
}

/**
 * Coerce an array of values. Returns { values, dropped } count.
 */
function coerceAll(vals: string[], type: string): { values: string[]; dropped: number } {
  if (!type) return { values: vals, dropped: 0 }
  let dropped = 0
  const values: string[] = []
  for (const v of vals) {
    const c = coerceValue(v, type)
    if (c !== null) values.push(c)
    else dropped++
  }
  return { values, dropped }
}

// Expose for parent (used on collapse)
defineExpose({ coerceAll })

function focusInput() {
  input.value?.focus()
}

function splitAndClean(raw: string): string[] {
  return raw.split(SPLIT_RE).map(v => v.trim()).filter(Boolean)
}

function add() {
  const val = draft.value.trim()
  if (!val) return
  const parts = splitAndClean(val)
  const { values, dropped } = coerceAll(parts, props.paramType)
  if (values.length > 0) {
    emit('update:modelValue', [...props.modelValue, ...values])
  }
  coerceWarning.value = dropped > 0 ? `${dropped} invalid value${dropped > 1 ? 's' : ''} ignored` : ''
  draft.value = ''
}

function remove(i: number) {
  const next = [...props.modelValue]
  next.splice(i, 1)
  emit('update:modelValue', next)
  coerceWarning.value = ''
}

function onBackspace() {
  if (draft.value === '' && props.modelValue.length > 0) {
    remove(props.modelValue.length - 1)
  }
}

function onPaste(e: ClipboardEvent) {
  e.preventDefault()
  const text = e.clipboardData?.getData('text') || ''
  const parts = text.split(/[,;\s\n]+/).map(v => v.trim()).filter(Boolean)
  const { values, dropped } = coerceAll(parts, props.paramType)
  if (values.length > 0) {
    emit('update:modelValue', [...props.modelValue, ...values])
  }
  coerceWarning.value = dropped > 0 ? `${dropped} invalid value${dropped > 1 ? 's' : ''} ignored` : ''
  draft.value = ''
}
</script>

<style scoped>
.chip-input-box {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  min-height: 36px;
  cursor: text;
  transition: border-color 0.15s ease;
}
.chip-input-box:focus-within {
  border-color: rgb(var(--v-theme-primary));
}
.chip-input-field {
  border: none;
  outline: none;
  background: transparent;
  font-size: 0.875rem;
  min-width: 80px;
  flex: 1;
}
</style>
