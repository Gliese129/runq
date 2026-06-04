<template>
  <v-row no-gutters style="max-width: 1060px; margin: 0 auto">
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

    <!-- Right: preview / create form -->
    <v-col cols="12" md="8">
      <!-- Preview existing project -->
      <v-card v-if="mode === 'select' && selectedConfig" class="pa-5">
        <div class="text-h6 font-weight-medium mb-4">{{ selectedConfig.project_name }}</div>
        <div class="d-flex flex-column ga-3">
          <div>
            <div class="text-caption text-on-surface-variant">Working directory</div>
            <code class="text-body-2">{{ selectedConfig.working_dir }}</code>
          </div>
          <div>
            <div class="text-caption text-on-surface-variant">Command template</div>
            <code class="text-body-2">{{ selectedConfig.command_template }}</code>
          </div>
          <div class="d-flex ga-6">
            <div>
              <div class="text-caption text-on-surface-variant">GPUs / task</div>
              <span class="text-body-2">{{ selectedConfig.defaults?.gpus_per_task || 1 }}</span>
            </div>
            <div>
              <div class="text-caption text-on-surface-variant">Max retry</div>
              <span class="text-body-2">{{ selectedConfig.defaults?.max_retry ?? 0 }}</span>
            </div>
            <div v-if="selectedConfig.python_env?.type">
              <div class="text-caption text-on-surface-variant">Python env</div>
              <span class="text-body-2">{{ selectedConfig.python_env.type }}{{ selectedConfig.python_env.name ? ':' + selectedConfig.python_env.name : '' }}</span>
            </div>
          </div>
          <div v-if="selectedConfig.resume?.enabled">
            <div class="text-caption text-on-surface-variant">Resume</div>
            <span class="text-body-2">Enabled{{ selectedConfig.resume.extra_args ? ` (${selectedConfig.resume.extra_args})` : '' }}</span>
          </div>
        </div>
      </v-card>

      <!-- Empty state -->
      <v-card v-else-if="mode === 'select'" class="pa-5 d-flex flex-column align-center justify-center" style="min-height: 300px">
        <v-icon size="48" color="on-surface-variant" class="mb-3">mdi-folder-open-outline</v-icon>
        <div class="text-body-1 text-on-surface-variant">Select a project or create a new one</div>
      </v-card>

      <!-- Create new project form -->
      <v-card v-else class="pa-5">
        <div class="text-subtitle-1 font-weight-medium mb-4">New Project</div>

        <!-- Script file trigger -->
        <v-text-field
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

        <v-text-field
          v-model="form.name"
          label="Project name"
          variant="outlined" density="compact" class="mb-3"
          :error-messages="form.error"
        />

        <!-- Working directory: clickable breadcrumb -->
        <div class="mb-3">
          <div class="text-caption text-on-surface-variant mb-1">Working directory</div>
          <div v-if="form.workDir" class="d-flex align-center flex-wrap ga-1 pa-2 rounded workdir-breadcrumb">
            <template v-for="(seg, i) in workDirSegments" :key="i">
              <v-icon v-if="i > 0" size="10" color="on-surface-variant">mdi-chevron-right</v-icon>
              <span
                class="text-body-2 cursor-pointer workdir-seg"
                :class="{ 'font-weight-medium text-primary': i === workDirSegments.length - 1 }"
                @click="truncateWorkDir(i)"
              >{{ seg }}</span>
            </template>
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

        <!-- Discovered params -->
        <div v-if="form.params.length > 0" class="mb-4">
          <div class="d-flex align-center justify-space-between mb-2">
            <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
              <v-icon size="12">mdi-variable</v-icon>
              Discovered parameters
              <v-chip size="x-small" variant="tonal">{{ form.params.length }}</v-chip>
            </div>
            <v-btn size="x-small" variant="text" @click="toggleAllParams">
              {{ allParamsIncluded ? 'Deselect all' : 'Select all' }}
            </v-btn>
          </div>
          <!-- Header -->
          <div class="params-list rounded">
            <div class="d-flex align-center ga-2 px-3 py-2 params-header text-caption text-on-surface-variant">
              <div style="width: 28px"></div>
              <div style="width: 120px">Name</div>
              <div style="width: 100px">Type</div>
              <div class="flex-grow-1">Default</div>
            </div>
            <!-- Rows -->
            <div style="max-height: 240px; overflow-y: auto">
              <div
                v-for="p in form.params" :key="p.name"
                class="d-flex align-center ga-2 px-3 py-1 param-row"
              >
                <v-checkbox-btn
                  v-model="p.include"
                  density="compact" hide-details
                  color="primary"
                  style="width: 28px; flex: none"
                />
                <code class="text-body-2" style="width: 120px; flex: none">{{ p.name }}</code>
                <v-select
                  v-model="p.type"
                  :items="paramTypes"
                  density="compact" variant="underlined" hide-details
                  style="width: 100px; flex: none; font-size: 13px"
                />
                <v-text-field
                  v-model="p.default"
                  placeholder="value"
                  density="compact" variant="underlined" hide-details
                  :rules="[v => validateParam(v, p.type)]"
                  class="flex-grow-1"
                  style="font-family: monospace; font-size: 13px"
                />
              </div>
            </div>
          </div>
        </div>

        <v-btn
          color="primary" variant="flat" block
          :disabled="!form.name.trim() || !form.workDir"
          :loading="form.creating"
          @click="$emit('create')"
        >
          <v-icon start size="16">mdi-check</v-icon>
          Register & Continue
        </v-btn>
      </v-card>

      <!-- File browser dialog -->
      <v-dialog v-model="showFileBrowser" max-width="720">
        <v-card class="pa-4">
          <div class="d-flex align-center justify-space-between mb-3">
            <div class="text-subtitle-1 font-weight-medium">Select Script</div>
            <v-btn icon size="small" variant="text" @click="showFileBrowser = false">
              <v-icon>mdi-close</v-icon>
            </v-btn>
          </div>

          <v-row no-gutters>
            <!-- Sidebar: favorites + recent -->
            <v-col cols="4" class="pr-3" style="max-height: 460px; overflow-y: auto">
              <!-- Pinned workspaces -->
              <div v-if="prefs.preferredWorkspaces.value.length > 0" class="mb-3">
                <div class="text-caption text-on-surface-variant mb-1 d-flex align-center ga-1">
                  <v-icon size="11">mdi-star</v-icon> Favorites
                </div>
                <div
                  v-for="ws in prefs.preferredWorkspaces.value" :key="ws"
                  class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
                  :class="{ 'bg-primary': browserCurrentDir === ws }"
                  @click="loadBrowserDir(ws)"
                >
                  <v-icon size="14" :color="browserCurrentDir === ws ? 'on-primary' : 'primary'">mdi-folder-star-outline</v-icon>
                  <span class="text-body-2 text-truncate" :class="browserCurrentDir === ws ? 'text-on-primary' : ''">
                    {{ ws.split('/').filter(Boolean).pop() || ws }}
                  </span>
                </div>
              </div>

              <!-- Recent scripts -->
              <div v-if="prefs.recentScripts.value.length > 0">
                <div class="text-caption text-on-surface-variant mb-1 d-flex align-center ga-1">
                  <v-icon size="11">mdi-clock-outline</v-icon> Recent
                </div>
                <div
                  v-for="s in prefs.recentScripts.value" :key="s.path"
                  class="quick-item d-flex align-center ga-2 pa-2 rounded cursor-pointer"
                  @click="onScriptPicked({ path: s.path, name: s.name, is_dir: false, size: 0 })"
                >
                  <v-icon size="14" color="primary">mdi-file-code-outline</v-icon>
                  <div class="flex-grow-1" style="min-width: 0">
                    <div class="text-body-2 font-weight-medium text-truncate">{{ s.name }}</div>
                    <div class="text-caption text-on-surface-variant">{{ s.project }}</div>
                  </div>
                </div>
              </div>

              <div v-if="prefs.preferredWorkspaces.value.length === 0 && prefs.recentScripts.value.length === 0"
                class="text-caption text-on-surface-variant pa-2"
              >
                No favorites or recent scripts yet
              </div>
            </v-col>

            <!-- File browser -->
            <v-col cols="8">
              <!-- Path input -->
              <v-text-field
                v-model="browserPath"
                placeholder="Paste path or browse..."
                prepend-inner-icon="mdi-link-variant"
                density="compact" variant="outlined" hide-details clearable
                class="mb-2"
                style="font-family: monospace; font-size: 12px"
                @keydown.enter="useBrowserPath"
              />

              <!-- Breadcrumb -->
              <div class="d-flex align-center flex-wrap ga-1 text-caption text-on-surface-variant mb-2">
                <span class="cursor-pointer breadcrumb-seg" @click="loadBrowserDir('')">~</span>
                <template v-for="(seg, i) in browserSegments" :key="i">
                  <v-icon size="10">mdi-chevron-right</v-icon>
                  <span
                    class="cursor-pointer breadcrumb-seg"
                    :class="{ 'font-weight-medium': i === browserSegments.length - 1 }"
                    @click="loadBrowserDir('/' + browserSegments.slice(0, i + 1).join('/'))"
                  >{{ seg }}</span>
                </template>
                <!-- Pin button -->
                <v-btn
                  v-if="browserCurrentDir"
                  icon size="x-small" variant="text"
                  :color="isBrowserDirPinned ? 'primary' : undefined"
                  @click="togglePinBrowserDir"
                  class="ml-1"
                >
                  <v-icon size="14">{{ isBrowserDirPinned ? 'mdi-star' : 'mdi-star-outline' }}</v-icon>
                </v-btn>
              </div>

              <!-- File list -->
              <v-list density="compact" class="rounded pa-0" style="max-height: 340px; overflow-y: auto">
                <v-list-item v-if="browserCurrentDir" @click="browserNavigateUp" class="rounded">
                  <template #prepend><v-icon size="16" color="on-surface-variant">mdi-arrow-up</v-icon></template>
                  <v-list-item-title class="text-body-2 text-on-surface-variant">..</v-list-item-title>
                </v-list-item>
                <v-list-item
                  v-for="entry in browserFilteredEntries" :key="entry.path"
                  @click="entry.is_dir ? loadBrowserDir(entry.path) : onScriptPicked(entry)"
                  class="rounded"
                >
                  <template #prepend>
                    <v-icon size="16" :color="entry.is_dir ? 'warning' : 'primary'">
                      {{ entry.is_dir ? 'mdi-folder' : 'mdi-file-code-outline' }}
                    </v-icon>
                  </template>
                  <v-list-item-title class="text-body-2">{{ entry.name }}</v-list-item-title>
                </v-list-item>
              </v-list>
            </v-col>
          </v-row>
        </v-card>
      </v-dialog>
    </v-col>
  </v-row>
</template>

<script setup lang="ts">
import { ref, computed, inject, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from '@/apis/client'
import { usePreferences } from '@/composables/usePreferences'
import type { FSEntry, ParseResult, ProjectConfig, ProjectSummary } from '@/types/api'
import { SUBMIT_STATE_KEY } from '@/types/submit'

const emit = defineEmits<{ create: [] }>()
const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const prefs = usePreferences()

// ── Mode ──
const mode = ref<'select' | 'create'>(state.matchedProjects.length > 0 ? 'select' : 'create')

// ── Select existing ──
const selectedConfig = ref<ProjectConfig | null>(null)

async function selectProject(name: string) {
	mode.value = 'select'
	state.projectName = name
	try {
		const cfg = await api.get<ProjectConfig>(`/projects/${encodeURIComponent(name)}`)
		selectedConfig.value = cfg
		applyProjectConfig(cfg)
		await refreshParamsFromProject(cfg)
	} catch {
		selectedConfig.value = null
	}
}

function applyProjectConfig(cfg: ProjectConfig) {
	form.name = cfg.project_name
	form.workDir = cfg.working_dir
	form.cmd = cfg.command_template
	form.gpus = cfg.defaults?.gpus_per_task || 1
	form.maxRetry = cfg.defaults?.max_retry ?? 0
	form.envType = cfg.python_env?.type || ''
	form.envPath = cfg.python_env?.path || ''
	form.envName = cfg.python_env?.name || ''
	form.error = ''
	state.groups = []
}

async function refreshParamsFromProject(cfg: ProjectConfig) {
	form.params = []
	const path = inferScriptPath(cfg)
	if (!path) return
	scriptPath.value = path
	try {
		const result = await api.post<ParseResult>('/fs/parse-script', { path }, { silent: true })
		form.params = (result.args || []).map(a => ({
			name: a.name,
			type: normalizeType(a.type),
			default: a.default || '',
			include: true,
		}))
	} catch {
		form.params = []
	}
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
      condaEnvs.value = await api.get<string[]>('/conda/envs')
      condaEnvsLoaded = true
    } catch { condaEnvs.value = [] }
    finally { condaEnvsLoading.value = false }
  }
})

// Params helpers
const paramTypes = ['int', 'float', 'string', 'bool', 'any']
const allParamsIncluded = computed(() => form.params.length > 0 && form.params.every(p => p.include))

function normalizeType(t: string): string {
  const lower = (t || '').toLowerCase()
  if (lower === 'str' || lower === 'string') return 'string'
  if (lower === 'int' || lower === 'integer') return 'int'
  if (lower === 'float' || lower === 'number') return 'float'
  if (lower === 'bool' || lower === 'boolean') return 'bool'
  if (paramTypes.includes(lower)) return lower
  return 'any'
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

async function onScriptPicked(entry: FSEntry) {
  scriptPath.value = entry.path
  showFileBrowser.value = false

  const dir = entry.path.replace(/[\\/][^\\/]+$/, '')
  const parts = dir.split(/[\\/]+/).filter(Boolean)
  if (!form.name) form.name = parts[parts.length - 1] || 'my-project'
  form.workDir = dir
  form.cmd = `python ${entry.name} {{args}}`

  // Parse script → env + params
  try {
    const result = await api.post<{
      detected_env?: string
      suggested_command: string
      args?: Array<{ name: string; type: string; default?: string }>
    }>('/fs/parse-script', { path: entry.path })

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
    form.params = (result.args || []).map(a => ({
      name: a.name,
      type: normalizeType(a.type),
      default: a.default || '',
      include: true,
    }))
  } catch {
    form.params = []
  }

  // Save to recent
  prefs.addRecentScript(entry.path, entry.name, form.name)
}

// ── File browser ──
const browserCurrentDir = ref('')
const browserEntries = ref<FSEntry[]>([])
const browserPath = ref('')

const browserSegments = computed(() =>
  browserCurrentDir.value ? browserCurrentDir.value.split('/').filter(Boolean) : []
)

const browserFilteredEntries = computed(() =>
  browserEntries.value.filter(e => !e.name.startsWith('.') && (e.is_dir || e.name.endsWith('.py')))
)

const isBrowserDirPinned = computed(() =>
  browserCurrentDir.value !== '' && prefs.preferredWorkspaces.value.includes(browserCurrentDir.value)
)

function togglePinBrowserDir() {
  if (!browserCurrentDir.value) return
  if (isBrowserDirPinned.value) prefs.removePreferredWorkspace(browserCurrentDir.value)
  else prefs.addPreferredWorkspace(browserCurrentDir.value)
}

async function loadBrowserDir(path: string) {
  browserCurrentDir.value = path
  browserPath.value = path ? path + '/' : ''
  try {
    browserEntries.value = await api.get<FSEntry[]>(`/fs/list?path=${encodeURIComponent(path)}`)
  } catch { browserEntries.value = [] }
}

function browserNavigateUp() {
  const parts = browserCurrentDir.value.split('/').filter(Boolean)
  parts.pop()
  loadBrowserDir(parts.length > 0 ? '/' + parts.join('/') : '')
}

function useBrowserPath() {
  const p = browserPath.value.trim()
  if (!p) return
  if (p.endsWith('.py')) {
    const name = p.split('/').pop() || p
    onScriptPicked({ path: p, name, is_dir: false, size: 0 })
  } else {
    loadBrowserDir(p.replace(/\/+$/, ''))
  }
}

// ── Init ──
onMounted(async () => {
  try {
    state.matchedProjects = await api.get<ProjectSummary[]>('/projects')
  } catch { state.matchedProjects = [] }

  if (state.matchedProjects.length === 0) {
    mode.value = 'create'
  } else if (state.projectName) {
    await selectProject(state.projectName)
  }

  // Pre-load browser at first favorite or home
  loadBrowserDir(prefs.preferredWorkspaces.value[0] || '')
})
</script>

<style scoped>
.quick-item { transition: background 0.15s ease; }
.quick-item:hover { background: rgb(var(--v-theme-surface-variant)); }
.breadcrumb-seg { text-decoration: underline dotted; text-underline-offset: 2px; }
.breadcrumb-seg:hover { color: rgb(var(--v-theme-primary)); }
.workdir-breadcrumb { background: rgb(var(--v-theme-surface-variant), 0.3); border: 1px solid rgb(var(--v-theme-outline-variant)); }
.workdir-seg { text-decoration: underline dotted; text-underline-offset: 2px; }
.workdir-seg:hover { color: rgb(var(--v-theme-primary)); }
.params-list { background: rgb(var(--v-theme-surface-variant), 0.2); border: 1px solid rgb(var(--v-theme-outline-variant)); overflow: hidden; }
.params-header { border-bottom: 1px solid rgb(var(--v-theme-outline-variant)); font-weight: 500; }
.param-row:hover { background: rgb(var(--v-theme-surface-variant), 0.3); }
.param-row + .param-row { border-top: 1px solid rgb(var(--v-theme-outline-variant), 0.3); }
</style>
