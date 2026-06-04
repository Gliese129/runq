<template>
  <v-row no-gutters>
    <!-- Left panel: project info + note -->
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

      <!-- Add parameter to sweep group -->
      <v-card class="pa-3">
        <div class="text-caption text-on-surface-variant mb-2">Add parameter</div>
        <div class="d-flex ga-2">
          <v-text-field
            v-model="newParamName"
            placeholder="param name"
            density="compact"
            variant="outlined"
            hide-details
            style="font-family: monospace"
            @keydown.enter="addParamToGroup"
          />
          <v-btn
            icon size="small" variant="tonal" color="primary"
            :disabled="!newParamName.trim()"
            @click="addParamToGroup"
          >
            <v-icon size="16">mdi-plus</v-icon>
          </v-btn>
        </div>
      </v-card>
    </v-col>

    <!-- Right panel: sweep group builder -->
    <v-col cols="12" md="8">
      <div class="d-flex align-center justify-space-between mb-3">
        <div class="d-flex align-center ga-2">
          <div class="text-subtitle-2">Sweep Groups</div>
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
                  @update:model-value="group.type = $event ? 'list' : 'grid'"
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
                  Empty — add params from left panel
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
                <div v-for="p in group.params" :key="p.name" class="param-sheet pa-3 rounded mb-2">
                  <div class="d-flex align-center justify-space-between mb-1">
                    <span class="text-body-2 font-weight-medium">{{ p.name }}</span>
                    <v-btn icon size="x-small" variant="text" @click="removeParamFromGroup(group.id, p.name)">
                      <v-icon size="14" color="on-surface-variant">mdi-close</v-icon>
                    </v-btn>
                  </div>
                  <ChipInput
                    v-model="p.values"
                    :placeholder="group.type === 'grid' ? 'Type value + Enter' : 'Add values in order'"
                    :color="group.type === 'grid' ? 'primary' : 'success'"
                  />
                </div>

                <div v-if="group.params.length === 0" class="drop-hint text-center pa-6 rounded">
                  <v-icon size="20" color="on-surface-variant" class="mb-1">mdi-plus-circle-outline</v-icon>
                  <div class="text-caption text-on-surface-variant">Add params from the left panel</div>
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
import { ref, inject } from 'vue'
import { useI18n } from 'vue-i18n'
import ChipInput from '@/components/ChipInput.vue'
import { SUBMIT_STATE_KEY, type GroupParam, type SweepGroup } from '@/types/submit'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!

const newParamName = ref('')
const lastFocusedGroupId = ref<string | null>(null)

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

function removeParamFromGroup(groupId: string, paramName: string) {
  const group = state.groups.find((g: SweepGroup) => g.id === groupId)
  if (!group) return
  group.params = group.params.filter((p: GroupParam) => p.name !== paramName)
  if (group.params.length === 0) removeGroup(groupId)
}

// ── Add param ──

function addParamToGroup() {
  const name = newParamName.value.trim()
  if (!name) return

  // Ensure at least one group exists
  if (state.groups.length === 0) addGroup('grid')

  // Add to focused group or last group
  let targetId = lastFocusedGroupId.value
  if (!targetId || !state.groups.find((g: SweepGroup) => g.id === targetId)) {
    targetId = state.groups[state.groups.length - 1].id
  }
  const group = state.groups.find((g: SweepGroup) => g.id === targetId)
  if (!group) return

  // Skip if already in any group
  for (const g of state.groups) {
    if (g.params.some((p: GroupParam) => p.name === name)) {
      newParamName.value = ''
      return
    }
  }

  group.params.push({ name, type: '', default: '', values: [] })
  group.expanded = true
  lastFocusedGroupId.value = targetId
  newParamName.value = ''
}
</script>

<style scoped>
.group-card--grid { border-left: 3px solid rgb(var(--v-theme-primary)) !important; }
.group-card--list { border-left: 3px solid rgb(var(--v-theme-success)) !important; }
.group-focused { box-shadow: 0 0 0 1px rgb(var(--v-theme-primary), 0.2); }
.param-sheet { background: rgb(var(--v-theme-surface-variant), 0.3); border: 0.5px solid rgb(var(--v-theme-surface-variant)); }
.drop-hint { border: 1.5px dashed rgb(var(--v-theme-outline-variant)); }
</style>
