<template>
  <div>
    <div class="d-flex align-center justify-space-between mb-4">
      <div>
        <div class="text-h5 font-weight-bold">{{ t('submit.title') }}</div>
        <div class="text-body-2 text-on-surface-variant mt-1">{{ t('submit.subtitle') }}</div>
      </div>
    </div>

    <!-- Identity bar: project → target, submit CTA. No steps — the plan
         panel answers live while params are shaped. -->
    <IdentityBar
      :total="displayTaskCount"
      :valid="validation.ok"
      :has-state="hasSubmitState"
      @select-project="selectProject"
      @submit="submit()"
      @import-yaml="showJobYamlBrowser = true"
      @reset="resetSubmitState"
    />

    <!-- Submit error (inline, not a toast — it needs room and actions) -->
    <div v-if="submitError" class="mb-3">
      <PreflightError
        v-if="isPreflightError"
        :message="submitError"
        closable
        @skip-preflight="submit(true)"
        @close="submitError = ''"
      />
      <v-alert
        v-else
        type="error" variant="tonal" density="compact" closable
        @click:close="submitError = ''"
      >
        {{ submitError }}
      </v-alert>
    </div>

    <!-- Editor + live plan: one screen, no review step -->
    <div class="d-flex ga-4 align-start submit-grid">
      <div class="flex-grow-1 min-w-0">
        <div v-if="!projectName" class="pa-8 text-center text-on-surface-variant">
          <v-icon size="36" class="mb-2" style="opacity: 0.5">mdi-folder-open-outline</v-icon>
          <div class="text-body-1">{{ t('submit.pick_project_first') }}</div>
        </div>
        <StepConfigure v-else />
      </div>
      <!-- Below md the plan STACKS under the editor instead of vanishing:
           hiding it turned "persistent review panel" into review-less
           submits on tablets (Codex r1 F4). -->
      <aside class="plan-col flex-shrink-0">
        <LivePlan />
      </aside>
    </div>

    <FileBrowserDialog
      v-model="showJobYamlBrowser"
      mode="script"
      :file-filter="'.yaml,.yml'"
      :target="target"
      @select="onJobYamlSelected"
    />
  </div>
</template>

<script setup lang="ts">
// Submit — one editor screen (RQ2-3 c2, kit ScreensSubmit):
//   · project + target live in a persistent identity bar (no Project step)
//   · the plan is a panel beside the params, not a step after them
// Defining or editing a project is its own page (/projects/:name/edit) —
// different lifetime, different destination, own URL (c1).
import { ref, computed, provide, reactive, onMounted, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { jobsApi } from '@/apis/jobs'
import { projectsApi } from '@/apis/projects'
import { filesApi } from '@/apis/files'
import { useSnackbar } from '@/composables/useSnackbar'
import { usePreferences } from '@/composables/usePreferences'
import { useConfigStore } from '@/stores/config'
import { queryClient } from '@/queries/client'
import { qk } from '@/queries/keys'
import type { ProjectSummary, ProjectConfig } from '@/types/api'

import PreflightError from '@/components/PreflightError.vue'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'
import IdentityBar from './IdentityBar.vue'
import LivePlan from './LivePlan.vue'
import StepConfigure from './StepConfigure.vue'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import { decompile, type ParamRow, type LinkSet } from './paramTable'
import {
  buildJobConfig as createJobConfig,
  dryRunHeaders as createDryRunHeaders,
  sweepSummary as summarizeSweep,
  totalTaskCount as countTotalTasks,
  validateConfigure,
} from './submitFlow'
import { normalizeParam, autoIncludeCommonParams, mergeParsedParams, inferScriptPath } from './projectParams'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const snack = useSnackbar()
const prefs = usePreferences()
const config = useConfigStore()

const projectName = ref(prefs.lastProject.value || '')
const matchedProjects = ref<ProjectSummary[]>([])
// newProject holds the SELECTED project's loaded config (param palette,
// job-name template, setup command for HF suggestions, raw source for
// read-modify-write saves). Authoring happens on the project-edit page.
const newProject = reactive({
  name: '',
  workDir: '',
  cmd: '',
  setupCmd: '',
  gpus: 1,
  maxRetry: 0,
  envType: '',
  envPath: '',
  envName: '',
  envText: '',
  jobName: '',
  params: [] as import('@/types/submit').ProjectParam[],
  source: undefined as ProjectConfig | undefined,
})
const target = ref(config.currentTarget)
const note = ref('')
const jobName = ref('') // per-submit override; '' = project default

const rows = ref<ParamRow[]>([])
const linkSets = ref<LinkSet[]>([])

const dryRunResult = ref<Record<string, any>[]>([])
const dryRunLoading = ref(false)
const dryRunError = ref('')
const noteResolved = ref('')
const submitting = ref(false)
const preflightEnabled = ref(true)

// ── Computed ──

const validation = computed(() => validateConfigure(rows.value, linkSets.value))
const totalTaskCount = computed(() => countTotalTasks(rows.value, linkSets.value))

// The local count answers per keystroke; the server plan replaces it as
// soon as it lands (and is the authority the CTA quotes). No project =
// nothing to submit — a phantom "1 task" would just be noise.
const displayTaskCount = computed(() => {
  if (!projectName.value || !validation.value.ok) return 0
  if (!dryRunLoading.value && dryRunResult.value.length > 0) return dryRunResult.value.length
  return totalTaskCount.value
})

const sweepSummary = computed(() => summarizeSweep(rows.value, linkSets.value))
const dryRunHeaders = computed(() => createDryRunHeaders(dryRunResult.value, newProject.params))

// ── Provide ──

provide(SUBMIT_STATE_KEY, reactive({
  projectName,
  matchedProjects,
  newProject,
  target,
  note,
  jobName,
  rows,
  linkSets,
  totalTaskCount,
  displayTaskCount,
  sweepSummary,
  dryRunResult,
  dryRunLoading,
  dryRunError,
  dryRunHeaders,
  noteResolved,
  submitting,
  preflightEnabled,
  prefs,
}))

// ── Project selection (the identity bar's job) ──

async function selectProject(name: string, preserveRows = false) {
  projectName.value = name
  prefs.lastProject.value = name
  if (!preserveRows) {
    rows.value = []
    linkSets.value = []
  }
  try {
    const cfg = await projectsApi.get(name)
    applyProjectConfig(cfg)
    // The project's pinned target is where it submits (registry fact).
    target.value = cfg.target || config.currentTarget
    await refreshParamsFromProject(cfg)
  } catch {
    newProject.source = undefined
  }
}

function applyProjectConfig(cfg: ProjectConfig) {
  newProject.source = cfg
  newProject.name = cfg.project_name
  newProject.workDir = cfg.working_dir
  newProject.cmd = cfg.command_template
  newProject.setupCmd = cfg.setup_command || ''
  newProject.envText = Object.entries(cfg.environment || {}).map(([k, v]) => `${k}=${v}`).join('\n')
  newProject.jobName = cfg.job_name || ''
  newProject.gpus = cfg.defaults?.gpus_per_task ?? 1
  newProject.maxRetry = cfg.defaults?.max_retry ?? 0
  newProject.envType = cfg.python_env?.type || ''
  newProject.envPath = cfg.python_env?.path || ''
  newProject.envName = cfg.python_env?.name || ''
  const rawParams = cfg.params || []
  newProject.params = rawParams.map(p => normalizeParam(p))
  if (!rawParams.some(p => p.include !== undefined)) autoIncludeCommonParams(newProject.params)
}

async function refreshParamsFromProject(cfg: ProjectConfig) {
  const path = inferScriptPath(cfg)
  if (!path) return
  try {
    const result = await filesApi.parseScript(path, { silent: true }, target.value)
    newProject.params = mergeParsedParams(newProject.params, result.args || [])
  } catch {
    // Keep persisted project.yaml params when script parsing is unavailable.
  }
}

// ── Live plan: debounced server expansion (merged dry-run + note resolve) ──

let planTimer: ReturnType<typeof setTimeout> | null = null
watch([rows, linkSets, note, jobName, projectName, target], () => {
  if (planTimer) clearTimeout(planTimer)
  if (!projectName.value) {
    dryRunResult.value = []
    noteResolved.value = ''
    return
  }
  if (!validation.value.ok) {
    dryRunResult.value = []
    dryRunError.value = ''
    return
  }
  dryRunLoading.value = true
  planTimer = setTimeout(async () => {
    try {
      const plan = await jobsApi.plan(buildJobConfig(), target.value)
      dryRunResult.value = plan.tasks
      noteResolved.value = plan.note_resolved
      dryRunError.value = ''
    } catch (e: any) {
      dryRunResult.value = []
      noteResolved.value = ''
      dryRunError.value = e?.message || t('submit.dryrun_failed')
    } finally {
      dryRunLoading.value = false
    }
  }, 500)
}, { deep: true })

function buildJobConfig() {
  const cfg = createJobConfig(projectName.value, note.value, rows.value, linkSets.value)
  const n = jobName.value.trim()
  if (n) cfg.name = n
  return cfg
}

// ── Entry points ──
// ?fromJob=<id>: re-run as template — the source job's raw config loads
// with note in {{...}} form, sweep blocks decompiled into rows/link-sets.
// ?fork=<run>: pre-fill the note so the lineage is recorded.
onMounted(async () => {
  try {
    matchedProjects.value = await projectsApi.list()
  } catch { matchedProjects.value = [] }

  const fromJob = route.query.fromJob
  const fork = typeof route.query.fork === 'string' ? route.query.fork : ''

  if (typeof fromJob === 'string' && fromJob) {
    try {
      const detail = await jobsApi.get(fromJob)
      if (!detail.config) {
        snack.warn(t('submit.no_source_config'))
      } else {
        note.value = detail.config.note || ''
        jobName.value = detail.config.name || ''
        const { rows: r, linkSets: ls } = decompile(detail.config)
        rows.value = r
        linkSets.value = ls
        await selectProject(detail.config.project || detail.job.project, true)
      }
    } catch (e: any) {
      snack.error(e?.message || t('submit.load_job_failed'))
    }
  } else if (restoreDraft()) {
    if (projectName.value) await selectProject(projectName.value, true)
  } else if (projectName.value) {
    await selectProject(projectName.value)
  }

  if (fork && !note.value) note.value = t('submit.fork_note', { run: fork })
})

// ── Import an existing job.yaml from the SERVER's filesystem ──
const showJobYamlBrowser = ref(false)

async function onJobYamlSelected(path: string) {
  showJobYamlBrowser.value = false
  const name = path.split(/[\\/]/).pop() || path
  try {
    const YAML = await import('js-yaml')
    const { content } = await filesApi.read(path, target.value)
    const cfg = YAML.load(content) as any
    if (!cfg || typeof cfg !== 'object' || (!cfg.sweep && !cfg.fixed_params)) {
      throw new Error('not a job.yaml (no sweep/fixed_params)')
    }
    note.value = cfg.note || ''
    jobName.value = cfg.name || ''
    const { rows: r, linkSets: ls } = decompile(cfg)
    rows.value = r
    linkSets.value = ls
    if (cfg.project) {
      if (matchedProjects.value.some(p => p.name === cfg.project)) {
        await selectProject(String(cfg.project), true)
      } else {
        projectName.value = String(cfg.project)
        snack.warn(t('submit.unregistered_project', { name: cfg.project }))
      }
    }
    snack.success(`Imported ${name} (${r.length} params)`)
  } catch (err: any) {
    snack.error(`Import failed: ${err?.message ?? err}`)
  }
}

const submitError = ref('')

// ── Draft insurance: survives accidental navigation, reloads, crashes ──
let draftTimer: ReturnType<typeof setTimeout> | null = null
watch([projectName, note, jobName, rows, linkSets], () => {
  if (draftTimer) clearTimeout(draftTimer)
  draftTimer = setTimeout(() => {
    if (rows.value.length === 0 && !note.value) return
    prefs.submitDraft.value = {
      projectName: projectName.value,
      note: note.value,
      jobName: jobName.value,
      rows: JSON.parse(JSON.stringify(rows.value)),
      linkSets: JSON.parse(JSON.stringify(linkSets.value)),
      ts: Date.now(),
    }
  }, 800)
}, { deep: true })

const hasSubmitState = computed(() =>
  rows.value.length > 0 || !!note.value || !!jobName.value.trim())

function resetSubmitState() {
  if (draftTimer) clearTimeout(draftTimer)
  prefs.submitDraft.value = null
  note.value = ''
  jobName.value = ''
  rows.value = []
  linkSets.value = []
  dryRunResult.value = []
  dryRunError.value = ''
  submitError.value = ''
  // The project SELECTION survives (it's a pointer, not unsaved work) —
  // re-seed its declared params into a fresh table.
  if (newProject.source) applyProjectConfig(newProject.source)
  snack.info(t('submit.state_cleared'))
}

function restoreDraft(): boolean {
  const d = prefs.submitDraft.value
  if (!d || !Array.isArray(d.rows) || d.rows.length === 0) return false
  projectName.value = d.projectName || projectName.value
  note.value = d.note || ''
  jobName.value = d.jobName || ''
  rows.value = d.rows
  linkSets.value = Array.isArray(d.linkSets) ? d.linkSets : []
  snack.info(t('submit.draft_restored'), t('submit.discard'), resetSubmitState)
  return true
}

const isPreflightError = computed(() =>
  submitError.value.includes('preflight') || submitError.value.includes('- import:') || submitError.value.includes('- pip_check:'))

async function submit(forceSkipPreflight = false) {
  submitting.value = true
  submitError.value = ''
  try {
    const res = await jobsApi.submit(buildJobConfig(), {
      preflightEnabled: preflightEnabled.value,
      forceSkipPreflight,
      target: target.value,
      timeoutMs: 50000,
      // errors render in-page (submitError) — a global snackbar on top of
      // that was double reporting
      silent: true,
    })
    snack.success(t('submit.success'))
    prefs.submitDraft.value = null
    queryClient.invalidateQueries({ queryKey: qk.jobs })
    router.push({ name: 'job-detail', params: { project: projectName.value, jobId: res.job_id } })
  } catch (e: any) {
    submitError.value = e?.message || t('common.error')
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.min-w-0 { min-width: 0; }
.submit-grid { flex-wrap: wrap; }
.plan-col { width: 320px; }
@media (max-width: 959px) {
  .plan-col { width: 100%; }
}
</style>
