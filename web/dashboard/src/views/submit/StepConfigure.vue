<template>
  <v-row no-gutters>
    <!-- Left panel: script + project + param palette -->
    <v-col cols="12" md="4" class="pr-md-4" style="position: sticky; top: 80px; align-self: flex-start">
      <v-card class="mb-3 pa-3">
        <div class="d-flex align-center ga-2">
          <v-icon size="16" color="primary">mdi-file-code-outline</v-icon>
          <code class="text-body-2 flex-grow-1 text-truncate">{{ state.selectedScript?.name }}</code>
          <v-chip v-if="state.parseResult?.detected_env" size="x-small" variant="tonal" color="secondary">
            {{ state.parseResult.detected_env }}
          </v-chip>
        </div>
      </v-card>

      <v-card class="mb-3 pa-3">
        <v-text-field
          v-model="state.projectName"
          :label="t('submit.project')"
          prepend-inner-icon="mdi-folder-outline"
          density="compact"
          variant="outlined"
          class="mb-2"
        />
        <v-text-field
          v-model="state.note"
          :label="t('submit.note')"
          :placeholder="t('submit.note_placeholder')"
          prepend-inner-icon="mdi-text-short"
          density="compact"
          variant="outlined"
          hide-details
        />
      </v-card>

      <v-card class="pa-3">
        <div class="d-flex align-center justify-space-between mb-2">
          <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
            {{ t('submit.params') }}
            <v-chip size="x-small" variant="tonal">{{ state.args.length }}</v-chip>
          </div>
          <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
            <v-icon size="11">mdi-cursor-move</v-icon> drag
          </div>
        </div>

        <draggable
          :list="dragSourceArgs"
          :group="{ name: 'params', pull: 'clone', put: false }"
          :sort="false"
          :clone="cloneParamForGroup"
          item-key="name"
          class="d-flex flex-column overflow-y-auto"
          :style="{ maxHeight: 'calc(100vh - 420px)' }"
        >
          <template #item="{ element: arg }">
            <v-expansion-panels v-model="expandedParam" class="mb-1">
              <v-expansion-panel
                :value="arg.name"
                :disabled="state.usedParamNames.has(arg.name)"
                elevation="0"
                bg-color="transparent"
              >
                <v-expansion-panel-title class="py-1 px-2" style="min-height: 34px">
                  <div class="d-flex align-center ga-1 flex-grow-1" style="min-width: 0">
                    <v-icon size="12" color="on-surface-variant" class="cursor-grab">mdi-drag-vertical</v-icon>
                    <span class="text-body-2 font-weight-medium text-truncate"
                      :class="state.usedParamNames.has(arg.name) ? 'text-disabled' : ''"
                    >{{ arg.name }}</span>
                    <span class="text-caption text-on-surface-variant">{{ arg.type }}</span>
                    <v-spacer />
                    <span v-if="state.usedParamNames.has(arg.name)" class="text-caption text-on-surface-variant" style="font-size: 10px">
                      in {{ getGroupLabel(arg.name) }}
                    </span>
                    <code v-else class="text-caption text-on-surface-variant text-truncate" style="max-width: 80px">
                      {{ displayValue(arg) }}
                    </code>
                  </div>
                  <template #actions="{ expanded }">
                    <div class="d-flex align-center">
                      <v-btn
                        v-if="!state.usedParamNames.has(arg.name)"
                        icon size="x-small" variant="text"
                        @click.stop="sendToGroup(arg.name)"
                        @mouseenter="hoverTargetGroupId = lastFocusedGroupId"
                        @mouseleave="hoverTargetGroupId = null"
                      >
                        <v-icon size="14" color="primary">mdi-chevron-right</v-icon>
                      </v-btn>
                      <v-icon size="14" class="ml-1">{{ expanded ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
                    </div>
                  </template>
                </v-expansion-panel-title>
                <v-expansion-panel-text>
                  <v-switch
                    v-if="arg.type === 'bool'"
                    v-model="getArg(arg.name).boolValue"
                    :label="getArg(arg.name).boolValue ? 'true' : 'false'"
                    hide-details density="compact" color="primary"
                  />
                  <v-text-field
                    v-else
                    v-model="getArg(arg.name).value"
                    :placeholder="arg.default || ''"
                    density="compact" variant="outlined" hide-details
                    style="font-family: monospace"
                  />
                </v-expansion-panel-text>
              </v-expansion-panel>
            </v-expansion-panels>
          </template>
        </draggable>
      </v-card>
    </v-col>

    <!-- Right panel: sweep group builder -->
    <v-col cols="12" md="8">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="d-flex align-center ga-2">
          <div class="text-subtitle-2">Sweep Groups</div>
          <v-btn icon size="x-small" variant="text" :disabled="!canUndo" @click="undo">
            <v-icon size="16">mdi-undo</v-icon>
          </v-btn>
          <v-btn icon size="x-small" variant="text" :disabled="!canRedo" @click="redo">
            <v-icon size="16">mdi-redo</v-icon>
          </v-btn>
        </div>
        <code class="text-body-2 text-on-surface-variant">
          = {{ state.totalTaskCount }} {{ state.totalTaskCount === 1 ? 'task' : 'tasks' }}
        </code>
      </div>

      <div class="d-flex flex-column ga-2 overflow-y-auto" :style="{ maxHeight: 'calc(100vh - 280px)' }">
        <v-card
          v-for="group in state.groups"
          :key="group.id"
          class="group-card"
          :class="{
            'group-card--grid': group.type === 'grid',
            'group-card--list': group.type === 'list',
            'group-highlight': hoverTargetGroupId === group.id,
            'group-focused': lastFocusedGroupId === group.id,
          }"
          @click="lastFocusedGroupId = group.id"
        >
          <v-expansion-panels v-model="group.expanded">
            <v-expansion-panel :value="true" elevation="0">
              <v-expansion-panel-title class="py-2 pr-2">
                <v-switch
                  :model-value="group.type === 'list'"
                  density="compact" hide-details color="success"
                  class="mr-2 flex-shrink-0" style="flex: none"
                  @click.stop
                  @update:model-value="switchGroupType(group, $event ? 'list' : 'grid')"
                >
                  <template #label>
                    <span class="text-caption font-weight-medium">{{ group.type === 'grid' ? 'Grid' : 'List' }}</span>
                  </template>
                </v-switch>

                <div v-if="group.expanded !== true && group.params.length > 0" class="flex-grow-1" style="min-width: 0">
                  <div v-for="p in group.params" :key="p.name" class="text-caption text-on-surface-variant text-truncate">
                    <span class="font-weight-medium">{{ p.name }}</span>: {{ p.values.length > 0 ? p.values.join(', ') : '-' }}
                  </div>
                  <div class="text-caption font-weight-medium text-primary mt-1">
                    {{ state.groupTaskCount(group) }} {{ group.type === 'grid' ? 'combinations' : 'runs' }}
                  </div>
                </div>
                <span v-else-if="group.expanded !== true" class="text-caption text-on-surface-variant">
                  Empty - drag params here
                </span>

                <template #actions="{ expanded }">
                  <div class="d-flex align-center ga-1">
                    <v-btn icon size="x-small" variant="text" @click.stop="removeGroup(group.id)">
                      <v-icon size="14" color="error">mdi-trash-can-outline</v-icon>
                    </v-btn>
                    <v-icon size="16">{{ expanded ? 'mdi-chevron-up' : 'mdi-chevron-down' }}</v-icon>
                  </div>
                </template>
              </v-expansion-panel-title>

              <v-expansion-panel-text>
                <draggable
                  :list="group.params"
                  :group="{ name: 'params', pull: false, put: true }"
                  item-key="name"
                  class="d-flex flex-column ga-2"
                  @add="onGroupDragAdd(group.id, $event)"
                >
                  <template #item="{ element: p }">
                    <div class="param-sheet pa-3 rounded">
                      <div class="d-flex align-center justify-space-between mb-1">
                        <div class="d-flex align-center ga-1">
                          <v-icon size="12" color="on-surface-variant">mdi-drag-vertical</v-icon>
                          <span class="text-body-2 font-weight-medium">{{ p.name }}</span>
                          <span class="text-caption text-on-surface-variant">{{ p.type }}</span>
                        </div>
                        <v-btn icon size="x-small" variant="text" @click="removeParamFromGroup(group.id, p.name)">
                          <v-icon size="14" color="on-surface-variant">mdi-close</v-icon>
                        </v-btn>
                      </div>
                      <ChipInput
                        v-model="p.values"
                        :placeholder="group.type === 'grid' ? 'Type value + Enter' : 'Add values in order'"
                        :color="group.type === 'grid' ? 'primary' : 'success'"
                        :param-type="p.type"
                      />
                    </div>
                  </template>
                </draggable>

                <div v-if="group.params.length === 0" class="drop-hint text-center pa-6 rounded">
                  <v-icon size="20" color="on-surface-variant" class="mb-1">mdi-drag-variant</v-icon>
                  <div class="text-caption text-on-surface-variant">Drag params here or click ">" in the palette</div>
                </div>
              </v-expansion-panel-text>
            </v-expansion-panel>
          </v-expansion-panels>
        </v-card>
      </div>

      <div class="d-flex justify-center mt-4">
        <v-btn size="small" variant="tonal" color="primary" @click="addGroup('grid')">
          <v-icon start size="14">mdi-plus</v-icon> Add Sweep Group
        </v-btn>
      </div>
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import ChipInput from '@/components/ChipInput.vue'
import draggable from 'vuedraggable'
import { SUBMIT_STATE_KEY, type ArgState, type GroupParam, type SweepGroup } from './types'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!

function getArg(name: string): ArgState {
  return state.args.find((a: any) => a.name === name) || ({} as ArgState)
}

function getGroupLabel(paramName: string): string {
  for (const g of state.groups) {
    if (g.params.some((p: GroupParam) => p.name === paramName)) {
      const idx = state.groups.filter((x: SweepGroup) => x.type === g.type).indexOf(g) + 1
      return `${g.type === 'grid' ? 'Grid' : 'List'} ${idx}`
    }
  }
  return ''
}

function displayValue(arg: { type: string; boolValue?: boolean; value?: string; default?: string }): string {
  if (arg.type === 'bool') return arg.boolValue ? 'true' : 'false'
  return arg.value || arg.default || '-'
}

// --- Undo / redo ---
interface ConfigSnapshot { args: { name: string; value: string; boolValue: boolean }[]; groups: string }
const undoStack = ref<ConfigSnapshot[]>([])
const redoStack = ref<ConfigSnapshot[]>([])
const isRestoring = ref(false)
let historyTimer: ReturnType<typeof setTimeout> | null = null

function takeSnapshot(): ConfigSnapshot {
  return { args: state.args.map((a: any) => ({ name: a.name, value: a.value, boolValue: a.boolValue })), groups: JSON.stringify(state.groups) }
}
function pushSnapshot() { if (isRestoring.value) return; undoStack.value.push(takeSnapshot()); redoStack.value = [] }
function restoreSnapshot(snap: ConfigSnapshot) {
  isRestoring.value = true
  for (const s of snap.args) { const arg = state.args.find((a: any) => a.name === s.name); if (arg) { arg.value = s.value; arg.boolValue = s.boolValue } }
  state.groups = JSON.parse(snap.groups)
  nextTick(() => { isRestoring.value = false })
}
function undo() { if (undoStack.value.length === 0) return; redoStack.value.push(takeSnapshot()); restoreSnapshot(undoStack.value.pop()!) }
function redo() { if (redoStack.value.length === 0) return; undoStack.value.push(takeSnapshot()); restoreSnapshot(redoStack.value.pop()!) }
const canUndo = computed(() => undoStack.value.length > 0)
const canRedo = computed(() => redoStack.value.length > 0)

watch(() => [state.args, state.groups], () => { if (isRestoring.value) return; if (historyTimer) clearTimeout(historyTimer); historyTimer = setTimeout(pushSnapshot, 600) }, { deep: true })

function onKeydown(e: KeyboardEvent) {
  if (state.step !== 1) return
  const mod = e.metaKey || e.ctrlKey
  if (mod && e.key === 'z' && !e.shiftKey) { e.preventDefault(); undo() }
  if (mod && e.key === 'z' && e.shiftKey) { e.preventDefault(); redo() }
  if (mod && e.key === 'y') { e.preventDefault(); redo() }
}
onMounted(() => document.addEventListener('keydown', onKeydown))
onUnmounted(() => document.removeEventListener('keydown', onKeydown))

// --- Group management ---
function addGroup(type: 'grid' | 'list') {
  const id = state.getNextGroupId()
  state.groups.push({ id: `g${id}`, type, expanded: true, params: [] })
  lastFocusedGroupId.value = `g${id}`
}
function removeGroup(id: string) {
  state.groups = state.groups.filter((g: SweepGroup) => g.id !== id)
  if (lastFocusedGroupId.value === id) lastFocusedGroupId.value = state.groups.length > 0 ? state.groups[state.groups.length - 1].id : null
}
function removeParamFromGroup(groupId: string, paramName: string) {
  const group = state.groups.find((g: SweepGroup) => g.id === groupId)
  if (!group) return
  group.params = group.params.filter((p: GroupParam) => p.name !== paramName)
  if (group.params.length === 0) removeGroup(groupId)
}

// --- Grid/List toggle + auto-coerce ---
function coerceValue(raw: string, type: string): string | null {
  const t = type.toLowerCase()
  if (t === 'int') { const n = Number(raw); return isNaN(n) ? null : String(Math.trunc(n)) }
  if (t === 'float') { const n = Number(raw); return isNaN(n) ? null : String(n) }
  if (t === 'bool') { const low = raw.toLowerCase(); if (['true','1','yes'].includes(low)) return 'true'; if (['false','0','no'].includes(low)) return 'false'; return null }
  return raw
}
function coerceGroupValues(group: SweepGroup) {
  for (const p of group.params) { if (!p.type) continue; p.values = p.values.map(v => coerceValue(v, p.type)).filter((v): v is string => v !== null) }
}
function switchGroupType(group: SweepGroup, newType: 'grid' | 'list') { group.type = newType }

watch(() => state.groups.map((g: SweepGroup) => g.expanded), (newVals, oldVals) => {
  if (!oldVals) return
  for (let i = 0; i < state.groups.length; i++) { if (oldVals[i] === true && newVals[i] !== true) coerceGroupValues(state.groups[i]) }
})

// --- Draggable ---
const dragSourceArgs = computed(() => state.args.map((a: any) => ({ name: a.name, type: a.type, default: a.value || a.default || '' })))
function cloneParamForGroup(src: { name: string; type: string; default: string }): GroupParam { return { name: src.name, type: src.type, default: src.default, values: [] } }
function onGroupDragAdd(groupId: string, _evt: any) {
  const group = state.groups.find((g: SweepGroup) => g.id === groupId)
  if (!group) return
  const seen = new Set<string>()
  group.params = group.params.reduceRight<GroupParam[]>((acc, p) => { if (seen.has(p.name)) return acc; seen.add(p.name); acc.unshift(p); return acc }, [])
  const otherUsed = new Set<string>()
  for (const g of state.groups) { if (g.id === groupId) continue; for (const p of g.params) otherUsed.add(p.name) }
  group.params = group.params.filter((p: GroupParam) => !otherUsed.has(p.name))
  group.expanded = true
  lastFocusedGroupId.value = groupId
}

// --- Palette state ---
const expandedParam = ref<string | null>(null)
const lastFocusedGroupId = ref<string | null>(null)
const hoverTargetGroupId = ref<string | null>(null)

function sendToGroup(paramName: string) {
  if (state.usedParamNames.has(paramName)) return
  let targetId = lastFocusedGroupId.value
  if (!targetId || !state.groups.find((g: SweepGroup) => g.id === targetId)) {
    if (state.groups.length === 0) addGroup('grid')
    targetId = state.groups[state.groups.length - 1].id
  }
  const group = state.groups.find((g: SweepGroup) => g.id === targetId)
  const arg = state.args.find((a: any) => a.name === paramName)
  if (!group || !arg || group.params.some((p: GroupParam) => p.name === paramName)) return
  group.params.push({ name: arg.name, type: arg.type, default: arg.value || arg.default || '', values: [] })
  group.expanded = true
  lastFocusedGroupId.value = targetId
}
</script>

<style scoped>
.group-card--grid { border-left: 3px solid rgb(var(--v-theme-primary)) !important; }
.group-card--list { border-left: 3px solid rgb(var(--v-theme-success)) !important; }
.group-focused { box-shadow: 0 0 0 1px rgb(var(--v-theme-primary), 0.2); }
.group-highlight { outline: 2px solid rgb(var(--v-theme-primary)); outline-offset: -1px; }
.param-sheet { background: rgb(var(--v-theme-surface-variant), 0.3); border: 0.5px solid rgb(var(--v-theme-surface-variant)); }
.cursor-grab { cursor: grab; }
.cursor-grab:active { cursor: grabbing; }
.drop-hint { border: 1.5px dashed rgb(var(--v-theme-outline-variant)); }
.text-disabled { color: rgb(var(--v-theme-on-surface-variant)) !important; }
</style>
