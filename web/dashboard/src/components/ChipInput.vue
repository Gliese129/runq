<template>
  <div>
    <div class="text-caption text-on-surface-variant mb-1">{{ label }}</div>
    <div
      class="d-flex flex-wrap align-center ga-1 pa-2 rounded-lg"
      style="border: 1px solid rgba(0,0,0,0.12); min-height: 40px; cursor: text"
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
    <div v-if="modelValue.length > 1" class="text-caption text-on-surface-variant mt-1">
      {{ modelValue.length }} {{ t('submit.values') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = withDefaults(defineProps<{
  modelValue: string[]
  label?: string
  placeholder?: string
  color?: string
}>(), {
  label: '',
  placeholder: 'Type and press Enter',
  color: 'primary',
})

const emit = defineEmits<{
  'update:modelValue': [vals: string[]]
}>()

const draft = ref('')
const input = ref<HTMLInputElement>()

function focusInput() {
  input.value?.focus()
}

function add() {
  const val = draft.value.trim()
  if (!val) return
  // Support comma-separated pasting
  const parts = val.split(',').map(v => v.trim()).filter(Boolean)
  emit('update:modelValue', [...props.modelValue, ...parts])
  draft.value = ''
}

function remove(i: number) {
  const next = [...props.modelValue]
  next.splice(i, 1)
  emit('update:modelValue', next)
}

function onBackspace() {
  if (draft.value === '' && props.modelValue.length > 0) {
    remove(props.modelValue.length - 1)
  }
}

function onPaste(e: ClipboardEvent) {
  const text = e.clipboardData?.getData('text') || ''
  if (text.includes(',') || text.includes('\n')) {
    e.preventDefault()
    const parts = text.split(/[,\n]/).map(v => v.trim()).filter(Boolean)
    emit('update:modelValue', [...props.modelValue, ...parts])
    draft.value = ''
  }
}
</script>

<style scoped>
.chip-input-field {
  border: none;
  outline: none;
  background: transparent;
  font-size: 0.875rem;
  min-width: 80px;
  flex: 1;
}
</style>
