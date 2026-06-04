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
            background: step > i ? 'rgb(var(--v-theme-primary))' : 'rgb(var(--v-theme-surface-variant))',
            transition: 'background 0.3s ease',
          }"
        />
      </div>
    </div>

    <StepProject v-if="step === 0" @create="onCreateProject" />
    <StepConfigure v-else-if="step === 1" />
    <StepReview v-else-if="step === 2" />

    <!-- Navigation -->
    <div class="d-flex align-center mt-5 ga-2" style="max-width: 960px; margin: 0 auto">
      <v-btn v-if="step > 0" variant="text" @click="step--">
        <v-icon start>mdi-arrow-left</v-icon>
        {{ t('common.back') }}
      </v-btn>
      <v-spacer />
      <v-btn
        v-if="step === 0"
        color="primary"
        variant="tonal"
        :disabled="!projectName"
        @click="goNext"
      >
        {{ t('common.next') }}
        <v-icon end>mdi-arrow-right</v-icon>
      </v-btn>
      <v-btn
        v-else-if="step === 1"
        color="primary"
        variant="tonal"
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
  </div>
</template>

<script setup lang="ts">
import { ref, computed, provide, reactive } from 'vue'
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
  params: [] as Array<{ name: string; type: string; default: string; include: boolean }>,
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
  if (group.params.length === 0) return 0
  if (group.type === 'grid') {
    return group.params.reduce((acc, p) => acc * Math.max(p.values.length, 1), 1)
  }
  return Math.max(...group.params.map(p => p.values.length), 0)
}

const totalTaskCount = computed(() => {
  if (groups.value.length === 0) return 1 // no sweep = 1 task
  const activeGroups = groups.value.filter(g => g.params.length > 0)
  if (activeGroups.length === 0) return 0 // explicit empty groups = 0 tasks
  return activeGroups.reduce((product, g) => product * (groupTaskCount(g) || 1), 1)
})

const displayTaskCount = computed(() => {
  if (step.value === 2 && !dryRunLoading.value) return dryRunResult.value.length
  return totalTaskCount.value
})

const sweepSummary = computed(() => {
  if (groups.value.length === 0) return ''
  return groups.value
    .filter(g => g.params.length > 0)
    .map(g => {
      const label = g.type === 'grid' ? 'Grid' : 'List'
      const parts = g.params.map(p => `${p.name}(${p.values.length})`)
      return `[${label}] ${parts.join(g.type === 'grid' ? ' x ' : ', ')}`
    })
    .join(' + ')
})

const dryRunHeaders = computed(() => {
  if (dryRunResult.value.length === 0) return []
  return Object.keys(dryRunResult.value[0]).map(k => ({ title: k, key: k }))
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

async function onCreateProject() {
  newProject.error = ''
  newProject.creating = true
  try {
    const payload: Record<string, any> = {
      project_name: newProject.name.trim(),
      working_dir: newProject.workDir,
      command_template: newProject.cmd,
      defaults: { gpus_per_task: newProject.gpus, max_retry: newProject.maxRetry },
    }
    if (newProject.envType) {
      payload.python_env = {
        type: newProject.envType,
        path: newProject.envPath || undefined,
        name: newProject.envName || undefined,
      }
    }
    await api.post('/projects', payload)
    matchedProjects.value.push({
      name: newProject.name.trim(),
      work_dir: newProject.workDir,
      job_count: 0,
    })
    projectName.value = newProject.name.trim()
    snack.success(`Project "${projectName.value}" registered`)
    seedGroupsFromParams()
    step.value++ // advance to configure
  } catch (e: any) {
    newProject.error = e?.message || 'Failed to create project'
  } finally {
    newProject.creating = false
  }
}

function seedGroupsFromParams() {
  const included = newProject.params.filter(p => p.include)
  if (included.length === 0 || groups.value.length > 0) return
  const id = ++groupIdCounter
  const params = included.map(p => ({
    name: p.name,
    type: p.type,
    default: p.default,
    values: p.default ? [p.default] : [],
  }))
  groups.value.push({ id: `g${id}`, type: 'grid', expanded: true, params })
}

async function goNext() {
  if (step.value === 0) {
    prefs.lastProject.value = projectName.value
    // Seed configure from discovered params (for existing projects, groups stay as-is)
    if (groups.value.length === 0 && newProject.params.length > 0) {
      seedGroupsFromParams()
    }
  }
  if (step.value === 1) {
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
  const sweep: { method: string; parameters: Record<string, { values: any[] }> }[] = []

  for (const g of groups.value) {
    const active = g.params.filter(p => p.values.length > 0)
    if (active.length === 0) continue
    const parameters: Record<string, { values: any[] }> = {}
    for (const p of active) {
      parameters[p.name] = { values: p.values.map(inferType) }
    }
    sweep.push({ method: g.type, parameters })
  }

  return {
    project: projectName.value,
    note: note.value,
    sweep,
  }
}

function inferType(v: string): any {
  if (v === 'true') return true
  if (v === 'false') return false
  const n = Number(v)
  if (!isNaN(n) && v.trim() !== '') return n
  return v
}

async function submit() {
  submitting.value = true
  try {
    const res = await api.post<{ job_id: string }>('/jobs', buildJobConfig())
    snack.success(t('submit.success'))
    router.push({ name: 'job-detail', params: { project: projectName.value, jobId: res.job_id } })
  } catch {
    // toast handled by api client
  } finally {
    submitting.value = false
  }
}
</script>
