<template>
  <div>
    <!-- Header + Step indicators -->
    <div style="max-width: 960px; margin: 0 auto">
      <div class="d-flex align-center justify-space-between mb-4">
        <div>
          <div class="text-h5 font-weight-bold">{{ t('submit.title') }}</div>
          <div class="text-body-2 text-on-surface-variant mt-1">{{ t('submit.subtitle') }}</div>
        </div>
        <v-chip v-if="step >= 1 && displayTaskCount > 0" variant="tonal" color="primary">
          {{ displayTaskCount }} {{ displayTaskCount === 1 ? 'task' : 'tasks' }}
        </v-chip>
      </div>

      <div class="d-flex ga-2 mb-5">
        <div v-for="(s, i) in steps" :key="i"
          class="flex-grow-1 rounded-pill"
          :style="{
            height: '4px',
            background: step > i ? 'rgb(var(--v-theme-primary))' : 'rgb(var(--v-theme-surface-variant))',
            transition: 'background 0.3s ease',
          }"
        />
      </div>
    </div>

    <StepSelect v-if="step === 0" @select="onScriptSelected" @load-from-job="loadFromJob" />
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
        v-if="step < 2"
        color="primary"
        variant="tonal"
        :disabled="step === 0 && !selectedScript"
        @click="goNext"
      >
        {{ t('common.next') }}
        <v-icon end>mdi-arrow-right</v-icon>
      </v-btn>
      <v-btn
        v-else
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
import { ref, computed, onMounted, provide, reactive } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { api } from '@/api/client'
import { useSnackbar } from '@/composables/useSnackbar'
import { usePreferences } from '@/composables/usePreferences'
import type { FSEntry, ParseResult, JobDetail } from '@/api/types'

import StepSelect from './StepSelect.vue'
import StepConfigure from './StepConfigure.vue'
import StepReview from './StepReview.vue'
import { SUBMIT_STATE_KEY, type ArgState, type SweepGroup } from './types'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const snack = useSnackbar()
const prefs = usePreferences()

const steps = ['select', 'configure', 'review']
const step = ref(0)

const selectedScript = ref<FSEntry | null>(null)
const fromJobId = ref(route.query.from as string || '')

const parseResult = ref<ParseResult | null>(null)
const projectName = ref(prefs.lastProject.value || '')
const note = ref('')
const args = ref<ArgState[]>([])

const groups = ref<SweepGroup[]>([])
let groupIdCounter = 0

const dryRunResult = ref<Record<string, any>[]>([])
const dryRunLoading = ref(false)
const dryRunError = ref('')
const submitting = ref(false)

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

// Backend does cartesian product across sweep blocks
const totalTaskCount = computed(() => {
  const activeGroups = groups.value.filter(g => g.params.length > 0)
  if (activeGroups.length === 0) return 1
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

provide(SUBMIT_STATE_KEY, reactive({
  step,
  selectedScript,
  parseResult,
  projectName,
  note,
  args,
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
  fromJobId,
  prefs,
  groupTaskCount,
  getNextGroupId: () => ++groupIdCounter,
}))

async function onScriptSelected(entry: FSEntry) {
  selectedScript.value = entry
  await goNext()
}

async function parseAndConfigure() {
  if (!selectedScript.value) return
  parseResult.value = await api.post<ParseResult>('/fs/parse-script', { path: selectedScript.value.path })
  const saved = prefs.getScriptArgs(selectedScript.value.path)

  groups.value = []
  groupIdCounter = 0

  args.value = (parseResult.value.args || []).map(a => ({
    ...a,
    value: saved[a.name] || a.default || '',
    sweep: false,
    sweepValues: [],
    boolValue: (saved[a.name] || a.default || '').toLowerCase() === 'true',
  }))

  if (!projectName.value) {
    const parts = selectedScript.value.path.split(/[\\/]+/)
    projectName.value = parts.length > 1 ? parts[parts.length - 2] : 'default'
  }

  prefs.addRecentScript(
    selectedScript.value.path,
    selectedScript.value.name,
    projectName.value,
  )
}

async function goNext() {
  if (step.value === 0) {
    await parseAndConfigure()
  }
  if (step.value === 1) {
    if (selectedScript.value) {
      const argMap: Record<string, string> = {}
      for (const a of args.value) {
        argMap[a.name] = a.type === 'bool' ? String(a.boolValue) : a.value
      }
      prefs.saveScriptArgs(selectedScript.value.path, argMap)
    }
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

  const baseParams: Record<string, { values: any[] }> = {}
  for (const a of args.value) {
    if (usedParamNames.value.has(a.name)) continue
    const val = a.type === 'bool' ? a.boolValue : a.value
    if (val !== '' && val !== undefined) {
      baseParams[a.name] = { values: [val] }
    }
  }
  if (Object.keys(baseParams).length > 0) {
    sweep.push({ method: 'list', parameters: baseParams })
  }

  for (const g of groups.value) {
    const active = g.params.filter(p => p.values.length > 0)
    if (active.length === 0) continue
    const parameters: Record<string, { values: any[] }> = {}
    for (const p of active) {
      const arg = args.value.find(a => a.name === p.name)
      const tp = arg?.type?.toLowerCase() || ''
      parameters[p.name] = {
        values: p.values.map(v => {
          if (tp === 'int') return parseInt(v, 10) || v
          if (tp === 'float') return parseFloat(v) || v
          if (tp === 'bool') return v === 'true'
          return v
        }),
      }
    }
    sweep.push({ method: g.type, parameters })
  }

  return {
    project: projectName.value,
    description: selectedScript.value?.path || '',
    note: note.value,
    sweep,
  }
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

async function loadFromJob() {
  if (!fromJobId.value) return
  try {
    const detail = await api.get<JobDetail>(`/jobs/${fromJobId.value}`)
    if (detail.tasks.length > 0) {
      projectName.value = detail.job.project
      note.value = `from ${detail.job.id.slice(0, 8)}`
      snack.info(t('submit.loaded_from_job'))
    }
  } catch {
    snack.error(t('submit.load_job_failed'))
  }
}

onMounted(() => {
  if (fromJobId.value) loadFromJob()
})
</script>
