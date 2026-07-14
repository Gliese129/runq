<template>
  <v-menu v-model="open" :close-on-content-click="false" location="bottom end" width="320">
    <template #activator="{ props: act }">
      <v-btn
        v-bind="act" icon size="x-small" variant="text" color="primary"
        :aria-label="t('submit.generate_values')" :title="t('submit.generate_values')"
      >
        <v-icon size="15">mdi-auto-fix</v-icon>
      </v-btn>
    </template>

    <v-card class="pa-3">
      <v-btn-toggle v-model="mode" mandatory density="compact" variant="outlined" divided class="mb-3 d-flex">
        <v-btn v-for="m in modes" :key="m.id" :value="m.id" size="x-small" class="flex-grow-1 text-none">{{ m.label }}</v-btn>
      </v-btn-toggle>

      <div class="d-flex ga-2 mb-2">
        <template v-for="f in fields" :key="f.key">
          <!-- integer counts: native stepper -->
          <v-number-input
            v-if="f.kind === 'int'"
            :model-value="intVal(f.key)"
            @update:model-value="inputs[f.key] = String($event ?? '')"
            :label="f.label" :min="f.min ?? 1"
            control-variant="stacked"
            density="compact" variant="outlined" hide-details
            class="font-mono"
          />
          <!-- multiplicative fields (step): arrows halve/double — grid
               refinement is multiplicative, ±1 is meaningless at 1e-3 scale -->
          <v-text-field
            v-else-if="f.kind === 'mult'"
            v-model="inputs[f.key]"
            :label="f.label"
            density="compact" variant="outlined" hide-details
            class="font-mono"
          >
            <template #append-inner>
              <div class="d-flex flex-column justify-center" style="margin: -4px 0">
                <v-icon size="14" class="cursor-pointer opacity-70" title="×2" @click="scaleField(f.key, 2)">mdi-chevron-up</v-icon>
                <v-icon size="14" class="cursor-pointer opacity-70" title="÷2" @click="scaleField(f.key, 0.5)">mdi-chevron-down</v-icon>
              </div>
            </template>
          </v-text-field>
          <v-text-field
            v-else
            v-model="inputs[f.key]"
            :label="f.label"
            density="compact" variant="outlined" hide-details
            class="font-mono" style="font-size: 12px"
          />
        </template>
      </div>

      <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.gen_preview') }} ({{ preview.length }})</div>
      <div class="d-flex flex-wrap ga-1 mb-3" style="max-height: 88px; overflow-y: auto">
        <v-chip v-for="v in preview" :key="v" size="x-small" variant="tonal" color="primary" class="font-mono">{{ v }}</v-chip>
        <span v-if="preview.length === 0" class="text-caption text-error">{{ emptyHint }}</span>
      </div>

      <div class="d-flex ga-2">
        <v-btn size="small" variant="tonal" color="primary" :disabled="preview.length === 0" @click="apply('replace')">{{ t('common.replace') }}</v-btn>
        <v-btn size="small" variant="text" color="primary" :disabled="preview.length === 0" @click="apply('append')">{{ t('common.append') }}</v-btn>
        <v-spacer />
        <v-btn size="small" variant="text" @click="open = false">{{ t('common.cancel') }}</v-btn>
      </div>

      <div class="text-caption text-on-surface-variant mt-2">
        tip: type <code>log 1e-4 1e-1 4</code> directly in the cell
      </div>
    </v-card>
  </v-menu>
</template>

<script setup lang="ts">
import { ref, reactive, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  linearSpace, logSpace, ratioSpace, aroundDefault, seedRange,
} from '@/views/submit/valueGenerators'

const { t } = useI18n()

const props = withDefaults(defineProps<{
  type: string            // 'int' | 'float'
  defaultValue?: string
}>(), { defaultValue: '' })

const emit = defineEmits<{
  apply: [values: string[], mode: 'replace' | 'append']
  'update:open': [open: boolean]
}>()

const open = ref(false)
watch(open, v => emit('update:open', v))
const mode = ref<'linear' | 'log' | 'ratio' | 'around' | 'seeds'>('log')

const modes = computed(() => {
  const base = [
    { id: 'log', label: 'log' },
    { id: 'linear', label: 'linear' },
    { id: 'ratio', label: '×ratio' },
  ]
  if (props.defaultValue) base.push({ id: 'around', label: '±default' })
  if (props.type === 'int') base.push({ id: 'seeds', label: 'seeds' })
  return base
})

const inputs = reactive<Record<string, string>>({
  min: '', max: '', step: '', count: '4', start: '', ratio: '2', n: '3',
})

interface FieldDef { key: string; label: string; kind?: 'int' | 'mult'; min?: number }

const FIELD_DEFS: Record<string, FieldDef[]> = {
  linear: [{ key: 'min', label: 'min' }, { key: 'max', label: 'max' }, { key: 'step', label: 'step', kind: 'mult' }],
  log: [{ key: 'min', label: 'min' }, { key: 'max', label: 'max' }, { key: 'count', label: 'count', kind: 'int', min: 2 }],
  ratio: [{ key: 'start', label: 'start' }, { key: 'ratio', label: 'ratio' }, { key: 'count', label: 'count', kind: 'int', min: 1 }],
  around: [],
  seeds: [{ key: 'n', label: 'how many', kind: 'int', min: 1 }],
}

function intVal(key: string): number {
  const n = parseInt(inputs[key], 10)
  return Number.isFinite(n) ? n : 1
}

/** Multiplicative stepper for scale-free fields, with float cleanup. */
function scaleField(key: string, factor: number) {
  const v = parseFloat(inputs[key])
  if (!Number.isFinite(v) || v === 0) { inputs[key] = factor >= 1 ? '1' : '0.5'; return }
  inputs[key] = String(parseFloat((v * factor).toPrecision(10)))
}

const fields = computed(() => FIELD_DEFS[mode.value])

const isInt = computed(() => props.type === 'int')
const num = (k: string) => parseFloat(inputs[k])

const preview = computed<string[]>(() => {
  switch (mode.value) {
    case 'linear': return linearSpace(num('min'), num('max'), num('step'), isInt.value)
    case 'log': return logSpace(num('min'), num('max'), num('count'), isInt.value)
    case 'ratio': return ratioSpace(num('start'), num('ratio'), num('count'), isInt.value)
    case 'around': return aroundDefault(parseFloat(props.defaultValue), isInt.value)
    case 'seeds': return seedRange(num('n'))
  }
})

const emptyHint = computed(() => {
  switch (mode.value) {
    case 'log': return 'need 0 < min < max, count ≥ 2'
    case 'linear': return 'need min ≤ max, step > 0'
    case 'ratio': return 'need start, ratio ≠ 1, count ≥ 1'
    case 'around': return 'no usable default'
    default: return 'need n ≥ 1'
  }
})

function apply(applyMode: 'replace' | 'append') {
  emit('apply', preview.value, applyMode)
  open.value = false
}
</script>

<style scoped>
.font-mono :deep(input) { font-family: monospace; }
</style>
