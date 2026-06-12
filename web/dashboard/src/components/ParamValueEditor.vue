<template>
  <!-- Resting state: the cell IS the default chip. Click to edit. -->
  <v-chip
    v-if="collapsed"
    size="small" variant="outlined"
    class="opacity-70 cursor-pointer"
    style="font-family: monospace"
    title="default — click to edit"
    @click="expand"
  >
    {{ defaultValue }}
    <v-icon end size="11" class="opacity-60">mdi-pencil-outline</v-icon>
  </v-chip>

  <!-- Edit state: collapses back to the default chip when left empty. -->
  <div v-else ref="rootEl" tabindex="-1" style="outline: none" @focusout="onFocusOut">
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

  <!-- str / file / folder: ChipInput (ordered, duplicates allowed — zip
       sequences like [A, A, B] are legal) + suggestions as append-on-click
       chips. A combobox's checkbox semantics can't express duplicates. -->
  <div v-else-if="type === 'str' || type === 'file' || type === 'folder'">
    <ChipInput
      :model-value="modelValue"
      @update:model-value="$emit('update:modelValue', $event)"
      :placeholder="effectivePlaceholder"
      :color="color"
      :param-type="type"
    />
    <div v-if="availableSuggestions.length > 0" class="d-flex flex-wrap ga-1 mt-1">
      <v-chip
        v-for="sug in availableSuggestions" :key="sug"
        size="x-small" variant="outlined" class="cursor-pointer opacity-70"
        style="font-family: monospace"
        :title="type !== 'str' ? sug : 'click to append (repeatable)'"
        @click="$emit('update:modelValue', [...modelValue, sug])"
      >+ {{ type !== 'str' ? (sug.split('/').pop() || sug) : sug }}</v-chip>
    </div>
    <div v-if="type !== 'str' && invalidPaths.length > 0" class="text-caption text-warning mt-1">
      {{ invalidPaths.length }} value(s) don't look like paths
    </div>
  </div>

  <!-- int/float: ChipInput with ⚡ generator in the field's action area -->
  <div v-else-if="type === 'int' || type === 'float'">
    <ChipInput
      :model-value="modelValue"
      @update:model-value="$emit('update:modelValue', $event)"
      :placeholder="effectivePlaceholder"
      :color="color"
      :param-type="type"
      generator-sugar
    >
      <template #actions>
        <ValueGenerator
          :type="type" :default-value="defaultValue"
          @apply="onGenerated" @update:open="generatorOpen = $event"
        />
      </template>
    </ChipInput>
  </div>

  <!-- list / fallback: plain ChipInput -->
  <ChipInput
    v-else
    :model-value="modelValue"
    @update:model-value="$emit('update:modelValue', $event)"
    :placeholder="effectivePlaceholder"
    :color="color"
    :param-type="type"
  />
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import ChipInput from './ChipInput.vue'
import ValueGenerator from './ValueGenerator.vue'

const props = withDefaults(defineProps<{
  modelValue: string[]
  type: string
  placeholder?: string
  color?: string
  defaultValue?: string   // param default — feeds the ±default generator
  suggestions?: string[]  // pre-defined values from project param definition
  /** Whether the cell may rest as the default chip. Linked rows must pass
   *  false: "fixed at default" and "zipped" are contradictory states. */
  collapsible?: boolean
}>(), {
  placeholder: 'Type value + Enter',
  color: 'primary',
  defaultValue: '',
  suggestions: () => [],
  collapsible: true,
})

const emit = defineEmits<{
  'update:modelValue': [values: string[]]
}>()

// ── Collapsed/edit state ──
// A cell with no values and a default rests as the default chip. Clicking
// it opens the editor; leaving the editor empty (focusout to outside)
// collapses it back. Cells with values never collapse.
const rootEl = ref<HTMLElement>()
const editing = ref(false)
// The ⚡ popover teleports to body; while it's open, focusout must not
// collapse the cell (the menu's activator would unmount under it).
const generatorOpen = ref(false)

const collapsed = computed(() =>
  props.collapsible && !editing.value && props.modelValue.length === 0 && !!props.defaultValue,
)

const effectivePlaceholder = computed(() =>
  props.defaultValue ? `default ${props.defaultValue} — type to override/sweep` : props.placeholder,
)

function expand() {
  editing.value = true
  nextTick(() => {
    const el = rootEl.value
    if (!el) return
    ;(el.querySelector('input') as HTMLElement | null ?? el).focus()
  })
}

function onFocusOut(e: FocusEvent) {
  if (generatorOpen.value) return
  const el = rootEl.value
  if (el && e.relatedTarget instanceof Node && el.contains(e.relatedTarget)) return
  if (props.modelValue.length === 0) editing.value = false
}

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


const availableSuggestions = computed(() => props.suggestions)

// ── Path validation for file/folder ──
const invalidPaths = computed(() =>
  props.modelValue.filter(v => {
    const t = v.trim()
    return t.length > 0 && !t.includes('/') && !t.includes('\\')
  })
)

// ── ⚡ generator ──
function onGenerated(values: string[], mode: 'replace' | 'append') {
  if (mode === 'replace') {
    emit('update:modelValue', values)
  } else {
    const merged = [...props.modelValue]
    for (const v of values) if (!merged.includes(v)) merged.push(v)
    emit('update:modelValue', merged)
  }
}
</script>
