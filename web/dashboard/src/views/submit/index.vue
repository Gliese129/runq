<template>
  <div>
    <!-- Header + Step indicators -->
    <div style="max-width: 960px; margin: 0 auto">
      <div class="d-flex align-center justify-space-between mb-4">
        <div>
          <div class="text-h5 font-weight-bold">{{ t('submit.title') }}</div>
          <div class="text-body-2 text-on-surface-variant mt-1">{{ t('submit.subtitle') }}</div>
        </div>
        <div class="d-flex align-center ga-2">
          <v-btn size="x-small" variant="text" @click="showJobYamlBrowser = true">
            <v-icon start size="12">mdi-file-import-outline</v-icon> import job.yaml
          </v-btn>
          <FileBrowserDialog
            v-model="showJobYamlBrowser"
            mode="script"
            :file-filter="'.yaml,.yml'"
            @select="onJobYamlSelected"
          />
          <v-chip v-if="step >= 2 && displayTaskCount > 0" variant="tonal" color="primary">
            {{ displayTaskCount }} {{ displayTaskCount === 1 ? 'task' : 'tasks' }}
          </v-chip>
        </div>
      </div>

      <div class="d-flex ga-2 mb-5">
        <div v-for="(_, i) in steps" :key="i"
          class="flex-grow-1 rounded-pill"
          :style="{
            height: '4px',
            background: step > i
              ? 'rgb(var(--v-theme-primary))'
              : step === i
                ? 'rgb(var(--v-theme-primary), 0.4)'
                : 'rgb(var(--v-theme-surface-variant))',
            transition: 'background 0.3s ease',
          }"
        />
      </div>
    </div>

    <KeepAlive>
      <component :is="stepComponents[step]" :key="step" />
    </KeepAlive>

    <!-- Navigation -->
    <div class="d-flex align-center mt-5 ga-2" style="max-width: 960px; margin: 0 auto">
      <v-btn v-if="step > 0" variant="text" @click="step--">
        <v-icon start>mdi-arrow-left</v-icon>
        {{ t('common.back') }}
      </v-btn>
      <v-spacer />
      <v-btn
        v-if="step < 2"
        color="primary"
        variant="tonal"
        :disabled="step === 0 && !projectName && !newProject.name.trim()"
        @click="goNext"
      >
        {{ t('common.next') }}
        <v-icon end>mdi-arrow-right</v-icon>
      </v-btn>
      <v-btn
        v-else-if="step === 2"
        color="primary"
        variant="flat"
        size="large"
        :loading="submitting"
        :disabled="dryRunResult.length === 0"
        @click="submit()"
      >
        <v-icon start>mdi-rocket-launch-outline</v-icon>
        {{ t('submit.submit') }} ({{ dryRunResult.length }} tasks)
      </v-btn>
    </div>

    <!-- Submit error -->
    <div v-if="submitError && step === 2" class="mt-3" style="max-width: 960px; margin: 0 auto">
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
  </div>
</template>

<script setup lang="ts">
import { ref, computed, provide, reactive, markRaw, onMounted, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { jobsApi } from '@/apis/jobs'
import { projectsApi } from '@/apis/projects'
import { filesApi } from '@/apis/files'
import { useSnackbar } from '@/composables/useSnackbar'
import { usePreferences } from '@/composables/usePreferences'
import type { ProjectSummary } from '@/types/api'

import PreflightError from '@/components/PreflightError.vue'
import FileBrowserDialog from '@/components/FileBrowserDialog.vue'
import StepProject from './StepProject.vue'
import StepConfigure from './StepConfigure.vue'
import StepReview from './StepReview.vue'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import { decompile, type ParamRow, type LinkSet } from './paramTable'
import {
  buildJobConfig as createJobConfig,
  buildProjectPayload as createProjectPayload,
  dryRunHeaders as createDryRunHeaders,
  sweepSummary as summarizeSweep,
  totalTaskCount as countTotalTasks,
  validateConfigure,
} from './submitFlow'

const stepComponents = [markRaw(StepProject), markRaw(StepConfigure), markRaw(StepReview)]

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const snack = useSnackbar()
const prefs = usePreferences()

const steps = ['project', 'configure', 'review']
const step = ref(0)

const projectName = ref(prefs.lastProject.value || '')
const matchedProjects = ref<ProjectSummary[]>([])
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
  creating: false,
  error: '',
  params: [] as import('@/types/submit').ProjectParam[],
  dirty: false,
})
const note = ref('')
const jobName = ref('') // per-submit override; '' = project default

const rows = ref<ParamRow[]>([])
const linkSets = ref<LinkSet[]>([])

const dryRunResult = ref<Record<string, any>[]>([])
const dryRunLoading = ref(false)
const dryRunError = ref('')
const submitting = ref(false)
const preflightEnabled = ref(true)

// ── Computed ──

const totalTaskCount = computed(() => countTotalTasks(rows.value, linkSets.value))

const displayTaskCount = computed(() => {
  if (step.value === 2 && !dryRunLoading.value) return dryRunResult.value.length
  return totalTaskCount.value
})

const sweepSummary = computed(() => summarizeSweep(rows.value, linkSets.value))

const dryRunHeaders = computed(() => {
  return createDryRunHeaders(dryRunResult.value, newProject.params)
})

// ── Provide ──

provide(SUBMIT_STATE_KEY, reactive({
  step,
  projectName,
  matchedProjects,
  newProject,
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
  submitting,
  preflightEnabled,
  prefs,
}))

// ── Actions ──

function buildProjectPayload() {
  return createProjectPayload(newProject)
}

async function saveProject(): Promise<boolean> {
  newProject.error = ''
  newProject.creating = true
  try {
    const payload = buildProjectPayload()
    const targetName = newProject.name.trim()
    const selectedName = projectName.value.trim()
    const selectedExists = selectedName && matchedProjects.value.some(p => p.name === selectedName)
    if (selectedExists && targetName !== selectedName) {
      newProject.error = 'Use Rename before continuing'
      return false
    }
    const isNew = !matchedProjects.value.some(p => p.name === targetName)
    if (isNew) {
      await projectsApi.create(payload)
      matchedProjects.value.push({
        name: targetName,
        work_dir: newProject.workDir,
        job_count: 0,
      })
      snack.success(`Project "${targetName}" registered`)
    } else {
      await projectsApi.update(targetName, payload)
    }
    projectName.value = targetName
    prefs.lastProject.value = projectName.value
    return true
  } catch (e: any) {
    newProject.error = e?.message || 'Failed to save project'
    return false
  } finally {
    newProject.creating = false
  }
}

async function goNext() {
  if (step.value === 0) {
    if (!projectName.value && !newProject.name.trim()) return
    // Save only when something was actually edited (or when creating a new
    // project). Selecting an existing project and clicking Next must be
    // side-effect free — no silent project rewrites.
    if (!projectName.value || newProject.dirty) {
      const ok = await saveProject()
      if (!ok) return
      newProject.dirty = false
    }
  }
  if (step.value === 1) {
    const validation = validateConfigure(rows.value, linkSets.value)
    if (!validation.ok) {
      snack.error(validation.message)
      return
    }
    // Run dry-run before entering review
    prefs.lastProject.value = projectName.value
    dryRunLoading.value = true
    dryRunError.value = ''
    step.value = 2
    try {
      dryRunResult.value = await jobsApi.dryRun(buildJobConfig())
    } catch (e: any) {
      dryRunResult.value = []
      dryRunError.value = e?.message || t('submit.dryrun_failed')
    } finally {
      dryRunLoading.value = false
    }
    return
  }
  step.value++
}

function buildJobConfig() {
  const cfg = createJobConfig(projectName.value, note.value, rows.value, linkSets.value)
  const n = jobName.value.trim()
  if (n) cfg.name = n
  return cfg
}

// ── Re-run as template (?fromJob=<id>) ──
// Loads the source job's raw config: note keeps its {{...}} template form
// (so {{version}} keeps incrementing), sweep blocks decompile into the flat
// row/link-set model. Lands on the configure step, ready to tweak & submit.
onMounted(async () => {
  const fromJob = route.query.fromJob
  if (typeof fromJob !== 'string' || !fromJob) {
    restoreDraft()
    return
  }
  try {
    const detail = await jobsApi.get(fromJob)
    if (!detail.config) {
      snack.warn('Source job has no stored config')
      return
    }
    projectName.value = detail.config.project || detail.job.project
    note.value = detail.config.note || ''
    jobName.value = detail.config.name || ''
    const { rows: r, linkSets: ls } = decompile(detail.config)
    rows.value = r
    linkSets.value = ls
    step.value = 1
  } catch (e: any) {
    snack.error(e?.message || 'Failed to load source job')
  }
})

// ── Import an existing job.yaml from the SERVER's filesystem: same path
// as re-run-from-template — note keeps its {{...}} form, sweep blocks
// decompile into the flat model.
const showJobYamlBrowser = ref(false)

async function onJobYamlSelected(path: string) {
  showJobYamlBrowser.value = false
  const name = path.split(/[\\/]/).pop() || path
  try {
    const YAML = await import('js-yaml')
    const { content } = await filesApi.read(path)
    const cfg = YAML.load(content) as any
    if (!cfg || typeof cfg !== 'object' || (!cfg.sweep && !cfg.fixed_params)) {
      throw new Error('not a job.yaml (no sweep/fixed_params)')
    }
    if (cfg.project) projectName.value = String(cfg.project)
    note.value = cfg.note || ''
    jobName.value = cfg.name || ''
    const { rows: r, linkSets: ls } = decompile(cfg)
    rows.value = r
    linkSets.value = ls
    step.value = 1
    snack.success(`Imported ${name} (${r.length} params)`)
    if (cfg.project && !matchedProjects.value.some(p => p.name === cfg.project)) {
      snack.warn(`Project "${cfg.project}" is not registered yet — register it in step 1 or import its project.yaml`)
    }
  } catch (err: any) {
    snack.error(`Import failed: ${err?.message ?? err}`)
  }
}

const submitError = ref('')

// Stale errors are misinformation: leaving the review step (Back, edits)
// clears the previous submit/preflight error immediately.
watch(step, () => { submitError.value = '' })

// ── Draft insurance: the wizard state survives accidental navigation,
// reloads and crashes. Saved (debounced) to localStorage; restored on
// mount when meaningful; cleared after a successful submit.
let draftTimer: ReturnType<typeof setTimeout> | null = null
watch([projectName, note, jobName, rows, linkSets, step], () => {
  if (draftTimer) clearTimeout(draftTimer)
  draftTimer = setTimeout(() => {
    if (rows.value.length === 0 && !note.value) return
    prefs.submitDraft.value = {
      projectName: projectName.value,
      note: note.value,
      jobName: jobName.value,
      rows: JSON.parse(JSON.stringify(rows.value)),
      linkSets: JSON.parse(JSON.stringify(linkSets.value)),
      step: step.value,
      ts: Date.now(),
    }
  }, 800)
}, { deep: true })

function restoreDraft(): boolean {
  const d = prefs.submitDraft.value
  if (!d || !Array.isArray(d.rows) || d.rows.length === 0) return false
  projectName.value = d.projectName || projectName.value
  note.value = d.note || ''
  jobName.value = d.jobName || ''
  rows.value = d.rows
  linkSets.value = Array.isArray(d.linkSets) ? d.linkSets : []
  step.value = Math.min(d.step ?? 1, 1) // never restore into review (dry-run is stale)
  snack.info('Restored unsubmitted draft — values and links are back')
  return true
}

const isPreflightError = computed(() => submitError.value.includes('preflight') || submitError.value.includes('- import:') || submitError.value.includes('- pip_check:'))

async function submit(forceSkipPreflight = false) {
  submitting.value = true
  submitError.value = ''
  try {
    const res = await jobsApi.submit(buildJobConfig(), {
      preflightEnabled: preflightEnabled.value,
      forceSkipPreflight,
      timeoutMs: 50000,
    })
    snack.success(t('submit.success'))
    prefs.submitDraft.value = null // submitted — the draft has served its purpose
    router.push({ name: 'job-detail', params: { project: projectName.value, jobId: res.job_id } })
  } catch (e: any) {
    submitError.value = e?.message || 'Submit failed'
  } finally {
    submitting.value = false
  }
}
</script>
