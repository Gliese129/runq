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
  if (group.params.length === 0) return 0
  const counts = group.params.map(p => activeValues(p).length)
  if (counts.every(c => c === 0)) return 0
  if (group.type === 'grid') {
    // Grid: cartesian product — only params with values contribute
    return counts.filter(c => c > 0).reduce((acc, c) => acc * c, 1)
  }
  // List: all params must have same count (backend enforces), show min for safety
  const nonZero = counts.filter(c => c > 0)
  return nonZero.length > 0 ? Math.min(...nonZero) : 0
}

const totalTaskCount = computed(() => {
  const activeGroups = groups.value.filter(g => g.params.length > 0)
  if (activeGroups.length === 0) return 1
  const counts = activeGroups.map(g => groupTaskCount(g))
  if (counts.some(c => c === 0)) return 0 // has params but no values = invalid, not 1
  return counts.reduce((product, c) => product * c, 1)
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
      const parts = g.params.map(p => `${p.name}(${activeValues(p).length})`)
      return `[${label}] ${parts.join(g.type === 'grid' ? ' x ' : ', ')}`
    })
    .join(' + ')
})

const dryRunHeaders = computed(() => {
  if (dryRunResult.value.length === 0) return []
  const keys = new Set<string>()
  for (const row of dryRunResult.value) {
    for (const key of Object.keys(row)) keys.add(key)
  }
  const ordered: string[] = []
  for (const p of newProject.params) {
    if (keys.delete(p.name)) ordered.push(p.name)
  }
  ordered.push(...Array.from(keys).sort())
  return ordered.map(k => ({ title: k, key: k }))
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
  // Persist all param metadata. `include` is submit-flow state, not project YAML state.
  const params = newProject.params.filter(p => p.name.trim())
  if (params.length > 0) {
    payload.params = params.map(p => {
      const choices = p.values?.filter(v => !isBlankValue(v))
      return {
        name: p.name.trim(),
        type: p.type,
        default: !isBlankValue(p.default) ? p.default : undefined,
        choices: choices?.length ? choices : undefined,
        min: p.min ?? undefined,
        max: p.max ?? undefined,
      }
    })
  }
  return payload
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
    // ── Validation ──
    const activeGroups = groups.value.filter(g => g.params.length > 0)
    // Check for params with no values
    for (const g of activeGroups) {
      const emptyParams = g.params.filter(p => activeValues(p).length === 0)
      if (emptyParams.length > 0) {
        snack.error(`"${emptyParams[0].name}" has no values — add values or remove it`)
        return
      }
      for (const p of g.params) {
        const invalid = activeValues(p).find(v => validateTypedValue(v, p.type))
        if (invalid != null) {
          snack.error(`"${p.name}" has invalid ${p.type} value: ${invalid}`)
          return
        }
      }
    }
    // List groups: equal non-empty value counts
    for (const g of groups.value) {
      if (g.type !== 'list' || g.params.length === 0) continue
      const lengths = g.params.map(p => activeValues(p).length).filter(l => l > 0)
      if (lengths.length > 0 && new Set(lengths).size > 1) {
        snack.error(t('submit.list_length_mismatch', 'List group 中各参数的值数量必须相等'))
        return
      }
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
  const sweep: { method: string; parameters: Record<string, { values: any[] }> }[] = []
  const sweptNames = new Set<string>()

  for (const g of groups.value) {
    const active = g.params.filter(p => activeValues(p).length > 0)
    if (active.length === 0) continue
    const parameters: Record<string, { values: any[] }> = {}
    for (const p of active) {
      const cleaned = activeValues(p)
      parameters[p.name] = { values: cleaned.map(v => coerceValue(v, p.type)) }
      sweptNames.add(p.name)
    }
    sweep.push({ method: g.type, parameters })
  }

  // Collect fixed_params: included params with default that aren't in sweep
  const fixedParams: Record<string, any> = {}
  for (const p of newProject.params) {
    if (!p.include || isBlankValue(p.default) || sweptNames.has(p.name)) continue
    fixedParams[p.name] = coerceValue(p.default, p.type)
  }

  return {
    project: projectName.value,
    note: note.value,
    fixed_params: Object.keys(fixedParams).length > 0 ? fixedParams : undefined,
    sweep,
  }
}

function coerceValue(v: string, type: string): any {
  const trimmed = v.trim()
  switch (type) {
    case 'int': { const n = parseInt(trimmed, 10); return isNaN(n) ? v : n }
    case 'float': { const n = parseFloat(trimmed); return isNaN(n) ? v : n }
    case 'bool': return trimmed.toLowerCase() === 'true' || trimmed === '1'
    case 'str': case 'file': case 'folder': case 'list': return v
    default: return v
  }
}

function isBlankValue(v: string): boolean {
  return String(v ?? '').trim() === ''
}

function activeValues(p: { values: string[] }): string[] {
  return p.values.filter(v => !isBlankValue(v))
}

function validateTypedValue(value: string, type: string): string {
  const trimmed = value.trim()
  switch (type) {
    case 'int':
      return /^-?\d+$/.test(trimmed) ? '' : value
    case 'float':
      return trimmed !== '' && Number.isFinite(Number(trimmed)) ? '' : value
    case 'bool':
      return ['true', 'false', '1', '0'].includes(trimmed.toLowerCase()) ? '' : value
    default:
      return ''
  }
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
