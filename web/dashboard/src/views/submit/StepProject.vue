<template>
  <v-row no-gutters style="max-width: 960px; margin: 0 auto">
    <!-- Left: project list -->
    <v-col cols="12" md="4" class="pr-md-4">
      <v-card class="pa-3">
        <div class="text-caption text-on-surface-variant mb-2 d-flex align-center ga-1">
          <v-icon size="12">mdi-folder-multiple-outline</v-icon>
          {{ t('submit.projects') }}
        </div>

        <v-text-field
          v-if="state.matchedProjects.length > 8"
          v-model="projectFilter"
          :placeholder="t('submit.filter')"
          prepend-inner-icon="mdi-magnify"
          density="compact" variant="outlined" hide-details clearable
          class="mb-2"
        />

        <v-list density="compact" class="pa-0" role="listbox" :aria-label="t('submit.projects')" style="max-height: calc(100vh - 380px); overflow-y: auto">
          <v-list-item
            v-for="p in sortedProjects"
            :key="p.name"
            :active="state.projectName === p.name && mode === 'select'"
            color="primary"
            role="option"
            :aria-selected="state.projectName === p.name && mode === 'select'"
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
              <span v-if="p.name === prefs.lastProject.value" class="text-primary"> · recent</span>
            </v-list-item-subtitle>
          </v-list-item>

          <v-list-item
            v-if="state.matchedProjects.length === 0"
            class="text-center text-on-surface-variant pa-4"
            disabled
          >
            <div class="text-caption">{{ t('submit.no_projects') }}</div>
          </v-list-item>
        </v-list>

        <v-divider class="my-2" />

        <v-btn
          variant="tonal" color="primary" size="small" block
          :class="{ 'v-btn--active': mode === 'create' }"
          @click="enterCreateMode"
        >
          <v-icon start size="14">mdi-plus</v-icon>
          {{ t('submit.new_project') }}
        </v-btn>
      </v-card>
    </v-col>

    <!-- Right -->
    <v-col cols="12" md="8">
      <!-- Empty state: nothing selected -->
      <v-card v-if="mode === 'select' && !state.projectName" class="pa-5 d-flex flex-column align-center justify-center" style="min-height: 300px">
        <v-icon size="48" color="on-surface-variant" class="mb-3">mdi-folder-open-outline</v-icon>
        <div class="text-body-1 text-on-surface-variant">{{ t('submit.pick_from_left') }}</div>
      </v-card>

      <!-- ═══ Path A: read-only summary card (the frequent path) ═══ -->
      <v-card v-else-if="mode === 'select' && !editingProject" class="pa-5">
        <div class="d-flex align-center flex-wrap ga-2 mb-4">
          <span class="text-subtitle-1 font-weight-medium">{{ form.name }}</span>
          <v-chip size="x-small" variant="tonal">{{ envSummary }}</v-chip>
          <v-chip size="x-small" variant="tonal">{{ form.gpus }} GPU{{ form.gpus === 1 ? '' : 's' }}</v-chip>
          <v-chip size="x-small" variant="tonal">retry {{ form.maxRetry }}</v-chip>
        </div>

        <table class="summary-table">
          <tbody>
            <tr>
              <td class="summary-label">workdir</td>
              <td class="font-mono text-body-2 text-on-surface-variant text-truncate" style="max-width: 420px">{{ form.workDir || '—' }}</td>
            </tr>
            <tr>
              <td class="summary-label">command</td>
              <td class="font-mono text-body-2 text-on-surface-variant">{{ form.cmd || '—' }}</td>
            </tr>
            <tr>
              <td class="summary-label" style="vertical-align: top">params</td>
              <td>
                <div class="d-flex align-center flex-wrap ga-1">
                  <v-chip
                    v-for="p in includedParams.slice(0, 8)" :key="p.name"
                    size="x-small" variant="tonal" class="font-mono"
                  >{{ p.name }}</v-chip>
                  <span v-if="includedParams.length > 8" class="text-caption text-on-surface-variant">
                    +{{ includedParams.length - 8 }} more
                  </span>
                  <span v-if="includedParams.length === 0" class="text-caption text-on-surface-variant">none</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div class="d-flex align-center justify-space-between mt-4 pt-3" style="border-top: 0.5px solid rgb(var(--v-theme-outline-variant))">
          <v-btn size="small" variant="text" color="primary" @click="enterEditMode">
            <v-icon start size="14">mdi-pencil-outline</v-icon> {{ t('submit.edit_project') }}
          </v-btn>
          <span class="text-caption text-on-surface-variant">{{ t('submit.read_only') }}</span>
        </div>
      </v-card>

      <!-- ═══ Path B resting state: script picker first ═══ -->
      <v-card v-else-if="isCreating && !scriptPicked && !form.name" class="pa-5">
        <div
          class="script-drop d-flex flex-column align-center justify-center pa-8 rounded mb-4 cursor-pointer"
          @click="showFileBrowser = true"
        >
          <v-icon size="36" color="primary" class="mb-2">mdi-file-code-outline</v-icon>
          <div class="text-body-1 font-weight-medium">{{ t('submit.pick_script') }}</div>
          <div class="text-caption text-on-surface-variant mt-1">
            {{ t('submit.script_autodetect') }}
          </div>
        </div>

        <template v-if="prefs.recentScripts.value.length > 0">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.recent_caps') }}</div>
          <div
            v-for="s in prefs.recentScripts.value.slice(0, 5)" :key="s.path"
            class="d-flex align-center ga-2 px-2 py-1 rounded recent-row cursor-pointer"
            @click="onBrowserSelect(s.path)"
          >
            <v-icon size="14" color="on-surface-variant">mdi-history</v-icon>
            <code class="text-body-2">{{ s.name }}</code>
            <span class="text-caption text-on-surface-variant text-truncate">{{ s.path }}</span>
          </div>
        </template>

        <div class="text-center mt-3">
          <v-btn size="x-small" variant="text" @click="skipScript">
            {{ t('submit.skip_manual') }}
          </v-btn>
        </div>
      </v-card>

      <!-- ═══ Edit form (create after script, or Edit project) ═══ -->
      <v-card v-else class="pa-5">
        <div class="d-flex align-center justify-space-between mb-4">
          <div class="text-subtitle-1 font-weight-medium">
            {{ isCreating ? t('submit.new_project') : form.name }}
          </div>
          <v-btn v-if="!isCreating" size="x-small" variant="text" @click="editingProject = false">
            <v-icon start size="12">mdi-chevron-up</v-icon> {{ t('submit.collapse') }}
          </v-btn>
        </div>

        <!-- Script summary line (create only) -->
        <v-text-field
          v-if="isCreating"
          :model-value="scriptSummary"
          :label="t('submit.script_file')"
          variant="outlined" density="compact" class="mb-3"
          prepend-inner-icon="mdi-file-code-outline"
          append-inner-icon="mdi-folder-search-outline"
          readonly
          :placeholder="t('submit.script_autofill')"
          @click="showFileBrowser = true"
          @click:append-inner="showFileBrowser = true"
        />

        <div class="d-flex align-center ga-2 mb-3">
          <v-text-field
            v-model="form.name"
            :label="t('submit.project_name')"
            variant="outlined" density="compact"
            hide-details="auto"
            :error-messages="form.error"
            :readonly="!isCreating"
          />
          <v-btn
            v-if="!isCreating"
            size="small" variant="text"
            @click="renameDialog = true"
          >{{ t('submit.rename') }}</v-btn>
        </div>

        <!-- Working directory: clickable breadcrumb -->
        <div class="mb-3">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.workdir') }}</div>
          <div v-if="form.workDir" class="d-flex align-center pa-2 rounded workdir-breadcrumb">
            <div class="d-flex align-center flex-wrap ga-1 flex-grow-1">
              <template v-for="(seg, i) in workDirSegments" :key="i">
                <v-icon v-if="i > 0" size="10" color="on-surface-variant">mdi-chevron-right</v-icon>
                <span
                  class="text-body-2 cursor-pointer workdir-seg"
                  :class="i === workDirSegments.length - 1 ? 'font-weight-medium text-primary' : ''"
                  @click="truncateWorkDir(i)"
                >{{ seg }}</span>
              </template>
            </div>
            <v-btn icon size="x-small" variant="text" @click="editWorkDir">
              <v-icon size="14" color="on-surface-variant">mdi-pencil-outline</v-icon>
            </v-btn>
          </div>
          <div v-else class="text-caption text-on-surface-variant pa-2">{{ t('submit.select_script_first') }}</div>
        </div>

        <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.cmd_template') }}</div>
        <div class="tmpl-display rounded pa-2 mb-1 cursor-pointer d-flex align-center ga-2" @click="cmdEditorOpen = true">
          <span
            class="font-mono flex-grow-1 text-body-2"
            :class="{ 'opacity-50': !form.cmd }"
            style="word-break: break-all"
            v-text="displayCmdTemplate"
          />
          <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
        </div>
        <div class="text-caption text-on-surface-variant mb-3">
          <span v-text="argsPlaceholder" /> will be replaced with parameters
        </div>
        <ShellTemplateEditor
          v-model="cmdEditorOpen"
          :value="form.cmd"
          title="command_template"
          :placeholders="cmdPlaceholders"
          @apply="form.cmd = $event"
        />

        <EnvKVEditor
          v-model="form.envText"
          :hint="t('submit.env_hint')"
        />

        <v-text-field
          v-model="form.setupCmd"
          :label="t('submit.setup_cmd')"
          variant="outlined" density="compact" class="mb-3"
          placeholder="e.g. hf download {{model}}"
          :hint="t('submit.setup_cmd_hint')"
          persistent-hint
        />

        <v-text-field
          v-model="form.jobName"
          :label="t('submit.job_name_tmpl')"
          variant="outlined" density="compact" class="mb-3 font-mono"
          :placeholder="jobNamePlaceholderLiteral"
          :hint="jobNameHint"
          persistent-hint
        />

        <v-row dense class="mb-3">
          <v-col cols="6">
            <v-number-input
              v-model="form.gpus" :label="t('submit.gpus_per_task')"
              variant="outlined" density="compact" :min="0"
              control-variant="stacked"
              :hint="config.isPoll ? 'HPC: only used as {{gpus}} in submit_template — set 0 for whole-node queues' : ''"
              :persistent-hint="config.isPoll"
              :hide-details="!config.isPoll"
            />
          </v-col>
          <v-col cols="6">
            <v-number-input
              v-model="form.maxRetry" :label="t('submit.max_retry')"
              variant="outlined" density="compact" :min="0" hide-details
              control-variant="stacked"
            />
          </v-col>
        </v-row>

        <!-- Python environment -->
        <div class="mb-3">
          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.python_env') }}</div>
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

              <div v-else class="text-caption text-on-surface-variant pa-2">
                Uses system Python
              </div>
            </v-col>
          </v-row>
        </div>

        <!-- Parameters: chips summary, single editor = the dialog -->
        <div class="mb-2">
          <div class="d-flex align-center justify-space-between mb-2">
            <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
              <v-icon size="12">mdi-variable</v-icon>
              Parameters
              <v-chip size="x-small" variant="tonal">{{ includedParams.length }} / {{ form.params.length }}</v-chip>
            </div>
            <v-btn size="x-small" variant="tonal" color="primary" @click="showParamEditor = true">
              <v-icon start size="12">mdi-pencil-outline</v-icon> Edit parameters
            </v-btn>
          </div>
          <div class="d-flex align-center flex-wrap ga-1">
            <v-chip
              v-for="p in includedParams" :key="p.name"
              size="x-small" variant="tonal" class="font-mono"
              @click="showParamEditor = true"
            >{{ p.name }}<span v-if="p.default" class="text-on-surface-variant">&nbsp;= {{ p.default }}</span></v-chip>
            <span v-if="form.params.length === 0" class="text-caption text-on-surface-variant">
              No parameters — select a script or add them in the editor
            </span>
          </div>
        </div>
      </v-card>

      <!-- Rename confirm dialog -->
      <v-dialog v-model="renameDialog" max-width="380">
        <v-card class="pa-4">
          <div class="text-subtitle-2 mb-3">{{ t('submit.rename_project') }}</div>
          <v-text-field
            v-model="renameTo" :label="t('submit.new_name')"
            variant="outlined" density="compact" hide-details autofocus
          />
          <div class="d-flex justify-end ga-2 mt-4">
            <v-btn size="small" variant="text" @click="renameDialog = false">{{ t('common.cancel') }}</v-btn>
            <v-btn size="small" variant="tonal" color="primary" :loading="renaming" :disabled="!renameTo.trim() || renameTo.trim() === state.projectName" @click="doRename">{{ t('submit.rename') }}</v-btn>
          </div>
        </v-card>
      </v-dialog>

      <!-- File browser dialog -->
      <FileBrowserDialog
        v-model="showFileBrowser"
        :mode="workDirEditMode ? 'directory' : 'script'"
        :file-filter="'.py,.sh,.yaml,.yml'"
        :initial-dir="form.workDir"
        @select="onBrowserSelect"
      />

      <!-- Param editor full-screen dialog -->
      <ParamEditorDialog
        v-model="showParamEditor"
        :params="form.params"
        @update:params="onParamsEdited"
      />
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject, watch, onMounted } from 'vue'
import * as YAML from 'js-yaml'
import { envApi } from '@/apis/env'
import { filesApi } from '@/apis/files'
import { projectsApi } from '@/apis/projects'
import { useI18n } from 'vue-i18n'
import { usePreferences } from '@/composables/usePreferences'
import { useConfigStore } from '@/stores/config'
import type { FSEntry, ParseResult, ProjectConfig } from '@/types/api'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import ParamEditorDialog from './ParamEditorDialog.vue'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'
import ShellTemplateEditor from '@/components/ShellTemplateEditor.vue'
import EnvKVEditor from '@/components/EnvKVEditor.vue'

// No emits — save handled by parent's goNext (only when dirty; see below)
const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const prefs = usePreferences()
const config = useConfigStore()

// ── Mode ──
const mode = ref<'select' | 'create'>('select')
const isCreating = computed(() => mode.value === 'create')
// Path A is read-only by default; this expands the edit form.
const editingProject = ref(false)

// ── Dirty tracking ──
// goNext saves the project ONLY when something was actually edited (or in
// create mode). Selecting a project and clicking Next is side-effect free.
let applying = false
watch(
  () => [form_(), state.newProject.params],
  () => { if (!applying) state.newProject.dirty = true },
  { deep: true },
)
function form_() {
  const { name, workDir, cmd, setupCmd, envText, jobName, gpus, maxRetry, envType, envPath, envName } = state.newProject
  return { name, workDir, cmd, setupCmd, envText, jobName, gpus, maxRetry, envType, envPath, envName }
}

function enterEditMode() {
  editingProject.value = true
}

// ── Project list ──
const projectFilter = ref('')
const sortedProjects = computed(() => {
  const q = projectFilter.value.trim().toLowerCase()
  // Archived projects don't belong in the submit picker — unarchive first.
  let list = state.matchedProjects.filter(p => !p.archived)
  if (q) list = list.filter(p => p.name.toLowerCase().includes(q))
  // Recent project pinned first, rest by job count desc
  return [...list].sort((a, b) => {
    if (a.name === prefs.lastProject.value) return -1
    if (b.name === prefs.lastProject.value) return 1
    return b.job_count - a.job_count
  })
})

// ── Rename (explicit dialog — name field is read-only in select mode) ──
const renameDialog = ref(false)
const renameTo = ref('')
const renaming = ref(false)

watch(renameDialog, (v) => { if (v) renameTo.value = state.projectName })

async function doRename() {
  const newName = renameTo.value.trim()
  if (!newName || newName === state.projectName) return
  renaming.value = true
  try {
    await projectsApi.rename(state.projectName, newName)
    const proj = state.matchedProjects.find(p => p.name === state.projectName)
    if (proj) proj.name = newName
    state.projectName = newName
    applying = true
    form.name = newName
    applying = false
    prefs.lastProject.value = newName
    renameDialog.value = false
  } catch (e: any) {
    form.error = e?.message || 'Rename failed'
  } finally {
    renaming.value = false
  }
}

// ── Param editor dialog ──
const showParamEditor = ref(false)

// ── Command template editor: args + every defined param name ──
const cmdEditorOpen = ref(false)
const cmdPlaceholders = computed(() => [
  'args',
  ...form.params.map(p => p.name).filter(Boolean),
])
const defaultCmdTemplate = 'python train.py {{args}}'
const argsPlaceholder = '{{args}}'
const jobNamePlaceholderLiteral = 'rq-{{task_id}}'
const jobNameHint = 'Scheduler job name ({{name}} in submit_template) - params + {{project}} {{job_id}} {{task_id}}. Sanitized automatically (never starts with a digit). Each submit can override it.'
const displayCmdTemplate = computed(() => form.cmd || defaultCmdTemplate)

function onParamsEdited(params: import('@/types/submit').ProjectParam[]) {
  form.params = params
  state.newProject.dirty = true
}

// ── Select existing ──
const selectedConfig = ref<ProjectConfig | null>(null)

async function selectProject(name: string) {
  mode.value = 'select'
  editingProject.value = false
  state.projectName = name
  try {
    const cfg = await projectsApi.get(name)
    selectedConfig.value = cfg
    applyProjectConfig(cfg, state.rows.length === 0)
    await refreshParamsFromProject(cfg)
  } catch {
    selectedConfig.value = null
  }
}

function applyProjectConfig(cfg: ProjectConfig, resetGroups = true) {
  applying = true
  form.name = cfg.project_name
  form.workDir = cfg.working_dir
  form.cmd = cfg.command_template
  form.setupCmd = cfg.setup_command || ''
  form.envText = Object.entries(cfg.environment || {}).map(([k, v]) => `${k}=${v}`).join('\n')
  form.jobName = cfg.job_name || ''
  form.gpus = cfg.defaults?.gpus_per_task || 1
  form.maxRetry = cfg.defaults?.max_retry ?? 0
  form.envType = cfg.python_env?.type || ''
  form.envPath = cfg.python_env?.path || ''
  form.envName = cfg.python_env?.name || ''
  form.error = ''
  const rawParams = ((cfg as any).params || []) as any[]
  form.params = rawParams.map(p => normalizeParam(p))
  // First-time heuristic ONLY: once any include flag has been persisted,
  // the user's curation is the truth — never clobber it.
  if (!rawParams.some(p => p.include !== undefined)) {
    autoIncludeCommonParams()
  }
  state.newProject.dirty = false
  applying = false
  if (resetGroups) { state.rows = []; state.linkSets = [] }
}

async function refreshParamsFromProject(cfg: ProjectConfig) {
  const path = inferScriptPath(cfg)
  if (!path) return
  scriptPath.value = path
  try {
    const result = await filesApi.parseScript(path, { silent: true })
    applying = true
    mergeParsedParams(result.args || [])
    state.newProject.dirty = false
    applying = false
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
const scriptPicked = ref(false)
const detectedSummary = ref('')

const scriptSummary = computed(() => {
  if (!scriptPath.value) return ''
  const name = scriptPath.value.split(/[\\/]/).pop() || scriptPath.value
  return detectedSummary.value ? `${name} · ${detectedSummary.value}` : name
})

function skipScript() {
  scriptPicked.value = true // show the form without a script
}


// Working dir breadcrumb
const workDirSegments = computed(() =>
  form.workDir ? form.workDir.split('/').filter(Boolean) : []
)

function truncateWorkDir(index: number) {
  const segs = workDirSegments.value.slice(0, index + 1)
  form.workDir = '/' + segs.join('/')
  if (isCreating.value) form.name = segs[segs.length - 1] || form.name
}

const workDirEditMode = ref(false)

function editWorkDir() {
  workDirEditMode.value = true
  showFileBrowser.value = true
}

function enterCreateMode() {
  mode.value = 'create'
  editingProject.value = false
  state.projectName = ''
  selectedConfig.value = null
  resetProjectForm()
  state.rows = []; state.linkSets = []
}

function resetProjectForm() {
  applying = true
  form.name = ''
  form.workDir = ''
  form.cmd = ''
  form.setupCmd = ''
  form.envText = ''
  form.jobName = ''
  form.gpus = 1
  form.maxRetry = 0
  form.envType = ''
  form.envPath = ''
  form.envName = ''
  form.error = ''
  form.params = []
  state.newProject.dirty = false
  applying = false
  scriptPath.value = ''
  scriptPicked.value = false
  detectedSummary.value = ''
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

const envSummary = computed(() => {
  switch (form.envType) {
    case 'conda': return `conda: ${form.envName || 'base'}`
    case 'venv': return `venv: ${form.envPath || '.venv'}`
    case 'uv': return `uv: ${form.envPath || '.venv'}`
    default: return 'system python'
  }
})

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

const COMMON_PARAMS = new Set([
  'epoch', 'epochs', 'num_epochs', 'n_epochs', 'max_epochs',
  'lr', 'learning_rate', 'learning-rate',
  'bs', 'batch_size', 'batch-size',
  'seed', 'num_workers', 'device', 'output', 'output_dir',
])

const includedParams = computed(() => form.params.filter(p => p.include))

/** Auto-select common params when > 5 params discovered. */
function autoIncludeCommonParams() {
  if (form.params.length <= 5) return
  for (const p of form.params) {
    p.include = COMMON_PARAMS.has(p.name.toLowerCase())
  }
  if (!form.params.some(p => p.include)) {
    for (let i = 0; i < Math.min(5, form.params.length); i++) {
      form.params[i].include = true
    }
  }
}

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

/** Build a ProjectParam from a parsed arg or persisted def, moving default
 *  into values for str/file/folder. Persisted `include` is the user's
 *  curation and survives; absent include = never curated. */
function normalizeParam(a: { name: string; type: string; default?: string; choices?: string[]; min?: number; max?: number; include?: boolean }): import('@/types/submit').ProjectParam {
  const type = normalizeType(a.type)
  const def = a.default || ''
  const values = Array.isArray((a as any).choices) ? (a as any).choices.map(String) : []
  if (['str', 'file', 'folder'].includes(type) && def && !values.includes(def)) {
    values.unshift(def)
  }
  return {
    name: a.name, type, default: def, include: a.include ?? true,
    values: values.length > 0 ? values : undefined,
    min: a.min, max: a.max,
    strict: (a as any).strict || undefined,
    scope: (a as any).scope || undefined,
  }
}

function onBrowserSelect(path: string) {
  if (workDirEditMode.value) {
    form.workDir = path
    workDirEditMode.value = false
    return
  }
  const name = path.split(/[\\/]/).pop() || path
  if (/\.ya?ml$/.test(name)) {
    importProjectYaml(path, name)
    return
  }
  onScriptPicked({ path, name, is_dir: false, size: 0 })
}

// Import a project.yaml living on the SERVER's filesystem (login node) —
// read via fs/read, fill the form, mark dirty so Next registers it.
async function importProjectYaml(path: string, name: string) {
  showFileBrowser.value = false
  try {
    const { content } = await filesApi.read(path)
    const parsed = YAML.load(content) as any
    if (!parsed || typeof parsed !== 'object' || !parsed.project_name) {
      throw new Error('not a project.yaml (missing project_name)')
    }
    applyProjectConfig(parsed, state.rows.length === 0)
    state.newProject.dirty = true // imported ≠ saved: Next will register it
    scriptPicked.value = true
    detectedSummary.value = `imported from ${name}`
    prefs.addRecentScript(path, name, form.name)
  } catch (err: any) {
    form.error = `Import failed: ${err?.message ?? err}`
    scriptPicked.value = true
  }
}

async function onScriptPicked(entry: FSEntry) {
  workDirEditMode.value = false
  scriptPath.value = entry.path
  scriptPicked.value = true
  showFileBrowser.value = false

  const dir = entry.path.replace(/[\\/][^\\/]+$/, '')
  const parts = dir.split(/[\\/]+/).filter(Boolean)
  if (!form.name) form.name = parts[parts.length - 1] || 'my-project'
  form.workDir = dir
  const isShell = /\.sh$/.test(entry.name)
  form.cmd = isShell ? `bash ${entry.name} {{args}}` : `python ${entry.name} {{args}}`

  try {
    const result = await filesApi.parseScript(entry.path)

    if (result.suggested_command) form.cmd = result.suggested_command

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

    form.params = (result.args || []).map(a => normalizeParam(a))
    autoIncludeCommonParams()

    const bits = [`${form.params.length} params`]
    if (result.detected_env) bits.push(`${result.detected_env} detected`)
    detectedSummary.value = bits.join(' · ')
  } catch {
    form.params = []
    detectedSummary.value = ''
  }

  prefs.addRecentScript(entry.path, entry.name, form.name)
}

// ── Init ──
onMounted(async () => {
  try {
    state.matchedProjects = await projectsApi.list()
  } catch { state.matchedProjects = [] }

  const hasFormData = !!form.name
  if (state.matchedProjects.length === 0) {
    mode.value = 'create'
  } else {
    mode.value = 'select'
    if (state.projectName && !hasFormData) {
      await selectProject(state.projectName)
    } else if (state.projectName) {
      mode.value = 'select'
      try {
        selectedConfig.value = await projectsApi.get(state.projectName)
      } catch { selectedConfig.value = null }
    }
  }
})
</script>

<style scoped>
.font-mono { font-family: monospace; }
.summary-table { width: 100%; border-collapse: collapse; }
.summary-table td { padding: 4px 0; }
.summary-label { width: 90px; font-size: 12px; color: rgb(var(--v-theme-on-surface-variant)); vertical-align: middle; }
.workdir-breadcrumb { background: rgb(var(--v-theme-surface-variant), 0.3); border: 1px solid rgb(var(--v-theme-outline-variant)); }
.workdir-seg { text-decoration: underline dotted; text-underline-offset: 2px; }
.workdir-seg:hover { color: rgb(var(--v-theme-primary)); }
.script-drop { border: 1.5px dashed rgb(var(--v-theme-outline-variant)); transition: border-color 0.15s ease, background 0.15s ease; }
.script-drop:hover { border-color: rgb(var(--v-theme-primary)); background: rgb(var(--v-theme-primary), 0.04); }
.recent-row:hover { background: rgb(var(--v-theme-surface-variant), 0.4); }
.tmpl-display {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface-variant), 0.2);
  transition: border-color 0.15s ease, background 0.15s ease;
  min-height: 38px;
}
.tmpl-display:hover {
  border-color: rgb(var(--v-theme-primary));
  background: rgb(var(--v-theme-surface-variant), 0.35);
}
</style>
