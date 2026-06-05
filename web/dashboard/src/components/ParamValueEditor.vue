<template>
  <!-- bool: two toggle chips -->
  <div v-if="type === 'bool'" class="d-flex ga-2">
    <v-chip
      :color="hasValue('true') ? color : undefined"
      :variant="hasValue('true') ? 'flat' : 'outlined'"
      size="small"
      @click="toggleValue('true')"
    >true</v-chip>
    <v-chip
      :color="hasValue('false') ? color : undefined"
      :variant="hasValue('false') ? 'flat' : 'outlined'"
      size="small"
      @click="toggleValue('false')"
    >false</v-chip>
  </div>

  <!-- str / file / folder: multi-select combobox with custom values -->
  <div v-else-if="type === 'str' || type === 'file' || type === 'folder'">
    <v-combobox
      :model-value="modelValue"
      @update:model-value="onComboUpdate"
      :items="suggestions"
      multiple chips closable-chips
      density="compact" variant="outlined" hide-details
      :placeholder="modelValue.length === 0 ? (type === 'str' ? 'Type or select values' : 'Type or select paths') : ''"
      :color="color"
      style="font-family: monospace; font-size: 13px"
    >
      <template #chip="{ item, props: chipProps }">
        <v-chip v-bind="chipProps" size="small" variant="tonal" :color="color">
          {{ type !== 'str' ? (item.title.split('/').pop() || item.title) : item.title }}
          <v-tooltip v-if="type !== 'str'" activator="parent" location="top">{{ item.title }}</v-tooltip>
        </v-chip>
      </template>
    </v-combobox>
    <div v-if="type !== 'str' && invalidPaths.length > 0" class="text-caption text-warning mt-1">
      {{ invalidPaths.length }} value(s) don't look like paths
    </div>
  </div>

  <!-- int/float: ChipInput + range generator -->
  <div v-else-if="type === 'int' || type === 'float'">
    <ChipInput
      :model-value="modelValue"
      @update:model-value="$emit('update:modelValue', $event)"
      :placeholder="placeholder"
      :color="color"
      :param-type="type"
    />
    <div class="d-flex align-center ga-2 mt-1">
      <v-text-field
        v-model.number="rangeMin" placeholder="min"
        density="compact" variant="underlined" hide-details
        type="number" style="max-width: 70px; font-size: 12px"
      />
      <v-text-field
        v-model.number="rangeMax" placeholder="max"
        density="compact" variant="underlined" hide-details
        type="number" style="max-width: 70px; font-size: 12px"
      />
      <v-text-field
        v-model.number="rangeStep" placeholder="step"
        density="compact" variant="underlined" hide-details
        type="number" style="max-width: 70px; font-size: 12px"
      />
      <v-btn
        size="x-small" variant="tonal" color="primary"
        :disabled="rangeMin == null || rangeMax == null || rangeStep == null || rangeStep <= 0 || (rangeMin != null && rangeMax != null && rangeMin >= rangeMax)"
        @click="generateRange"
      >
        <v-icon size="12" start>mdi-auto-fix</v-icon>
        Range
      </v-btn>
    </div>
    <div v-if="rangeTruncated" class="text-caption text-warning mt-1">
      Capped at {{ MAX_RANGE }} values — adjust step size for finer granularity
    </div>
  </div>

  <!-- list / fallback: plain ChipInput -->
  <ChipInput
    v-else
    :model-value="modelValue"
    @update:model-value="$emit('update:modelValue', $event)"
    :placeholder="placeholder"
    :color="color"
    :param-type="type"
  />
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import ChipInput from './ChipInput.vue'

const props = withDefaults(defineProps<{
  modelValue: string[]
  type: string
  placeholder?: string
  color?: string
  min?: number
  max?: number
  step?: number
  suggestions?: string[]  // pre-defined values from project param definition
}>(), {
  placeholder: 'Type value + Enter',
  color: 'primary',
  suggestions: () => [],
})

const emit = defineEmits<{
  'update:modelValue': [values: string[]]
}>()

function hasValue(v: string) {
  return props.modelValue.includes(v)
}

function toggleValue(v: string) {
  const vals = [...props.modelValue]
  const idx = vals.indexOf(v)
  if (idx >= 0) vals.splice(idx, 1)
  else vals.push(v)
  emit('update:modelValue', vals)
}

/** v-combobox emits mixed types (string | object) — normalize to string[] */
function onComboUpdate(val: any) {
  const cleaned = (val || []).map((v: any) => typeof v === 'string' ? v : String(v?.title ?? v)).filter(Boolean)
  emit('update:modelValue', cleaned)
}

// ── Path validation for file/folder ──
const invalidPaths = computed(() =>
  props.modelValue.filter(v => {
    const t = v.trim()
    return t.length > 0 && !t.includes('/') && !t.includes('\\')
  })
)

// ── Range generator ──
const MAX_RANGE = 20
const rangeMin = ref<number | null>(props.min ?? null)
const rangeMax = ref<number | null>(props.max ?? null)
const rangeStep = ref<number | null>(props.step ?? null)
const rangeTruncated = ref(false)

function generateRange() {
  if (rangeMin.value == null || rangeMax.value == null || rangeStep.value == null || rangeStep.value <= 0) return
  if (rangeMin.value >= rangeMax.value) return
  rangeTruncated.value = false
  const values: string[] = []
  const isInt = props.type === 'int'
  for (let v = rangeMin.value; v <= rangeMax.value + 1e-9; v += rangeStep.value) {
    values.push(isInt ? String(Math.round(v)) : String(parseFloat(v.toPrecision(10))))
    if (values.length >= MAX_RANGE) {
      rangeTruncated.value = true
      break
    }
  }
  emit('update:modelValue', values)
}
</script>
