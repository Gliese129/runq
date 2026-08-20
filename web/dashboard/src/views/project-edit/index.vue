<template>
  <div>
    <!-- Own chrome: back, what this is, what it will change, one commit (kit ScreensProjectEdit) -->
    <div class="d-flex align-center ga-2 mb-4 flex-wrap">
      <v-btn icon size="small" variant="text" :aria-label="t('common.back')" :title="t('common.back')" @click="cancel">
        <v-icon size="18">mdi-arrow-left</v-icon>
      </v-btn>
      <v-icon size="17" color="primary">{{ isCreating ? 'mdi-folder-plus-outline' : 'mdi-pencil-outline' }}</v-icon>
      <span class="text-subtitle-1 font-weight-medium">{{ isCreating ? t('projectEdit.title_new') : (form.name || project) }}</span>
      <span class="text-caption text-on-surface-variant hidden-xs" style="border-left: 0.5px solid rgb(var(--v-theme-outline-variant)); padding-left: 10px">
        {{ isCreating ? t('projectEdit.subtitle_new') : changeSummary }}
      </span>
      <v-chip v-if="!isCreating && changedLabels.length > 0" size="x-small" variant="tonal" color="primary">
        {{ t('projectEdit.n_edited', { n: changedLabels.length }) }}
      </v-chip>
      <v-spacer />
      <span class="text-caption text-on-surface-variant">⌘S</span>
      <v-btn size="small" variant="text" @click="cancel">{{ t('common.cancel') }}</v-btn>
      <v-btn size="small" variant="tonal" color="primary" :loading="saving" :disabled="!canSave" @click="save">
        {{ saveLabel }}
      </v-btn>
    </div>

    <!-- Create resting state: script picker first (a project starts from its script) -->
    <v-card v-if="isCreating && !scriptPicked && !form.name" class="pa-5 mx-auto" style="max-width: 640px">
      <div class="script-drop d-flex flex-column align-center justify-center pa-8 rounded mb-4 cursor-pointer" @click="showFileBrowser = true">
        <v-icon size="36" color="primary" class="mb-2">mdi-file-code-outline</v-icon>
        <div class="text-body-1 font-weight-medium">{{ t('submit.pick_script') }}</div>
        <div class="text-caption text-on-surface-variant mt-1">{{ t('submit.script_autodetect') }}</div>
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
        <v-btn size="x-small" variant="text" @click="scriptPicked = true">{{ t('submit.skip_manual') }}</v-btn>
      </div>
    </v-card>

    <!-- The editor: sticky section rail + one visible column (tabs would hide changes) -->
    <div v-else class="d-flex ga-4 mx-auto" style="max-width: 920px">
      <nav class="rail flex-shrink-0 d-none d-md-flex flex-column">
        <button
          v-for="s in railSections" :key="s.id"
          type="button" class="rail-item d-flex align-center ga-2"
          :class="{ 'rail-item--active': activeSection === s.id }"
          @click="jumpTo(s.id)"
        >
          <span class="flex-grow-1 text-left">{{ s.label }}</span>
          <span v-if="s.changed" class="rail-dot" :title="t('projectEdit.changed')" />
        </button>
      </nav>

      <div class="flex-grow-1 min-w-0">
        <!-- ── Identity ── -->
        <section :id="secId('identity')" class="form-section">
          <div class="section-head d-flex align-baseline ga-2">
            <span class="section-title">{{ t('projectEdit.section_identity') }}</span>
            <span v-if="fieldChanged('name')" class="rail-dot" />
          </div>
          <div class="d-flex align-center ga-2">
            <v-text-field
              v-model="form.name"
              :label="t('submit.project_name')"
              variant="outlined" density="compact" hide-details="auto"
              :error-messages="form.error"
              :readonly="!isCreating"
            />
            <v-btn v-if="!isCreating" size="small" variant="text" @click="renameDialog = true">{{ t('submit.rename') }}</v-btn>
          </div>
        </section>

        <!-- ── Source ── -->
        <section :id="secId('source')" class="form-section">
          <div class="section-head d-flex align-baseline ga-2">
            <span class="section-title">{{ t('projectEdit.section_source') }}</span>
            <span v-if="fieldChanged('workDir') || fieldChanged('cmd') || fieldChanged('setupCmd')" class="rail-dot" />
            <v-spacer />
            <span class="text-caption text-on-surface-variant">{{ t('projectEdit.source_hint') }}</span>
          </div>

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
              <v-btn icon size="x-small" variant="text" :aria-label="t('common.edit')" :title="t('common.edit')" @click="editWorkDir">
                <v-icon size="14" color="on-surface-variant">mdi-pencil-outline</v-icon>
              </v-btn>
            </div>
            <div v-else class="text-caption text-on-surface-variant pa-2">{{ t('submit.select_script_first') }}</div>
          </div>

          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.cmd_template') }}</div>
          <div class="tmpl-display rounded pa-2 mb-1 cursor-pointer d-flex align-center ga-2" @click="cmdEditorOpen = true">
            <span class="font-mono flex-grow-1 text-body-2" :class="{ 'opacity-50': !form.cmd }" style="word-break: break-all" v-text="displayCmdTemplate" />
            <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
          </div>
          <div class="text-caption text-on-surface-variant mb-3">
            <span v-text="'{{args}}'" /> {{ t('submit.args_replaced') }}
          </div>
          <ShellTemplateEditor
            v-model="cmdEditorOpen"
            :value="form.cmd"
            title="command_template"
            :placeholders="cmdPlaceholders"
            @apply="form.cmd = $event"
          />

          <v-text-field
            v-model="form.setupCmd"
            :label="t('submit.setup_cmd')"
            variant="outlined" density="compact"
            placeholder="e.g. hf download {{model}}"
            :hint="t('submit.setup_cmd_hint')"
            persistent-hint
          />
        </section>

        <!-- ── Environment ── -->
        <section :id="secId('environment')" class="form-section">
          <div class="section-head d-flex align-baseline ga-2">
            <span class="section-title">{{ t('projectEdit.section_environment') }}</span>
            <span v-if="fieldChanged('envText') || fieldChanged('envType') || fieldChanged('envPath') || fieldChanged('envName')" class="rail-dot" />
          </div>

          <EnvKVEditor v-model="form.envText" :hint="t('submit.env_hint')" />

          <!-- .env is a fact about the tree, not a form field: report what's
               there (key NAMES only — values never travel to the browser list). -->
          <div class="d-flex align-start ga-2 pa-3 rounded mb-3" :class="dotenv.found ? 'dotenv-found' : 'dotenv-missing'">
            <v-icon size="14" :color="dotenv.found ? 'success' : 'on-surface-variant'" class="mt-1">
              {{ dotenv.found ? 'mdi-key-outline' : 'mdi-key-remove' }}
            </v-icon>
            <div class="flex-grow-1">
              <template v-if="dotenv.found">
                <div class="text-body-2">{{ t('projectEdit.env_found') }}</div>
                <div class="d-flex align-center flex-wrap ga-1 mt-1">
                  <v-chip v-for="k in dotenv.keys" :key="k" size="x-small" variant="tonal" class="font-mono">{{ k }}</v-chip>
                  <span v-if="dotenv.keys.length === 0" class="text-caption text-on-surface-variant">{{ t('projectEdit.env_no_keys') }}</span>
                </div>
              </template>
              <template v-else>
                <div class="text-body-2 text-on-surface-variant">{{ t('projectEdit.env_none') }}</div>
                <div class="text-caption text-on-surface-variant mt-1">
                  {{ t('projectEdit.env_create_hint_pre') }} <code>{{ (form.workDir || '<workdir>') + '/.env' }}</code> {{ t('projectEdit.env_create_hint_post') }}
                </div>
              </template>
            </div>
          </div>

          <div class="text-caption text-on-surface-variant mb-1">{{ t('submit.python_env') }}</div>
          <v-row dense>
            <v-col cols="4">
              <v-select
                v-model="form.envType" :items="envTypes" item-title="label" item-value="value"
                density="compact" variant="outlined" hide-details placeholder="system"
              />
            </v-col>
            <v-col cols="8">
              <v-text-field
                v-if="form.envType === 'venv' || form.envType === 'uv'"
                v-model="form.envPath" placeholder=".venv"
                density="compact" variant="outlined" hide-details prepend-inner-icon="mdi-folder-outline"
              >
                <template #append-inner>
                  <v-icon v-if="form.envPath" size="14" color="success">mdi-check-circle</v-icon>
                </template>
              </v-text-field>
              <v-combobox
                v-else-if="form.envType === 'conda'"
                v-model="form.envName" :items="condaEnvs" :loading="condaEnvsLoading"
                density="compact" variant="outlined" hide-details placeholder="base"
              />
              <div v-else class="text-caption text-on-surface-variant pa-2">{{ t('submit.system_python') }}</div>
            </v-col>
          </v-row>
        </section>

        <!-- ── Run defaults ── -->
        <section :id="secId('defaults')" class="form-section">
          <div class="section-head d-flex align-baseline ga-2">
            <span class="section-title">{{ t('projectEdit.section_defaults') }}</span>
            <span v-if="fieldChanged('gpus') || fieldChanged('maxRetry') || fieldChanged('jobName')" class="rail-dot" />
          </div>
          <v-row dense class="mb-3">
            <v-col cols="6">
              <v-number-input
                v-model="form.gpus" :label="t('submit.gpus_per_task')"
                variant="outlined" density="compact" :min="0" control-variant="stacked"
                :hint="config.isPoll ? t('submit.gpus_hpc_hint') : ''"
                :persistent-hint="config.isPoll" :hide-details="!config.isPoll"
              />
            </v-col>
            <v-col cols="6">
              <v-number-input
                v-model="form.maxRetry" :label="t('submit.max_retry')"
                variant="outlined" density="compact" :min="-1" hide-details control-variant="stacked"
              />
            </v-col>
          </v-row>
          <v-text-field
            v-model="form.jobName"
            :label="t('submit.job_name_tmpl')"
            variant="outlined" density="compact" class="font-mono"
            placeholder="rq-{{task_id}}"
            :hint="t('submit.job_name_tmpl_hint')"
            persistent-hint
          />
        </section>

        <!-- ── Parameters ── -->
        <section :id="secId('params')" class="form-section">
          <div class="section-head d-flex align-baseline ga-2">
            <span class="section-title">{{ t('projectEdit.section_params') }}</span>
            <span v-if="paramsChanged" class="rail-dot" />
            <v-spacer />
            <span class="text-caption text-on-surface-variant">{{ t('projectEdit.params_hint', { n: form.params.length }) }}</span>
          </div>
          <div class="d-flex align-center justify-space-between mb-2">
            <div class="text-caption text-on-surface-variant d-flex align-center ga-1">
              <v-icon size="12">mdi-variable</v-icon>
              {{ t('submit.parameters') }}
              <v-chip size="x-small" variant="tonal">{{ includedParams.length }} / {{ form.params.length }}</v-chip>
            </div>
            <v-btn size="x-small" variant="tonal" color="primary" @click="showParamEditor = true">
              <v-icon start size="12">mdi-pencil-outline</v-icon> {{ t('submit.edit_params') }}
            </v-btn>
          </div>
          <div class="d-flex align-center flex-wrap ga-1">
            <v-chip
              v-for="p in includedParams" :key="p.name"
              size="x-small" variant="tonal" class="font-mono" @click="showParamEditor = true"
            >{{ p.name }}<span v-if="p.default" class="text-on-surface-variant">&nbsp;= {{ p.default }}</span></v-chip>
            <span v-if="form.params.length === 0" class="text-caption text-on-surface-variant">{{ t('submit.no_params_hint') }}</span>
          </div>
        </section>
      </div>
    </div>

    <!-- Footer fact (edit mode): config changes never rewrite history -->
    <div v-if="!isCreating" class="d-flex align-center ga-2 mt-6 pt-3 mx-auto text-caption text-on-surface-variant" style="max-width: 920px; border-top: 0.5px solid rgb(var(--v-theme-outline-variant))">
      <v-icon size="12">mdi-information-outline</v-icon>
      <span>{{ t('projectEdit.footer_note') }}</span>
    </div>

    <!-- Rename confirm dialog -->
    <v-dialog v-model="renameDialog" max-width="380">
      <v-card class="pa-4">
        <div class="text-subtitle-2 mb-3">{{ t('submit.rename_project') }}</div>
        <v-text-field v-model="renameTo" :label="t('submit.new_name')" variant="outlined" density="compact" hide-details autofocus />
        <div class="d-flex justify-end ga-2 mt-4">
          <v-btn size="small" variant="text" @click="renameDialog = false">{{ t('common.cancel') }}</v-btn>
          <v-btn size="small" variant="tonal" color="primary" :loading="renaming" :disabled="!renameTo.trim() || renameTo.trim() === project" @click="doRename">{{ t('submit.rename') }}</v-btn>
        </div>
      </v-card>
    </v-dialog>

    <FileBrowserDialog
      v-model="showFileBrowser"
      :mode="workDirEditMode ? 'directory' : 'script'"
      :file-filter="'.py,.sh,.yaml,.yml'"
      :initial-dir="form.workDir"
      @select="onBrowserSelect"
    />

    <ParamEditorDialog
      v-model="showParamEditor"
      :params="form.params"
      @update:params="onParamsEdited"
    />
  </div>
</template>

<script setup lang="ts">
// ProjectEdit — the project config editor as its own route (RQ2-3 c1, kit
// ScreensProjectEdit). A project is persistent config with its own lifetime;
// a submit is one run. They were briefly the same screen and it made "new
// project" read as a step on the way to submitting. This page has its own
// URL, its own chrome, and its own exit: nothing here is a step in another
// flow.
//
//   /projects/new                         → create
//   /projects/:project/edit               → edit, exit to the project page
//   ...?redirect=submit                   → exit into submit instead
import { ref, computed, reactive, watch, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute, onBeforeRouteLeave } from 'vue-router'
import { useI18n } from 'vue-i18n'
import * as YAML from 'js-yaml'
import { envApi } from '@/apis/env'
import { filesApi } from '@/apis/files'
import { projectsApi } from '@/apis/projects'
import { usePreferences } from '@/composables/usePreferences'
import { useConfirm } from '@/composables/useConfirm'
import { useSnackbar } from '@/composables/useSnackbar'
import { useConfigStore } from '@/stores/config'
import type { FSEntry, ProjectConfig } from '@/types/api'
import type { ProjectParam } from '@/types/submit'
import { buildProjectPayload } from '../submit/submitFlow'
import { normalizeParam, autoIncludeCommonParams, mergeParsedParams as mergeParams, inferScriptPath } from '../submit/projectParams'
import ParamEditorDialog from './ParamEditorDialog.vue'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'
import ShellTemplateEditor from '@/components/ShellTemplateEditor.vue'
import EnvKVEditor from '@/components/EnvKVEditor.vue'

const props = defineProps<{ project?: string }>()

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const prefs = usePreferences()
const confirm = useConfirm()
const snack = useSnackbar()
const config = useConfigStore()

const isCreating = computed(() => !props.project)
const redirectTo = computed(() => (route.query.redirect === 'submit' ? 'submit' : 'project'))

// ── Form state (same shape buildProjectPayload consumes) ──
const form = reactive({
  name: '',
  workDir: '',
  cmd: '',
  setupCmd: '',
  envText: '',
  jobName: '',
  gpus: 1,
  maxRetry: 0,
  envType: '',
  envPath: '',
  envName: '',
  error: '',
  params: [] as ProjectParam[],
})
const source = ref<ProjectConfig | undefined>(undefined)

// ── Dirty tracking: field-level diff against the applied baseline ──
// (kit diffForm: the header says WHAT will change, not just that something
// did — "Will change command, parameters".)
const FIELD_KEYS = ['name', 'workDir', 'cmd', 'setupCmd', 'envText', 'jobName', 'gpus', 'maxRetry', 'envType', 'envPath', 'envName'] as const
type FieldKey = (typeof FIELD_KEYS)[number]

function fieldSnapshot(): Record<FieldKey, string> {
  const out = {} as Record<FieldKey, string>
  for (const k of FIELD_KEYS) out[k] = String(form[k] ?? '')
  return out
}
const baseline = ref(fieldSnapshot())
const baselineParams = ref('[]')

function markClean() {
  baseline.value = fieldSnapshot()
  baselineParams.value = JSON.stringify(form.params)
}
function fieldChanged(k: FieldKey): boolean {
  return String(form[k] ?? '') !== baseline.value[k]
}
const paramsChanged = computed(() => JSON.stringify(form.params) !== baselineParams.value)

const FIELD_LABEL_KEYS: Record<FieldKey, string> = {
  name: 'projectEdit.field_name',
  workDir: 'submit.workdir',
  cmd: 'projectEdit.field_command',
  setupCmd: 'submit.setup_cmd',
  envText: 'projectEdit.field_environment',
  jobName: 'submit.job_name_tmpl',
  gpus: 'submit.gpus_per_task',
  maxRetry: 'submit.max_retry',
  envType: 'submit.python_env',
  envPath: 'submit.python_env',
  envName: 'submit.python_env',
}
const changedLabels = computed(() => {
  const out: string[] = []
  for (const k of FIELD_KEYS) if (fieldChanged(k)) out.push(t(FIELD_LABEL_KEYS[k]))
  if (paramsChanged.value) out.push(t('projectEdit.field_params'))
  return [...new Set(out)]
})
const dirty = computed(() => changedLabels.value.length > 0)
const changeSummary = computed(() =>
  changedLabels.value.length > 0
    ? t('projectEdit.will_change', { fields: changedLabels.value.join(', ') })
    : t('projectEdit.no_changes'))

const canSave = computed(() => !!form.name.trim() && (isCreating.value || dirty.value) && !saving.value)
const saveLabel = computed(() => {
  if (isCreating.value) return t('projectEdit.create')
  return redirectTo.value === 'submit' ? t('projectEdit.save_continue') : t('projectEdit.save')
})

// ── Section rail (scroll-spy over the visible column) ──
const RAIL_IDS = ['identity', 'source', 'environment', 'defaults', 'params'] as const
function secId(s: string): string { return `pe-sec-${s}` }
const activeSection = ref(secId('identity'))
const railSections = computed(() => [
  { id: secId('identity'), label: t('projectEdit.section_identity'), changed: fieldChanged('name') },
  { id: secId('source'), label: t('projectEdit.section_source'), changed: fieldChanged('workDir') || fieldChanged('cmd') || fieldChanged('setupCmd') },
  { id: secId('environment'), label: t('projectEdit.section_environment'), changed: fieldChanged('envText') || fieldChanged('envType') || fieldChanged('envPath') || fieldChanged('envName') },
  { id: secId('defaults'), label: t('projectEdit.section_defaults'), changed: fieldChanged('gpus') || fieldChanged('maxRetry') || fieldChanged('jobName') },
  { id: secId('params'), label: t('projectEdit.section_params'), changed: paramsChanged.value },
])

function onScroll() {
  let cur = secId(RAIL_IDS[0])
  for (const s of RAIL_IDS) {
    const el = document.getElementById(secId(s))
    if (el && el.getBoundingClientRect().top <= 140) cur = secId(s)
  }
  // The last section can never reach the top — at the bottom of the page it
  // is what you're looking at, so claim it.
  if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) {
    cur = secId(RAIL_IDS[RAIL_IDS.length - 1])
  }
  activeSection.value = cur
}
function jumpTo(id: string) {
  document.getElementById(id)?.scrollIntoView({ block: 'start' })
}

// ── Keyboard: ⌘S saves, Esc leaves (full-page editor, not a card) ──
function onKeydown(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    if (canSave.value) save()
  } else if (e.key === 'Escape' && !document.querySelector('input:focus, textarea:focus, select:focus, .v-overlay--active')) {
    cancel()
  }
}

onMounted(() => {
  window.addEventListener('scroll', onScroll, true)
  window.addEventListener('keydown', onKeydown)
})
onUnmounted(() => {
  window.removeEventListener('scroll', onScroll, true)
  window.removeEventListener('keydown', onKeydown)
})

// Leaving with unsaved edits is a data-loss moment on a full-page editor.
onBeforeRouteLeave(async () => {
  if (!dirty.value || saving.value) return true
  return confirm.confirm({
    title: t('projectEdit.discard_title'),
    body: t('projectEdit.discard_body'),
    confirmText: t('projectEdit.discard_confirm'),
    danger: true,
  })
})

// ── Load (edit mode) ──
onMounted(async () => {
  if (!props.project) return
  try {
    const cfg = await projectsApi.get(props.project)
    applyProjectConfig(cfg)
    await refreshParamsFromProject(cfg)
  } catch (e: any) {
    form.error = e?.message || t('common.error')
  }
})

function applyProjectConfig(cfg: ProjectConfig) {
  source.value = cfg
  form.name = cfg.project_name
  form.workDir = cfg.working_dir
  form.cmd = cfg.command_template
  form.setupCmd = cfg.setup_command || ''
  form.envText = Object.entries(cfg.environment || {}).map(([k, v]) => `${k}=${v}`).join('\n')
  form.jobName = cfg.job_name || ''
  // ?? not ||: gpus_per_task 0 (CPU-only) is a legal stored value.
  form.gpus = cfg.defaults?.gpus_per_task ?? 1
  form.maxRetry = cfg.defaults?.max_retry ?? 0
  form.envType = cfg.python_env?.type || ''
  form.envPath = cfg.python_env?.path || ''
  form.envName = cfg.python_env?.name || ''
  form.error = ''
  const rawParams = cfg.params || []
  form.params = rawParams.map(p => normalizeParam(p))
  if (!rawParams.some(p => p.include !== undefined)) autoIncludeCommonParams(form.params)
  markClean()
}

async function refreshParamsFromProject(cfg: ProjectConfig) {
  const path = inferScriptPath(cfg)
  if (!path) return
  scriptPath.value = path
  try {
    const result = await filesApi.parseScript(path, { silent: true })
    form.params = mergeParams(form.params, result.args || [])
    markClean()
  } catch {
    // Keep persisted project.yaml params when script parsing is unavailable.
  }
}

// ── Save / exit ──
const saving = ref(false)

function exitTo(name: string) {
  if (redirectTo.value === 'submit') {
    prefs.lastProject.value = name
    router.push({ name: 'submit' })
  } else if (name) {
    router.push({ name: 'project', params: { project: name } })
  } else {
    router.push({ name: 'overview' })
  }
}

async function save() {
  form.error = ''
  saving.value = true
  try {
    const payload = buildProjectPayload(form, source.value)
    const name = form.name.trim()
    if (isCreating.value) {
      await projectsApi.create(payload)
      snack.success(t('submit.project_registered', { name }))
    } else {
      await projectsApi.update(name, payload)
      snack.success(t('projectEdit.saved'))
    }
    prefs.lastProject.value = name
    markClean()
    exitTo(name)
  } catch (e: any) {
    form.error = e?.message || t('common.error')
  } finally {
    saving.value = false
  }
}

function cancel() {
  exitTo(props.project || '')
}

// ── Rename (explicit dialog — name field is read-only in edit mode) ──
const renameDialog = ref(false)
const renameTo = ref('')
const renaming = ref(false)
watch(renameDialog, v => { if (v) renameTo.value = props.project || '' })

async function doRename() {
  const newName = renameTo.value.trim()
  if (!newName || newName === props.project) return
  renaming.value = true
  try {
    await projectsApi.rename(props.project!, newName)
    prefs.lastProject.value = newName
    renameDialog.value = false
    // The route param is the project's identity — follow the rename.
    router.replace({ name: 'project-edit', params: { project: newName }, query: route.query })
    form.name = newName
    baseline.value.name = newName
  } catch (e: any) {
    form.error = e?.message || t('common.error')
  } finally {
    renaming.value = false
  }
}

// ── Script picking (create mode) ──
const scriptPath = ref('')
const scriptPicked = ref(false)
const showFileBrowser = ref(false)
const detectedSummary = ref('')
const workDirEditMode = ref(false)

const scriptSummary = computed(() => {
  if (!scriptPath.value) return ''
  const name = scriptPath.value.split(/[\\/]/).pop() || scriptPath.value
  return detectedSummary.value ? `${name} · ${detectedSummary.value}` : name
})

const workDirSegments = computed(() => (form.workDir ? form.workDir.split('/').filter(Boolean) : []))

function truncateWorkDir(index: number) {
  const segs = workDirSegments.value.slice(0, index + 1)
  form.workDir = '/' + segs.join('/')
  if (isCreating.value) form.name = segs[segs.length - 1] || form.name
}

function editWorkDir() {
  workDirEditMode.value = true
  showFileBrowser.value = true
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

async function importProjectYaml(path: string, name: string) {
  showFileBrowser.value = false
  try {
    const { content } = await filesApi.read(path)
    const parsed = YAML.load(content) as any
    if (!parsed || typeof parsed !== 'object' || !parsed.project_name) {
      throw new Error('not a project.yaml (missing project_name)')
    }
    applyProjectConfig(parsed)
    // Imported ≠ saved: reset the baseline to empty so everything counts as
    // a pending change and Save registers it.
    baseline.value = {} as any
    for (const k of FIELD_KEYS) baseline.value[k] = ''
    baselineParams.value = '[]'
    scriptPicked.value = true
    detectedSummary.value = t('projectEdit.imported_from', { name })
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
  const isShell = entry.name.endsWith('.sh')
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
    autoIncludeCommonParams(form.params)
    const bits = [`${form.params.length} params`]
    if (result.detected_env) bits.push(`${result.detected_env} detected`)
    detectedSummary.value = bits.join(' · ')
  } catch {
    form.params = []
    detectedSummary.value = ''
  }
  prefs.addRecentScript(entry.path, entry.name, form.name)
}

// ── .env detection: a fact about the tree, reported with key NAMES only ──
const dotenv = reactive({ found: false, keys: [] as string[] })
let dotenvTimer: ReturnType<typeof setTimeout> | null = null
watch(() => form.workDir, dir => {
  dotenv.found = false
  dotenv.keys = []
  if (dotenvTimer) clearTimeout(dotenvTimer)
  if (!dir) return
  dotenvTimer = setTimeout(async () => {
    try {
      const entries = await filesApi.list(dir)
      const hit = entries.find(e => !e.is_dir && e.name === '.env')
      if (!hit) return
      dotenv.found = true
      const { content } = await filesApi.read(hit.path)
      dotenv.keys = content
        .split('\n')
        .map(l => l.trim())
        .filter(l => l && !l.startsWith('#') && l.includes('='))
        .map(l => l.slice(0, l.indexOf('=')).trim())
        .filter(Boolean)
    } catch {
      // Missing dir / unreadable file: the card simply reports "no .env".
    }
  }, 400)
}, { immediate: true })

// ── Params helpers ──
const showParamEditor = ref(false)
const includedParams = computed(() => form.params.filter(p => p.include))

function onParamsEdited(params: ProjectParam[]) {
  form.params = params
}

const cmdEditorOpen = ref(false)
const cmdPlaceholders = computed(() => ['args', ...form.params.map(p => p.name).filter(Boolean)])
const defaultCmdTemplate = 'python train.py {{args}}'
const displayCmdTemplate = computed(() => form.cmd || defaultCmdTemplate)

const envTypes = [
  { label: 'System', value: 'system' },
  { label: 'venv', value: 'venv' },
  { label: 'uv', value: 'uv' },
  { label: 'Conda', value: 'conda' },
]
const condaEnvs = ref<string[]>([])
const condaEnvsLoading = ref(false)
let condaEnvsLoaded = false
watch(() => form.envType, async type => {
  if (type === 'conda' && !condaEnvsLoaded) {
    condaEnvsLoading.value = true
    try {
      condaEnvs.value = await envApi.listCondaEnvs()
      condaEnvsLoaded = true
    } catch { condaEnvs.value = [] }
    finally { condaEnvsLoading.value = false }
  }
})

</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.min-w-0 { min-width: 0; }

.rail {
  position: sticky;
  top: 72px;
  align-self: flex-start;
  width: 160px;
  gap: 2px;
}
.rail-item {
  padding: 5px 8px;
  border-radius: var(--radius);
  border: none;
  background: transparent;
  cursor: pointer;
  font-size: 12.5px;
  color: rgb(var(--v-theme-on-surface-variant));
  transition: var(--transition-color);
}
.rail-item:hover { background: rgb(var(--v-theme-surface-variant), 0.5); }
.rail-item--active {
  color: rgb(var(--v-theme-primary));
  font-weight: 500;
  background: rgb(var(--v-theme-primary), 0.08);
}
.rail-dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: rgb(var(--v-theme-primary));
  flex-shrink: 0;
  display: inline-block;
}

.form-section { margin-bottom: 20px; scroll-margin-top: 72px; }
.section-head {
  padding-bottom: 6px;
  margin-bottom: 12px;
  border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant));
}
.section-title {
  font-size: 12px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: rgb(var(--v-theme-on-surface-variant));
}

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
.dotenv-found { background: rgb(var(--v-theme-success), 0.06); border: 0.5px solid rgb(var(--v-theme-success), 0.3); }
.dotenv-missing { background: rgb(var(--v-theme-surface-variant), 0.25); border: 0.5px solid rgb(var(--v-theme-outline-variant)); }
</style>
