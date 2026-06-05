<template>
  <v-row no-gutters style="max-width: 960px; margin: 0 auto">
    <!-- Left: project list -->
    <v-col cols="12" md="4" class="pr-md-4">
      <v-card class="pa-3">
        <div class="text-caption text-on-surface-variant mb-2 d-flex align-center ga-1">
          <v-icon size="12">mdi-folder-multiple-outline</v-icon>
          Projects
        </div>

        <v-list density="compact" class="pa-0" style="max-height: calc(100vh - 340px); overflow-y: auto">
          <v-list-item
            v-for="p in state.matchedProjects"
            :key="p.name"
            :active="state.projectName === p.name && mode === 'select'"
            rounded
            class="mb-1"
            @click="selectProject(p.name)"
          >
            <template #prepend>
              <v-icon size="16" color="primary">mdi-folder-outline</v-icon>
            </template>
            <v-list-item-title class="text-body-2 font-weight-medium">{{ p.name }}</v-list-item-title>
            <v-list-item-subtitle class="text-caption">
              {{ p.job_count }} {{ p.job_count === 1 ? 'job' : 'jobs' }}
            </v-list-item-subtitle>
          </v-list-item>

          <v-list-item
            v-if="state.matchedProjects.length === 0"
            class="text-center text-on-surface-variant pa-4"
            disabled
          >
            <div class="text-caption">No projects registered</div>
          </v-list-item>
        </v-list>

        <v-divider class="my-2" />

        <v-btn
          variant="tonal" color="primary" size="small" block
          :class="{ 'v-btn--active': mode === 'create' }"
          @click="enterCreateMode"
        >
          <v-icon start size="14">mdi-plus</v-icon>
          New Project
        </v-btn>
      </v-card>
    </v-col>

    <!-- Right: unified form (select=readonly, create=editable) -->
    <v-col cols="12" md="8">
      <!-- Empty state: nothing selected -->
      <v-card v-if="mode === 'select' && !state.projectName" class="pa-5 d-flex flex-column align-center justify-center" style="min-height: 300px">
        <v-icon size="48" color="on-surface-variant" class="mb-3">mdi-folder-open-outline</v-icon>
        <div class="text-body-1 text-on-surface-variant">Select a project or create a new one</div>
      </v-card>

      <!-- Project form -->
      <v-card v-else class="pa-5">
        <div class="text-subtitle-1 font-weight-medium mb-4">
          {{ isCreating ? 'New Project' : form.name }}
        </div>

        <!-- Script file trigger (create only) -->
        <v-text-field
          v-if="isCreating"
          :model-value="scriptDisplay"
          label="Script file"
          variant="outlined" density="compact" class="mb-3"
          prepend-inner-icon="mdi-file-code-outline"
          append-inner-icon="mdi-folder-search-outline"
          readonly
          placeholder="Select a script to auto-fill..."
          @click="showFileBrowser = true"
          @click:append-inner="showFileBrowser = true"
        />

        <div class="d-flex align-center ga-2 mb-3">
          <v-text-field
            v-model="form.name"
            label="Project name"
            variant="outlined" density="compact"
            hide-details="auto"
            :error-messages="form.error"
          />
          <v-btn
            v-if="!isCreating && form.name !== state.projectName && form.name.trim()"
            size="small" variant="tonal" color="primary"
            :loading="renaming"
            @click="doRename"
          >Rename</v-btn>
        </div>

        <!-- Working directory: clickable breadcrumb -->
        <div class="mb-3">
          <div class="text-caption text-on-surface-variant mb-1">Working directory</div>
          <div v-if="form.workDir" class="d-flex align-center pa-2 rounded workdir-breadcrumb">
            <div class="d-flex align-center flex-wrap ga-1 flex-grow-1">
              <template v-for="(seg, i) in workDirSegments" :key="i">
                <v-icon v-if="i > 0" size="10" color="on-surface-variant">mdi-chevron-right</v-icon>
                <span
                  class="text-body-2"
                  :class="[
                    i === workDirSegments.length - 1 ? 'font-weight-medium text-primary' : '',
                    isCreating ? 'cursor-pointer workdir-seg' : '',
                  ]"
                  @click="isCreating && truncateWorkDir(i)"
                >{{ seg }}</span>
              </template>
            </div>
            <v-btn
              v-if="isCreating"
              icon size="x-small" variant="text"
              @click="editWorkDir"
            >
              <v-icon size="14" color="on-surface-variant">mdi-pencil-outline</v-icon>
            </v-btn>
          </div>
          <div v-else class="text-caption text-on-surface-variant pa-2">Select a script first</div>
        </div>

        <v-text-field
          v-model="form.cmd"
          label="Command template"
          variant="outlined" density="compact" class="mb-3"
          hint="{{args}} will be replaced with parameters"
          persistent-hint
        />

        <v-row dense class="mb-3">
          <v-col cols="6">
            <v-text-field
              v-model.number="form.gpus" label="GPUs per task"
              variant="outlined" density="compact" type="number" :min="0" hide-details
                              />
          </v-col>
          <v-col cols="6">
            <v-text-field
              v-model.number="form.maxRetry" label="Max retry"
              variant="outlined" density="compact" type="number" :min="0" hide-details
                />
          </v-col>
        </v-row>

        <!-- Python environment -->
        <div class="mb-3">
          <div class="text-caption text-on-surface-variant mb-1">Python environment</div>
          <v-row dense>
            <v-col cols="4">
              <v-select
                v-model="form.envType"
                :items="envTypes"
                item-title="label"
                item-value="value"
                density="compact"
                variant="outlined"
                hide-details
                placeholder="system"
                              />
            </v-col>
            <v-col cols="8">
              <!-- venv/uv: auto-detected path (editable) -->
              <v-text-field
                v-if="form.envType === 'venv' || form.envType === 'uv'"
                v-model="form.envPath"
                placeholder=".venv"
                density="compact"
                variant="outlined"
                hide-details
                prepend-inner-icon="mdi-folder-outline"
              >
                <template #append-inner>
                  <v-icon size="14" color="success" v-if="form.envPath">mdi-check-circle</v-icon>
                </template>
              </v-text-field>

              <!-- conda: select from env list or type -->
              <v-combobox
                v-else-if="form.envType === 'conda'"
                v-model="form.envName"
                :items="condaEnvs"
                :loading="condaEnvsLoading"
                density="compact"
                variant="outlined"
                hide-details
                placeholder="base"
              />

              <!-- system or empty: nothing -->
              <div v-else class="text-caption text-on-surface-variant pa-2">
                Uses system Python
              </div>
            </v-col>
          </v-row>
        </div>

        <!-- Parameters -->
        <div class="mb-4">
          <div class="d-flex align-center justify-space-between mb-2">
            <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
              <v-icon size="12">mdi-variable</v-icon>
              Parameters
              <v-chip size="x-small" variant="tonal">{{ includedCount }} / {{ form.params.length }}</v-chip>
            </div>
            <div class="d-flex align-center ga-1">
              <v-btn v-if="!compactMode && form.params.length > 0" size="x-small" variant="text" @click="toggleAllParams">
                {{ allParamsIncluded ? 'Deselect all' : 'Select all' }}
              </v-btn>
              <v-btn icon size="x-small" variant="text" @click="showParamEditor = true">
                <v-icon size="16">mdi-arrow-expand</v-icon>
              </v-btn>
            </div>
          </div>

          <div v-if="visibleParams.length > 0" class="params-list rounded">
            <div style="max-height: 320px; overflow-y: auto">
              <div v-for="p in visibleParams" :key="p.name" class="d-flex align-center ga-2 px-3 py-2 param-row">
                <v-checkbox-btn
                  v-model="p.include"
                  density="compact" hide-details color="primary"
                  style="flex: none"
                />
                <code class="text-body-2" style="min-width: 80px; flex: none">{{ p.name }}</code>
                <v-chip size="x-small" variant="tonal" class="flex-shrink-0">{{ p.type }}</v-chip>
                <v-text-field
                  v-model="p.default"
                  placeholder="default"
                  density="compact" variant="underlined" hide-details
                  class="flex-grow-1"
                  style="font-family: monospace; font-size: 13px"
                />
              </div>
            </div>
            <div v-if="hiddenCount > 0" class="d-flex align-center justify-center ga-1 pa-2 compact-hint" @click="showParamEditor = true">
              <v-icon size="12" color="on-surface-variant">mdi-dots-horizontal</v-icon>
              <span class="text-caption text-on-surface-variant">{{ hiddenCount }} more hidden —</span>
              <span class="text-caption text-primary cursor-pointer">expand to edit all</span>
            </div>
          </div>
          <div v-else-if="form.params.length === 0" class="text-caption text-on-surface-variant pa-3 text-center">
            No parameters
          </div>
        </div>

        <!-- Next button in parent handles save -->
      </v-card>

      <!-- File browser dialog -->
      <FileBrowserDialog
        v-model="showFileBrowser"
        :mode="workDirEditMode ? 'directory' : 'script'"
        :initial-dir="form.workDir"
        @select="onBrowserSelect"
      />

      <!-- Param editor full-screen dialog -->
      <ParamEditorDialog
        v-model="showParamEditor"
        :params="form.params"
        @update:params="form.params = $event"
      />
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { envApi } from '@/apis/env'
import { filesApi } from '@/apis/files'
import { projectsApi } from '@/apis/projects'
import { usePreferences } from '@/composables/usePreferences'
import type { FSEntry, ParseResult, ProjectConfig } from '@/types/api'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import ParamEditorDialog from './ParamEditorDialog.vue'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'

// No emits — save handled by parent's goNext
const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const prefs = usePreferences()

// ── Rename ──
const renaming = ref(false)

async function doRename() {
  const newName = form.name.trim()
  if (!newName || newName === state.projectName) return
  renaming.value = true
  try {
    await projectsApi.rename(state.projectName, newName)
    const proj = state.matchedProjects.find(p => p.name === state.projectName)
    if (proj) proj.name = newName
    state.projectName = newName
    prefs.lastProject.value = newName
  } catch (e: any) {
    form.error = e?.message || 'Rename failed'
  } finally {
    renaming.value = false
  }
}

// ── Param editor dialog ──
const showParamEditor = ref(false)

// ── Mode ──
const mode = ref<'select' | 'create'>('select') // updated after API load in onMounted
const isCreating = computed(() => mode.value === 'create')

// ── Select existing ──
const selectedConfig = ref<ProjectConfig | null>(null)

async function selectProject(name: string) {
	mode.value = 'select'
	state.projectName = name
	try {
		const cfg = await projectsApi.get(name)
		selectedConfig.value = cfg
		applyProjectConfig(cfg, state.groups.length === 0)
		await refreshParamsFromProject(cfg)
	} catch {
		selectedConfig.value = null
	}
}

function applyProjectConfig(cfg: ProjectConfig, resetGroups = true) {
	form.name = cfg.project_name
	form.workDir = cfg.working_dir
	form.cmd = cfg.command_template
	form.gpus = cfg.defaults?.gpus_per_task || 1
	form.maxRetry = cfg.defaults?.max_retry ?? 0
	form.envType = cfg.python_env?.type || ''
	form.envPath = cfg.python_env?.path || ''
	form.envName = cfg.python_env?.name || ''
	form.error = ''
	// Load persisted params from project config
	form.params = ((cfg as any).params || []).map((p: any) => normalizeParam(p))
	autoIncludeCommonParams()
	if (resetGroups) state.groups = []
}

async function refreshParamsFromProject(cfg: ProjectConfig) {
	const path = inferScriptPath(cfg)
	if (!path) return
	scriptPath.value = path
	try {
		const result = await filesApi.parseScript(path, { silent: true })
		mergeParsedParams(result.args || [])
		autoIncludeCommonParams()
	} catch {
		// Keep persisted project.yaml params when script parsing is unavailable.
	}
}

function mergeParsedParams(args: ParseResult['args']) {
  const existing = new Map(form.params.map(p => [p.name, p]))
  const merged: import('@/types/submit').ProjectParam[] = []

  for (const arg of args || []) {
    const discovered = normalizeParam(arg)
    const saved = existing.get(discovered.name)
    if (!saved) {
      merged.push(discovered)
      continue
    }
    existing.delete(discovered.name)
    merged.push({
      ...discovered,
      ...saved,
      values: saved.values?.length ? [...saved.values] : discovered.values ? [...discovered.values] : undefined,
      min: saved.min ?? discovered.min,
      max: saved.max ?? discovered.max,
    })
  }

  for (const saved of existing.values()) {
    merged.push(saved)
  }
  form.params = merged
}

function inferScriptPath(cfg: ProjectConfig): string {
	const cmd = cfg.command_template || ''
	const match = cmd.match(/(?:^|\s)([^\s"'`]+\.py)(?:\s|$)/)
	if (!match) return ''
	const script = match[1]
	if (script.startsWith('/')) return script
	const base = (cfg.working_dir || '').replace(/\/+$/, '')
	return base ? `${base}/${script}` : script
}

// ── Create new ──
const form = state.newProject
const scriptPath = ref('')
const showFileBrowser = ref(false)

const scriptDisplay = computed(() => {
  if (!scriptPath.value) return ''
  return scriptPath.value.split(/[\\/]/).pop() || scriptPath.value
})

// Working dir breadcrumb
const workDirSegments = computed(() =>
  form.workDir ? form.workDir.split('/').filter(Boolean) : []
)

function truncateWorkDir(index: number) {
  const segs = workDirSegments.value.slice(0, index + 1)
  form.workDir = '/' + segs.join('/')
  // Update project name suggestion to match new dir
  form.name = segs[segs.length - 1] || form.name
}

const workDirEditMode = ref(false)

function editWorkDir() {
  workDirEditMode.value = true
  showFileBrowser.value = true
}

function enterCreateMode() {
	mode.value = 'create'
	state.projectName = ''
	selectedConfig.value = null
	resetProjectForm()
	state.groups = []
}

function resetProjectForm() {
	form.name = ''
	form.workDir = ''
	form.cmd = ''
	form.gpus = 1
	form.maxRetry = 0
	form.envType = ''
	form.envPath = ''
	form.envName = ''
	form.error = ''
	form.params = []
	scriptPath.value = ''
}

// ── Python env ──
const envTypes = [
  { label: 'System', value: 'system' },
  { label: 'venv', value: 'venv' },
  { label: 'uv', value: 'uv' },
  { label: 'Conda', value: 'conda' },
]
const condaEnvs = ref<string[]>([])
const condaEnvsLoading = ref(false)
let condaEnvsLoaded = false

watch(() => form.envType, async (type) => {
  if (type === 'conda' && !condaEnvsLoaded) {
    condaEnvsLoading.value = true
    try {
      condaEnvs.value = await envApi.listCondaEnvs()
      condaEnvsLoaded = true
    } catch { condaEnvs.value = [] }
    finally { condaEnvsLoading.value = false }
  }
})

// Params helpers
const paramTypes = ['int', 'float', 'str', 'bool', 'file', 'folder', 'list']

// Common param names to auto-include in compact mode
const COMMON_PARAMS = new Set([
  'epoch', 'epochs', 'num_epochs', 'n_epochs', 'max_epochs',
  'lr', 'learning_rate', 'learning-rate',
  'bs', 'batch_size', 'batch-size', 'batch_size',
  'seed', 'num_workers', 'device', 'output', 'output_dir',
])

const compactMode = computed(() => form.params.length > 5)

const visibleParams = computed(() => {
  if (!compactMode.value) return form.params
  return form.params.filter(p => p.include)
})

const hiddenCount = computed(() => form.params.length - visibleParams.value.length)

/** Auto-select common params when > 5 params discovered.
 *  All others default to deselected — edit in expand dialog. */
function autoIncludeCommonParams() {
  if (form.params.length <= 5) return // small set: keep all included
  // Deselect all, then select common ones
  for (const p of form.params) {
    p.include = COMMON_PARAMS.has(p.name.toLowerCase())
  }
  // If nothing matched, include the first 5
  if (!form.params.some(p => p.include)) {
    for (let i = 0; i < Math.min(5, form.params.length); i++) {
      form.params[i].include = true
    }
  }
}

const includedCount = computed(() => form.params.filter(p => p.include).length)
const allParamsIncluded = computed(() => form.params.length > 0 && includedCount.value === form.params.length)

function normalizeType(t: string): string {
  const lower = (t || '').toLowerCase()
  if (lower === 'str' || lower === 'string') return 'str'
  if (lower === 'int' || lower === 'integer') return 'int'
  if (lower === 'float' || lower === 'number') return 'float'
  if (lower === 'bool' || lower === 'boolean') return 'bool'
  if (lower === 'file') return 'file'
  if (lower === 'folder' || lower === 'dir' || lower === 'directory') return 'folder'
  if (lower === 'path') return 'file'
  if (lower === 'list' || lower === 'array') return 'list'
  if (paramTypes.includes(lower)) return lower
  return 'str'
}

/** Build a ProjectParam from a parsed arg, moving default into values for str/file/folder. */
function normalizeParam(a: { name: string; type: string; default?: string; choices?: string[]; min?: number; max?: number }): import('@/types/submit').ProjectParam {
  const type = normalizeType(a.type)
  const def = a.default || ''
  // Read choices from backend → values in frontend
  const values = Array.isArray((a as any).choices) ? (a as any).choices.map(String) : []
  // str/file/folder: ensure default is in values[0]
  if (['str', 'file', 'folder'].includes(type) && def && !values.includes(def)) {
    values.unshift(def)
  }
  return {
    name: a.name, type, default: def, include: true,
    values: values.length > 0 ? values : undefined,
    min: a.min, max: a.max,
  }
}

function validateParam(value: string, type: string): boolean | string {
  if (!value) return true
  switch (type) {
    case 'int':
      return Number.isInteger(Number(value)) || 'Must be an integer'
    case 'float':
      return !isNaN(Number(value)) || 'Must be a number'
    case 'bool':
      return ['true', 'false', '0', '1'].includes(value.toLowerCase()) || 'Must be true/false'
    default:
      return true
  }
}

function toggleAllParams() {
  const newVal = !allParamsIncluded.value
  for (const p of form.params) p.include = newVal
}

function onBrowserSelect(path: string) {
  if (workDirEditMode.value) {
    form.workDir = path
    workDirEditMode.value = false
    return
  }
  // Script selected
  const name = path.split(/[\\/]/).pop() || path
  onScriptPicked({ path, name, is_dir: false, size: 0 })
}

async function onScriptPicked(entry: FSEntry) {
  workDirEditMode.value = false
  scriptPath.value = entry.path
  showFileBrowser.value = false

  const dir = entry.path.replace(/[\\/][^\\/]+$/, '')
  const parts = dir.split(/[\\/]+/).filter(Boolean)
  if (!form.name) form.name = parts[parts.length - 1] || 'my-project'
  form.workDir = dir
  form.cmd = `python ${entry.name} {{args}}`

  // Parse script → env + params
  try {
    const result = await filesApi.parseScript(entry.path)

    if (result.suggested_command) form.cmd = result.suggested_command

    // Parse detected env string like "conda:base" or "uv:.venv"
    if (result.detected_env) {
      const [type, detail] = result.detected_env.split(':', 2)
      form.envType = type || ''
      if (type === 'conda') {
        form.envName = detail || 'base'
        form.envPath = ''
      } else if (type === 'venv' || type === 'uv') {
        form.envPath = detail || '.venv'
        form.envName = ''
      }
    }

    // Populate discovered params
    form.params = (result.args || []).map(a => normalizeParam(a))
    autoIncludeCommonParams()
  } catch {
    form.params = []
  }

  // Save to recent
  prefs.addRecentScript(entry.path, entry.name, form.name)
}


// ── Init ──
onMounted(async () => {
  // Refresh project list from backend
  try {
    state.matchedProjects = await projectsApi.list()
  } catch { state.matchedProjects = [] }

  // If form already has data (user came back), preserve state
  const hasFormData = !!form.name
  if (state.matchedProjects.length === 0) {
    mode.value = 'create'
  } else {
    mode.value = 'select'
    if (state.projectName && !hasFormData) {
      await selectProject(state.projectName)
    } else if (state.projectName) {
      // Came back — keep form, just restore mode + selectedConfig
      mode.value = 'select'
      try {
        selectedConfig.value = await projectsApi.get(state.projectName)
      } catch { selectedConfig.value = null }
    }
  }

})
</script>

<style scoped>
.workdir-breadcrumb { background: rgb(var(--v-theme-surface-variant), 0.3); border: 1px solid rgb(var(--v-theme-outline-variant)); }
.workdir-seg { text-decoration: underline dotted; text-underline-offset: 2px; }
.workdir-seg:hover { color: rgb(var(--v-theme-primary)); }
.params-list { background: rgb(var(--v-theme-surface-variant), 0.2); border: 1px solid rgb(var(--v-theme-outline-variant)); overflow: hidden; }
.param-row:hover { background: rgb(var(--v-theme-surface-variant), 0.3); }
.param-row + .param-row { border-top: 1px solid rgb(var(--v-theme-outline-variant), 0.3); }
.compact-hint { border-top: 1px solid rgb(var(--v-theme-outline-variant), 0.3); cursor: pointer; transition: background 0.15s ease; }
.compact-hint:hover { background: rgb(var(--v-theme-surface-variant), 0.3); }
</style>
