<template>
  <div>
    <!-- Header + Step indicators -->
    <div style="max-width: 960px; margin: 0 auto">
      <div class="d-flex align-center justify-space-between mb-4">
        <div>
          <div class="text-h5 font-weight-bold">{{ t('submit.title') }}</div>
          <div class="text-body-2 text-on-surface-variant mt-1">{{ t('submit.subtitle') }}</div>
        </div>
        <v-chip v-if="step >= 2 && displayTaskCount > 0" variant="tonal" color="primary">
          {{ displayTaskCount }} {{ displayTaskCount === 1 ? 'task' : 'tasks' }}
        </v-chip>
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
        @click="submit"
      >
        <v-icon start>mdi-rocket-launch-outline</v-icon>
        {{ t('submit.submit') }} ({{ dryRunResult.length }} tasks)
      </v-btn>
    </div>

    <!-- Persistent submit error -->
    <v-alert
      v-if="submitError && step === 2"
      type="error" variant="tonal" density="compact" closable
      class="mt-3"
      style="max-width: 960px; margin: 0 auto"
      @click:close="submitError = ''"
    >
      {{ submitError }}
    </v-alert>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, provide, reactive, markRaw } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { api } from '@/apis/client'
import { useSnackbar } from '@/composables/useSnackbar'
import { usePreferences } from '@/composables/usePreferences'
import type { ProjectSummary } from '@/types/api'

import StepProject from './StepProject.vue'
import StepConfigure from './StepConfigure.vue'
import StepReview from './StepReview.vue'
import { SUBMIT_STATE_KEY, type SweepGroup } from '@/types/submit'
import {
  buildJobConfig as createJobConfig,
  buildProjectPayload as createProjectPayload,
  dryRunHeaders as createDryRunHeaders,
  groupTaskCount as countGroupTasks,
  sweepSummary as summarizeSweep,
  totalTaskCount as countTotalTasks,
  validateConfigure,
} from './submitFlow'

const stepComponents = [markRaw(StepProject), markRaw(StepConfigure), markRaw(StepReview)]

const { t } = useI18n()
const router = useRouter()
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
  gpus: 1,
  maxRetry: 0,
  envType: '',
  envPath: '',
  envName: '',
  creating: false,
  error: '',
  params: [] as import('@/types/submit').ProjectParam[],
})
const note = ref('')

const groups = ref<SweepGroup[]>([])
let groupIdCounter = 0

const dryRunResult = ref<Record<string, any>[]>([])
const dryRunLoading = ref(false)
const dryRunError = ref('')
const submitting = ref(false)

// ── Computed ──

const usedParamNames = computed(() => {
  const names = new Set<string>()
  for (const g of groups.value) {
    for (const p of g.params) names.add(p.name)
  }
  return names
})

function groupTaskCount(group: SweepGroup): number {
  return countGroupTasks(group)
}

const totalTaskCount = computed(() => countTotalTasks(groups.value))

const displayTaskCount = computed(() => {
  if (step.value === 2 && !dryRunLoading.value) return dryRunResult.value.length
  return totalTaskCount.value
})

const sweepSummary = computed(() => summarizeSweep(groups.value))

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
  groups,
  groupIdCounter,
  usedParamNames,
  totalTaskCount,
  displayTaskCount,
  sweepSummary,
  dryRunResult,
  dryRunLoading,
  dryRunError,
  dryRunHeaders,
  submitting,
  prefs,
  groupTaskCount,
  getNextGroupId: () => ++groupIdCounter,
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
      await api.post('/projects', payload)
      matchedProjects.value.push({
        name: targetName,
        work_dir: newProject.workDir,
        job_count: 0,
      })
      snack.success(`Project "${targetName}" registered`)
    } else {
      await api.put(`/projects/${encodeURIComponent(targetName)}`, payload)
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
    // Always save project (create or update) — writes yaml + DB
    if (!projectName.value && !newProject.name.trim()) return
    const ok = await saveProject()
    if (!ok) return
  }
  if (step.value === 1) {
    const validation = validateConfigure(groups.value, {
      listLengthMismatchMessage: t('submit.list_length_mismatch', 'List group 中各参数的值数量必须相等'),
    })
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
      dryRunResult.value = await api.post<Record<string, any>[]>('/jobs/dry-run', buildJobConfig())
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
  return createJobConfig(projectName.value, note.value, newProject, groups.value)
}

const submitError = ref('')

async function submit() {
  submitting.value = true
  submitError.value = ''
  try {
    const res = await api.post<{ job_id: string }>('/jobs', buildJobConfig())
    snack.success(t('submit.success'))
    router.push({ name: 'job-detail', params: { project: projectName.value, jobId: res.job_id } })
  } catch (e: any) {
    submitError.value = e?.message || 'Submit failed'
  } finally {
    submitting.value = false
  }
}
</script>
