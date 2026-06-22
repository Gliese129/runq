<template>
  <div class="log-side-panel">
    <!-- Processor toggles — compact btn row -->
    <div class="panel-section">
      <div class="panel-header">Processors</div>
      <div class="btn-wrap">
        <v-btn
          v-for="p in processors" :key="p.key"
          size="x-small"
          :variant="toggles[p.key as keyof ProcessorToggles] ? 'flat' : 'outlined'"
          :color="toggles[p.key as keyof ProcessorToggles] ? 'primary' : undefined"
          class="toggle-btn"
          @click="$emit('toggle-processor', p.key)"
        >{{ p.label }}</v-btn>
      </div>
    </div>

    <!-- Pre-Drain rules — compact -->
    <div class="panel-section">
      <div class="d-flex align-center justify-space-between">
        <div class="panel-header mb-0">Rules</div>
        <v-btn size="x-small" variant="text" icon="mdi-plus" density="compact" @click="openAdd" />
      </div>
      <div class="btn-wrap" v-if="rules.length > 0">
        <v-btn
          v-for="(rule, idx) in rules" :key="idx"
          size="x-small"
          :variant="rule.enabled ? 'flat' : 'outlined'"
          :color="rule.enabled ? 'secondary' : undefined"
          class="toggle-btn"
          @click="$emit('toggle-rule', idx)"
          @contextmenu.prevent="openEdit(idx)"
        >
          {{ rule.name }}
          <v-tooltip activator="parent" location="top">
            /{{ rule.pattern }}/ → {{ rule.replacement }}
            <br>Right-click to edit
          </v-tooltip>
        </v-btn>
      </div>
      <div v-else class="text-caption text-disabled pa-1">No rules</div>
    </div>

    <!-- Rule edit dialog -->
    <v-dialog v-model="dialog" max-width="480">
      <v-card>
        <v-card-title class="text-subtitle-1">
          {{ editIdx < 0 ? 'Add Rule' : 'Edit Rule' }}
          <v-btn v-if="editIdx >= 0" size="x-small" variant="text" icon="mdi-delete-outline"
            color="error" class="ml-2" @click="removeAndClose" />
        </v-card-title>
        <v-card-text class="pb-0">
          <v-text-field v-model="editForm.name" label="Name" variant="outlined" density="compact" class="mb-2" />
          <v-text-field v-model="editForm.pattern" label="Regex pattern" variant="outlined" density="compact"
            class="mb-2 mono-input" :error-messages="patternError" />
          <v-text-field v-model="editForm.replacement" label="Replacement" variant="outlined" density="compact"
            class="mono-input" hint="Supports $1, $2, etc." persistent-hint />
        </v-card-text>
        <v-card-actions>
          <v-spacer />
          <v-btn variant="text" @click="dialog = false">Cancel</v-btn>
          <v-btn variant="tonal" color="primary" :disabled="!editForm.name || !editForm.pattern || !!patternError" @click="saveRule">
            {{ editIdx < 0 ? 'Add' : 'Save' }}
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>

    <!-- Motif groups — compact list -->
    <div v-if="motifGroups.length > 0" class="panel-section">
      <div class="panel-header">Groups ({{ motifGroups.length }})</div>
      <div
        v-for="g in motifGroups" :key="g.id"
        class="group-row"
        :class="{ 'group-row--hidden': isHidden(g) }"
      >
        <v-btn
          size="x-small" variant="text" density="compact"
          :icon="isHidden(g) ? 'mdi-eye-off-outline' : 'mdi-eye-outline'"
          :color="isHidden(g) ? 'default' : 'primary'"
          @click="$emit('toggle-cluster', g.id)"
        />
        <span class="group-label text-caption" @click="$emit('scroll-to-group', g.id)" :title="g.label">
          <v-chip v-if="g.motifLength > 1 && new Set(g.templates).size > 1"
            size="x-small" variant="tonal" color="info" class="mr-1" label>P</v-chip>
          {{ g.label }}
        </span>
        <v-chip size="x-small" variant="text" class="group-count flex-shrink-0">
          {{ g.totalLines }}
        </v-chip>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import type { ProcessorToggles, MotifGroup, PreDrainRule } from '@/utils/logProcessors'

const props = defineProps<{
  toggles: ProcessorToggles
  motifGroups: MotifGroup[]
  hiddenGroupIds: Set<number>
  rules: PreDrainRule[]
}>()

const emit = defineEmits<{
  'toggle-processor': [key: string]
  'toggle-cluster': [groupId: number]
  'scroll-to-group': [groupId: number]
  'toggle-rule': [index: number]
  'add-rule': [rule: PreDrainRule]
  'update-rule': [index: number, rule: PreDrainRule]
  'remove-rule': [index: number]
}>()

const processors = [
  { key: 'crFolder',       label: '\\r' },
  { key: 'tracebackFold',  label: 'TB' },
  { key: 'levelColoring',  label: 'Level' },
  { key: 'metricHighlight', label: 'Metric' },
  { key: 'rankColoring',   label: 'Rank' },
]

function isHidden(g: MotifGroup): boolean {
  return props.hiddenGroupIds.has(g.id)
}

// ── Rule edit dialog ──
const dialog = ref(false)
const editIdx = ref(-1) // -1 = adding new
const editForm = ref({ name: '', pattern: '', replacement: '' })

const patternError = computed(() => {
  if (!editForm.value.pattern) return ''
  try {
    new RegExp(editForm.value.pattern)
    return ''
  } catch (e: any) {
    return e.message || 'Invalid regex'
  }
})

function openAdd() {
  editIdx.value = -1
  editForm.value = { name: '', pattern: '', replacement: '' }
  dialog.value = true
}

function openEdit(idx: number) {
  editIdx.value = idx
  const r = props.rules[idx]
  editForm.value = { name: r.name, pattern: r.pattern, replacement: r.replacement }
  dialog.value = true
}

function saveRule() {
  const rule: PreDrainRule = { ...editForm.value, enabled: true }
  if (editIdx.value < 0) {
    emit('add-rule', rule)
  } else {
    rule.enabled = props.rules[editIdx.value].enabled
    emit('update-rule', editIdx.value, rule)
  }
  dialog.value = false
}

function removeAndClose() {
  emit('remove-rule', editIdx.value)
  dialog.value = false
}
</script>

<style scoped>
.log-side-panel {
  width: 220px;
  min-width: 220px;
  border-left: 1px solid rgb(var(--v-theme-outline-variant));
  padding: 8px;
  overflow-y: auto;
  max-height: 600px;
}
.panel-section + .panel-section {
  margin-top: 10px;
  padding-top: 8px;
  border-top: 1px solid rgb(var(--v-theme-outline-variant));
}
.panel-header {
  font-size: 0.7rem;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: rgb(var(--v-theme-on-surface-variant));
  margin-bottom: 6px;
}
.btn-wrap {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.toggle-btn {
  text-transform: none;
  font-size: 0.7rem !important;
  letter-spacing: 0;
  min-width: 0 !important;
  padding: 0 8px !important;
  height: 24px !important;
}
.mono-input :deep(input) {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 0.85rem;
}
.group-row {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 2px 0;
  border-bottom: 1px solid rgba(var(--v-theme-outline-variant), 0.5);
}
.group-row:last-child { border-bottom: none; }
.group-row--hidden { opacity: 0.45; }
.group-label {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  cursor: pointer;
  line-height: 1.3;
}
.group-label:hover { text-decoration: underline; }
.group-count {
  font-size: 0.7rem;
  color: rgb(var(--v-theme-on-surface-variant));
}
</style>
