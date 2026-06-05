<template>
  <v-row no-gutters style="max-width: 960px; margin: 0 auto">
    <!-- Left panel: project + param palette -->
    <v-col cols="12" md="4" class="pr-md-4" style="position: sticky; top: 80px; align-self: flex-start">
      <v-card class="mb-3 pa-3">
        <div class="d-flex align-center ga-2 mb-2">
          <v-icon size="16" color="primary">mdi-folder-outline</v-icon>
          <span class="text-body-2 font-weight-medium">{{ state.projectName }}</span>
        </div>
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
            Parameters
            <v-chip size="x-small" variant="tonal">{{ paletteParams.length }}</v-chip>
          </div>
          <v-btn
            v-if="paletteParams.some(p => !usedParamNames.has(p.name))"
            size="x-small" variant="text" color="primary" class="text-none"
            @click="addAllUnused"
          >Add all</v-btn>
        </div>

        <draggable
          :list="paletteParams"
          :group="{ name: 'params', pull: 'clone', put: false }"
          :sort="false"
          :clone="cloneParamForGroup"
          item-key="name"
          class="d-flex flex-column overflow-y-auto"
          :style="{ maxHeight: 'calc(100vh - 420px)' }"
        >
          <template #item="{ element: param }">
            <div
              class="palette-item d-flex align-center ga-2 pa-2 rounded mb-1"
              :class="{ 'text-disabled': usedParamNames.has(param.name) }"
            >
              <v-icon size="12" color="on-surface-variant" class="cursor-grab flex-shrink-0">mdi-drag-vertical</v-icon>
              <div class="flex-grow-1" style="min-width: 0">
                <div class="d-flex align-center ga-1">
                  <span class="text-body-2 font-weight-medium text-truncate">{{ param.name }}</span>
                  <span class="text-caption text-on-surface-variant">{{ param.type }}</span>
                </div>
                <div v-if="param.default" class="text-caption text-on-surface-variant text-truncate" style="font-family: monospace; font-size: 11px">
                  = {{ param.default }}
                </div>
              </div>
              <span v-if="usedParamNames.has(param.name)" class="text-caption text-on-surface-variant flex-shrink-0" style="font-size: 10px">
                {{ getGroupLabel(param.name) }}
              </span>
              <v-btn
                v-else
                icon size="x-small" variant="text" color="primary" class="flex-shrink-0"
                @click.stop="sendToGroup(param.name)"
              >
                <v-icon size="14">mdi-chevron-right</v-icon>
              </v-btn>
            </div>
          </template>
        </draggable>

        <!-- Manual add -->
        <div class="d-flex ga-2 mt-2">
          <v-text-field
            v-model="newParamName"
            placeholder="custom param..."
            density="compact"
            variant="underlined"
            hide-details
            style="font-family: monospace; font-size: 13px"
            @keydown.enter="addCustomParam"
          />
          <v-btn
            icon size="x-small" variant="text" color="primary"
            :disabled="!newParamName.trim()"
            @click="addCustomParam"
          >
            <v-icon size="14">mdi-plus</v-icon>
          </v-btn>
        </div>
      </v-card>
    </v-col>

    <!-- Right panel: sweep group builder -->
    <v-col cols="12" md="8">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="text-subtitle-2">Sweep Groups</div>
        <code class="text-body-2 text-on-surface-variant">
          = {{ state.totalTaskCount }} {{ state.totalTaskCount === 1 ? 'task' : 'tasks' }}
        </code>
      </div>

      <v-alert
        v-if="state.groups.length === 0"
        density="compact"
        variant="tonal"
        color="primary"
        icon="mdi-pin-outline"
        class="mb-3"
      >
        No sweep configured. Dry-run will create one task; {{ fixedDefaultCount }} default {{ fixedDefaultCount === 1 ? 'param' : 'params' }} will stay fixed.
      </v-alert>

      <div class="d-flex flex-column ga-2 overflow-y-auto" :style="{ maxHeight: 'calc(100vh - 280px)' }">
        <v-card
          v-for="group in state.groups"
          :key="group.id"
          class="group-card"
          :class="{
            'group-card--grid': group.type === 'grid',
            'group-card--list': group.type === 'list',
            'group-focused': lastFocusedGroupId === group.id,
          }"
          @click="lastFocusedGroupId = group.id"
        >
          <v-expansion-panels v-model="group.expanded">
            <v-expansion-panel :value="true" elevation="0">
              <v-expansion-panel-title class="py-2 pr-2">
                <v-btn-toggle
                  :model-value="group.type"
                  @update:model-value="group.type = $event || group.type"
                  mandatory density="compact" variant="outlined" divided
                  class="mr-2 flex-shrink-0"
                  @click.stop
                >
                  <v-btn value="grid" size="x-small" :color="group.type === 'grid' ? 'primary' : undefined">
                    <v-icon start size="12">mdi-grid</v-icon> Grid
                  </v-btn>
                  <v-btn value="list" size="x-small" :color="group.type === 'list' ? 'success' : undefined">
                    <v-icon start size="12">mdi-table</v-icon> List
                  </v-btn>
                </v-btn-toggle>

                <div v-if="group.expanded !== true && group.params.length > 0" class="flex-grow-1" style="min-width: 0">
                  <div class="text-caption text-on-surface-variant text-truncate">
                    {{ group.params.map(p => `${p.name}(${p.values.filter(v => !isBlankValue(v)).length})`).join(group.type === 'grid' ? ' × ' : ', ') }}
                  </div>
                  <div class="text-caption font-weight-medium mt-1" :class="group.type === 'grid' ? 'text-primary' : 'text-success'">
                    = {{ state.groupTaskCount(group) }} {{ group.type === 'grid' ? 'combinations' : 'runs' }}
                  </div>
                </div>
                <span v-else-if="group.expanded !== true" class="text-caption text-on-surface-variant">
                  Empty — add params from palette
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
                <!-- Quick actions -->
                <div v-if="group.params.some(p => p.default && !p.values.includes(p.default))" class="d-flex ga-2 mb-2">
                  <v-btn size="x-small" variant="text" color="primary" class="text-none" @click="useDefaultsForGroup(group)">
                    <v-icon start size="12">mdi-auto-fix</v-icon> Use all defaults
                  </v-btn>
                </div>

                <!-- ═══ Grid mode: per-param value editor ═══ -->
                <template v-if="group.type === 'grid'">
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
                            <!-- Default chip + Use default -->
                            <template v-if="p.default">
                              <v-chip size="x-small" variant="outlined" class="ml-1">default: {{ p.default }}</v-chip>
                              <v-btn
                                v-if="!p.values.includes(p.default)"
                                size="x-small" variant="text" color="primary" class="text-none"
                                @click="p.values.push(p.default)"
                              >+ use</v-btn>
                            </template>
                          </div>
                          <v-btn icon size="x-small" variant="text" @click="removeParamFromGroup(group.id, p.name)">
                            <v-icon size="14" color="on-surface-variant">mdi-close</v-icon>
                          </v-btn>
                        </div>
                        <ParamValueEditor
                          v-model="p.values"
                          :type="p.type || 'str'"
                          placeholder="Type value + Enter"
                          color="primary"
                          :min="p.meta?.min"
                          :max="p.meta?.max"
                          :step="p.meta?.step"
                          :suggestions="getParamSuggestions(p.name)"
                        />
                      </div>
                    </template>
                  </draggable>
                </template>

                <!-- ═══ List mode: editable table ═══ -->
                <template v-else-if="group.type === 'list'">
                  <!-- Drop zone for params (hidden once params exist) -->
                  <draggable
                    :list="group.params"
                    :group="{ name: 'params', pull: false, put: true }"
                    item-key="name"
                    class="list-drop-zone mb-2"
                    :class="{ 'list-drop-zone--empty': group.params.length === 0 }"
                    @add="onGroupDragAdd(group.id, $event)"
                  >
                    <template #item="{ element: p }">
                      <v-chip size="x-small" closable variant="tonal" color="success" class="ma-1"
                        @click:close="removeParamFromGroup(group.id, p.name)"
                      >{{ p.name }}</v-chip>
                    </template>
                  </draggable>

                  <!-- Table -->
                  <div v-if="group.params.length > 0" class="overflow-x-auto">
                    <table class="data-mono" style="width: 100%">
                      <thead>
                        <tr>
                          <th class="text-caption" style="width: 32px">#</th>
                          <th v-for="p in group.params" :key="p.name" class="text-caption font-weight-medium">
                            {{ p.name }}
                            <span class="text-on-surface-variant font-weight-regular"> {{ p.type }}</span>
                          </th>
                          <th style="width: 32px"></th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(_, rowIdx) in listRowCount(group)" :key="rowIdx">
                          <td class="text-caption text-on-surface-variant">{{ rowIdx + 1 }}</td>
                          <td v-for="p in group.params" :key="p.name">
                            <input
                              class="list-cell"
                              :value="p.values[rowIdx] || ''"
                              placeholder=""
                              @input="setListCell(group, p.name, rowIdx, ($event.target as HTMLInputElement).value)"
                            />
                          </td>
                          <td>
                            <v-btn icon size="x-small" variant="text" @click="removeListRow(group, rowIdx)">
                              <v-icon size="12" color="on-surface-variant">mdi-close</v-icon>
                            </v-btn>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                    <v-btn size="x-small" variant="text" color="success" class="mt-1" @click="addListRow(group)">
                      <v-icon start size="12">mdi-plus</v-icon> Add row
                    </v-btn>
                  </div>
                </template>

                <!-- Empty state (shared) -->
                <div v-if="group.params.length === 0" class="drop-hint text-center pa-6 rounded">
                  <v-icon size="20" color="on-surface-variant" class="mb-1">mdi-drag-variant</v-icon>
                  <div class="text-caption text-on-surface-variant">Drag params here or click "›" in the palette</div>
                </div>
              </v-expansion-panel-text>
            </v-expansion-panel>
          </v-expansion-panels>
        </v-card>
      </div>

      <div class="d-flex justify-center ga-2 mt-4">
        <v-btn size="small" variant="tonal" color="primary" @click="addGroup('grid')">
          <v-icon start size="14">mdi-grid</v-icon> Grid
        </v-btn>
        <v-btn size="small" variant="tonal" color="success" @click="addGroup('list')">
          <v-icon start size="14">mdi-table</v-icon> List
        </v-btn>
      </div>
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject } from 'vue'
import { useI18n } from 'vue-i18n'
import draggable from 'vuedraggable'
import ParamValueEditor from '@/components/ParamValueEditor.vue'
import { SUBMIT_STATE_KEY, type GroupParam, type SweepGroup } from '@/types/submit'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!

const newParamName = ref('')
const lastFocusedGroupId = ref<string | null>(null)

// ── Param palette ──
// Sourced from discovered params (StepProject) + any custom-added ones
const extraParams = ref<Array<{ name: string; type: string; default: string }>>([])

const paletteParams = computed(() => {
  const fromProject = (state.newProject.params || [])
    .filter(p => p.include)
    .map(p => ({
      name: p.name,
      type: p.type || 'str',
      default: p.default || '',
      min: p.min,
      max: p.max,
      values: p.values,
    }))
  const combined = [...fromProject, ...extraParams.value]
  // Dedupe by name
  const seen = new Set<string>()
  return combined.filter(p => {
    if (seen.has(p.name)) return false
    seen.add(p.name)
    return true
  })
})

const usedParamNames = computed(() => {
  const names = new Set<string>()
  for (const g of state.groups) {
    for (const p of g.params) names.add(p.name)
  }
  return names
})

const fixedDefaultCount = computed(() =>
  (state.newProject.params || []).filter(p => p.include && !isBlankValue(p.default)).length
)

function isBlankValue(v: string): boolean {
  return String(v ?? '').trim() === ''
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

// ── Draggable ──

function cloneParamForGroup(src: any): GroupParam {
  const meta: any = {}
  if (src.min != null) meta.min = src.min
  if (src.max != null) meta.max = src.max
  return { name: src.name, type: src.type, default: src.default, values: [], meta }
}

function onGroupDragAdd(groupId: string, _evt: any) {
  const group = state.groups.find((g: SweepGroup) => g.id === groupId)
  if (!group) return
  // Dedupe within group
  const seen = new Set<string>()
  group.params = group.params.reduceRight<GroupParam[]>((acc, p) => {
    if (seen.has(p.name)) return acc
    seen.add(p.name)
    acc.unshift(p)
    return acc
  }, [])
  // Remove from other groups
  const otherUsed = new Set<string>()
  for (const g of state.groups) {
    if (g.id === groupId) continue
    for (const p of g.params) otherUsed.add(p.name)
  }
  group.params = group.params.filter((p: GroupParam) => !otherUsed.has(p.name))
  group.expanded = true
  lastFocusedGroupId.value = groupId
}

// ── Send via button ──

function sendToGroup(paramName: string) {
  if (usedParamNames.value.has(paramName)) return
  let targetId = lastFocusedGroupId.value
  if (!targetId || !state.groups.find((g: SweepGroup) => g.id === targetId)) {
    if (state.groups.length === 0) addGroup('grid')
    targetId = state.groups[state.groups.length - 1].id
  }
  const group = state.groups.find((g: SweepGroup) => g.id === targetId)
  const src = paletteParams.value.find(p => p.name === paramName)
  if (!group || !src || group.params.some((p: GroupParam) => p.name === paramName)) return
  const meta: any = {}
  if ((src as any).min != null) meta.min = (src as any).min
  if ((src as any).max != null) meta.max = (src as any).max
  group.params.push({ name: src.name, type: src.type, default: src.default, values: [], meta })
  group.expanded = true
  lastFocusedGroupId.value = targetId
}

/** Get pre-defined values from project param definition (for combobox suggestions) */
function getParamSuggestions(paramName: string): string[] {
  const proj = (state.newProject.params || []).find(p => p.name === paramName)
  return (proj as any)?.values || []
}

function useDefaultsForGroup(group: SweepGroup) {
  for (const p of group.params) {
    if (p.default && !p.values.includes(p.default)) {
      p.values.push(p.default)
    }
  }
}

function addAllUnused() {
  const unused = paletteParams.value.filter(p => !usedParamNames.value.has(p.name))
  if (unused.length === 0) return
  if (state.groups.length === 0) addGroup('grid')
  const targetId = lastFocusedGroupId.value || state.groups[state.groups.length - 1].id
  const group = state.groups.find((g: SweepGroup) => g.id === targetId)
  if (!group) return
  for (const src of unused) {
    if (group.params.some((p: GroupParam) => p.name === src.name)) continue
    const meta: any = {}
    if ((src as any).min != null) meta.min = (src as any).min
    if ((src as any).max != null) meta.max = (src as any).max
    group.params.push({ name: src.name, type: src.type, default: src.default, values: [], meta })
  }
  group.expanded = true
}

// ── Manual add ──

function addCustomParam() {
  const name = newParamName.value.trim()
  if (!name) return
  // Check not already in palette
  if (paletteParams.value.some(p => p.name === name)) {
    // Already exists — just send to group
    sendToGroup(name)
  } else {
    extraParams.value.push({ name, type: 'str', default: '' })
    // Auto-send to group
    sendToGroup(name)
  }
  newParamName.value = ''
}

// ── Group management ──

function addGroup(type: 'grid' | 'list') {
  const id = state.getNextGroupId()
  state.groups.push({ id: `g${id}`, type, expanded: true, params: [] })
  lastFocusedGroupId.value = `g${id}`
}

function removeGroup(id: string) {
  state.groups = state.groups.filter((g: SweepGroup) => g.id !== id)
  if (lastFocusedGroupId.value === id) {
    lastFocusedGroupId.value = state.groups.length > 0 ? state.groups[state.groups.length - 1].id : null
  }
}

// ── List mode table helpers ──

function listRowCount(group: SweepGroup): number {
  if (group.params.length === 0) return 0
  return Math.max(...group.params.map(p => p.values.length), 1)
}

function setListCell(group: SweepGroup, paramName: string, rowIdx: number, value: string) {
  const param = group.params.find((p: GroupParam) => p.name === paramName)
  if (!param) return
  // Extend values array if needed
  while (param.values.length <= rowIdx) param.values.push('')
  param.values[rowIdx] = value
}

function addListRow(group: SweepGroup) {
  for (const p of group.params) {
    p.values.push('')
  }
}

function removeListRow(group: SweepGroup, rowIdx: number) {
  for (const p of group.params) {
    if (rowIdx < p.values.length) p.values.splice(rowIdx, 1)
  }
}

function removeParamFromGroup(groupId: string, paramName: string) {
  const group = state.groups.find((g: SweepGroup) => g.id === groupId)
  if (!group) return
  group.params = group.params.filter((p: GroupParam) => p.name !== paramName)
  if (group.params.length === 0) removeGroup(groupId)
}
</script>

<style scoped>
.group-card--grid { border-left: 3px solid rgb(var(--v-theme-primary), 0.3) !important; }
.group-card--list { border-left: 3px solid rgb(var(--v-theme-success), 0.3) !important; }
.group-card--grid.group-focused { border-left-color: rgb(var(--v-theme-primary)) !important; box-shadow: 0 0 0 1px rgb(var(--v-theme-primary), 0.2); }
.group-card--list.group-focused { border-left-color: rgb(var(--v-theme-success)) !important; box-shadow: 0 0 0 1px rgb(var(--v-theme-success), 0.2); }
.param-sheet { background: rgb(var(--v-theme-surface-variant), 0.3); border: 0.5px solid rgb(var(--v-theme-surface-variant)); }
.palette-item { transition: background 0.15s ease; }
.palette-item:hover { background: rgb(var(--v-theme-surface-variant), 0.5); }
.drop-hint { border: 1.5px dashed rgb(var(--v-theme-outline-variant)); }
.list-drop-zone { display: flex; flex-wrap: wrap; min-height: 28px; padding: 4px; border-radius: 4px; border: 1px dashed rgb(var(--v-theme-outline-variant)); }
.list-drop-zone--empty { min-height: 48px; align-items: center; justify-content: center; }
.list-cell { width: 100%; border: none; background: transparent; outline: none; font-family: inherit; font-size: inherit; padding: 2px 0; border-bottom: 1px solid transparent; color: inherit; }
.list-cell:focus { border-bottom-color: rgb(var(--v-theme-success)); }
</style>
