<template>
  <div style="max-width: 720px; margin: 0 auto">
    <!-- Header -->
    <div class="d-flex align-center justify-space-between mb-6">
      <div>
        <div class="text-h5 font-weight-bold">{{ t('submit.title') }}</div>
        <div class="text-body-2 text-on-surface-variant mt-1">{{ t('submit.subtitle') }}</div>
      </div>
      <v-chip v-if="step >= 1 && displayTaskCount > 0" variant="tonal" color="primary">
        {{ displayTaskCount }} {{ displayTaskCount === 1 ? 'task' : 'tasks' }}
      </v-chip>
    </div>

    <!-- Step indicators -->
    <div class="d-flex ga-2 mb-6">
      <div v-for="(s, i) in steps" :key="i"
        class="flex-grow-1 rounded-pill"
        :style="{
          height: '4px',
          background: step > i ? 'rgb(var(--v-theme-primary))' : 'rgb(var(--v-theme-surface-variant))',
          transition: 'background 0.3s ease',
        }"
      />
    </div>

    <!-- ==================== STEP 1: Select Script ==================== -->
    <div v-if="step === 0">
      <!-- Unified path input: type to navigate, debounced -->
      <v-card class="mb-4 pa-4">
        <v-text-field
          v-model="pastedPath"
          :label="t('submit.paste_path')"
          placeholder="/home/user/experiments/train.py"
          prepend-inner-icon="mdi-link-variant"
          clearable
          hide-details
          @keydown.enter="usePastedPath"
          @update:model-value="onPathInput"
        />
      </v-card>

      <!-- Preferred workspaces -->
      <v-card v-if="prefs.preferredWorkspaces.value.length > 0" class="mb-4 pa-4">
        <div class="text-subtitle-2 mb-3">{{ t('submit.workspaces') }}</div>
        <div class="d-flex flex-wrap ga-2">
          <v-chip
            v-for="ws in prefs.preferredWorkspaces.value"
            :key="ws"
            :variant="currentPath === ws ? 'flat' : 'tonal'"
            :color="currentPath === ws ? 'primary' : undefined"
            class="cursor-pointer"
            closable
            @click="navigateTo(ws)"
            @click:close="prefs.removePreferredWorkspace(ws)"
          >
            <v-icon start size="14">mdi-folder-star-outline</v-icon>
            {{ ws.split('/').filter(Boolean).pop() || ws }}
          </v-chip>
        </div>
      </v-card>

      <!-- Recent scripts -->
      <v-card v-if="prefs.recentScripts.value.length > 0" class="mb-4 pa-4">
        <div class="text-subtitle-2 mb-3">{{ t('submit.recent') }}</div>
        <div class="d-flex flex-column ga-1">
          <div
            v-for="s in prefs.recentScripts.value"
            :key="s.path"
            class="d-flex align-center pa-2 rounded-lg cursor-pointer hover-bg"
            @click="quickSelect(s.path, s.name)"
          >
            <v-icon size="18" color="primary" class="mr-3">mdi-file-code-outline</v-icon>
            <div class="flex-grow-1">
              <div class="text-body-2 font-weight-medium">{{ s.name }}</div>
              <div class="text-caption text-on-surface-variant">{{ s.project }} · {{ timeAgo(s.ts) }}</div>
            </div>
            <v-icon size="16" color="on-surface-variant">mdi-arrow-right</v-icon>
          </div>
        </div>
      </v-card>

      <!-- Start from previous job -->
      <v-card v-if="fromJobId" class="mb-4 pa-4">
        <div class="d-flex align-center ga-2">
          <v-icon color="primary">mdi-content-copy</v-icon>
          <div class="flex-grow-1">
            <div class="text-subtitle-2">{{ t('submit.from_job') }}</div>
            <code class="text-caption text-on-surface-variant">{{ fromJobId.slice(0, 8) }}</code>
          </div>
          <v-btn size="small" variant="tonal" color="primary" @click="loadFromJob">
            {{ t('common.use') }}
          </v-btn>
        </div>
      </v-card>

      <!-- Browse directory -->
      <v-card class="pa-4">
        <div class="d-flex align-center justify-space-between mb-3">
          <div class="d-flex align-center ga-2">
            <div class="text-subtitle-2">{{ t('submit.browse') }}</div>
            <v-tooltip :text="t('submit.pin_workspace')" location="top">
              <template #activator="{ props: tp }">
                <v-btn
                  v-bind="tp"
                  icon
                  size="x-small"
                  variant="text"
                  :color="isWorkspacePinned ? 'primary' : undefined"
                  @click="togglePinWorkspace"
                >
                  <v-icon size="16">{{ isWorkspacePinned ? 'mdi-star' : 'mdi-star-outline' }}</v-icon>
                </v-btn>
              </template>
            </v-tooltip>
          </div>
          <div class="d-flex align-center ga-2">
            <v-tooltip :text="t('submit.show_hidden')" location="top">
              <template #activator="{ props: tp }">
                <v-btn
                  v-bind="tp"
                  icon
                  size="x-small"
                  :variant="showHidden ? 'tonal' : 'text'"
                  :color="showHidden ? 'primary' : undefined"
                  @click="showHidden = !showHidden"
                >
                  <v-icon size="14">mdi-eye{{ showHidden ? '' : '-off' }}-outline</v-icon>
                </v-btn>
              </template>
            </v-tooltip>
            <v-btn-toggle v-model="fileFilter" mandatory density="compact" variant="outlined">
              <v-btn value=".py" size="x-small">.py</v-btn>
              <v-btn value=".yaml" size="x-small">.yaml</v-btn>
              <v-btn value="" size="x-small">{{ t('submit.all_files') }}</v-btn>
            </v-btn-toggle>
          </div>
        </div>

        <!-- Current path — clickable segments for quick navigation -->
        <div class="d-flex align-center flex-wrap ga-1 mb-2 text-caption text-on-surface-variant">
          <span class="cursor-pointer" style="text-decoration: underline dotted" @click="loadDir('', true)">~</span>
          <template v-for="(seg, i) in pathSegments" :key="i">
            <v-icon size="10">mdi-chevron-right</v-icon>
            <span
              class="cursor-pointer"
              :style="{ textDecoration: i < pathSegments.length - 1 ? 'underline dotted' : 'none', fontWeight: i === pathSegments.length - 1 ? 500 : 400 }"
              @click="navigateToSegment(i)"
            >{{ seg }}</span>
          </template>
        </div>

        <v-list density="compact" class="rounded-lg" style="max-height: 320px; overflow-y: auto">
          <v-list-item
            v-if="currentPath !== ''"
            @click="navigateUp"
            class="rounded-lg"
          >
            <template #prepend>
              <v-icon size="18" color="on-surface-variant">mdi-arrow-up</v-icon>
            </template>
            <v-list-item-title class="text-body-2">..</v-list-item-title>
          </v-list-item>

          <v-list-item
            v-for="entry in filteredEntries"
            :key="entry.path"
            @click="entry.is_dir ? navigateTo(entry.path) : selectAndProceed(entry)"
            class="rounded-lg"
            :active="selectedScript?.path === entry.path"
          >
            <template #prepend>
              <v-icon size="18" :color="entry.is_dir ? 'warning' : 'primary'">
                {{ entry.is_dir ? 'mdi-folder' : 'mdi-file-code-outline' }}
              </v-icon>
            </template>
            <v-list-item-title class="text-body-2">{{ entry.name }}</v-list-item-title>
            <template #append>
              <span v-if="!entry.is_dir" class="text-caption text-on-surface-variant">
                {{ formatSize(entry.size) }}
              </span>
            </template>
          </v-list-item>
        </v-list>

        <div v-if="filteredEntries.length === 0 && !loadingDir" class="text-center text-on-surface-variant pa-6">
          <v-icon size="32" class="mb-2" color="on-surface-variant">mdi-file-search-outline</v-icon>
          <div class="text-body-2">{{ t('submit.no_files') }}</div>
        </div>
      </v-card>
    </div>

    <!-- ==================== STEP 2: Configure ==================== -->
    <div v-else-if="step === 1">
      <!-- Script info card -->
      <v-card class="mb-4 pa-4">
        <div class="d-flex align-center ga-2">
          <v-icon color="primary">mdi-file-code-outline</v-icon>
          <code class="text-body-2 flex-grow-1">{{ selectedScript?.name }}</code>
          <v-chip v-if="parseResult?.detected_env" size="small" variant="tonal" color="secondary">
            {{ parseResult.detected_env }}
          </v-chip>
        </div>
      </v-card>

      <!-- Project & Note -->
      <v-card class="mb-4 pa-4">
        <v-text-field
          v-model="projectName"
          :label="t('submit.project')"
          prepend-inner-icon="mdi-folder-outline"
          class="mb-3"
        />
        <v-text-field
          v-model="note"
          :label="t('submit.note')"
          :placeholder="t('submit.note_placeholder')"
          prepend-inner-icon="mdi-text-short"
          hide-details
        />
      </v-card>

      <!-- Parameters -->
      <v-card class="pa-4">
        <div class="d-flex align-center justify-space-between mb-3">
          <div class="text-subtitle-2">
            {{ t('submit.params') }}
            <v-chip size="x-small" variant="tonal" class="ml-1">{{ visibleArgs.length }}</v-chip>
          </div>
          <v-text-field
            v-if="args.length > 5"
            v-model="paramSearch"
            :placeholder="t('submit.filter_params')"
            prepend-inner-icon="mdi-magnify"
            density="compact"
            variant="plain"
            hide-details
            single-line
            style="max-width: 200px"
          />
        </div>

        <div class="d-flex flex-column ga-3">
          <div v-for="arg in visibleArgs" :key="arg.name" class="rounded-lg pa-3" style="background: rgb(var(--v-theme-surface-variant), 0.3)">
            <div class="d-flex align-center justify-space-between mb-2">
              <div class="d-flex align-center ga-2">
                <span class="text-body-2 font-weight-medium">{{ arg.name }}</span>
                <v-chip size="x-small" variant="text" color="on-surface-variant">{{ arg.type }}</v-chip>
              </div>
              <v-switch
                v-model="arg.sweep"
                :label="t('submit.sweep')"
                hide-details
                density="compact"
              />
            </div>

            <!-- Boolean: switch -->
            <v-switch
              v-if="arg.type === 'bool'"
              v-model="arg.boolValue"
              :label="arg.boolValue ? 'true' : 'false'"
              hide-details
              color="primary"
            />

            <!-- Sweep mode: chip input -->
            <ChipInput
              v-else-if="arg.sweep"
              v-model="arg.sweepValues"
              :label="t('submit.sweep_values')"
              :placeholder="t('submit.sweep_placeholder')"
              color="secondary"
            />

            <!-- Number -->
            <v-text-field
              v-else-if="arg.type === 'int' || arg.type === 'float'"
              v-model="arg.value"
              type="number"
              :placeholder="arg.default || ''"
              hide-details
            />

            <!-- Default: text -->
            <v-text-field
              v-else
              v-model="arg.value"
              :placeholder="arg.default || ''"
              hide-details
            />
          </div>
        </div>

        <div v-if="visibleArgs.length === 0" class="text-center text-on-surface-variant pa-4">
          {{ paramSearch ? t('submit.no_match') : t('submit.no_params') }}
        </div>
      </v-card>
    </div>

    <!-- ==================== STEP 3: Review & Submit ==================== -->
    <div v-else-if="step === 2">
      <!-- Summary -->
      <v-card class="mb-4 pa-4">
        <div class="d-flex flex-column ga-2">
          <div class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">{{ t('submit.project') }}</span>
            <span class="font-weight-medium">{{ projectName }}</span>
          </div>
          <div class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">{{ t('submit.script_label') }}</span>
            <code class="text-body-2">{{ selectedScript?.name }}</code>
          </div>
          <div class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">{{ t('submit.total_tasks') }}</span>
            <v-chip variant="tonal" color="primary" size="small">{{ dryRunResult.length }}</v-chip>
          </div>
          <div v-if="sweepSummary" class="d-flex align-center justify-space-between">
            <span class="text-on-surface-variant">{{ t('submit.sweep_label') }}</span>
            <span class="text-body-2">{{ sweepSummary }}</span>
          </div>
        </div>
      </v-card>

      <!-- Dry-run preview -->
      <v-card class="pa-4">
        <div class="text-subtitle-2 mb-3">{{ t('submit.preview') }}</div>

        <div v-if="dryRunLoading" class="d-flex justify-center pa-6">
          <v-progress-circular indeterminate color="primary" />
        </div>

        <v-data-table
          v-else-if="dryRunResult.length > 0"
          :headers="dryRunHeaders"
          :items="dryRunResult"
          density="compact"
          :items-per-page="10"
        >
          <template #bottom v-if="dryRunResult.length <= 10" />
        </v-data-table>

        <div v-else class="text-center text-on-surface-variant pa-6">
          <v-icon size="32" class="mb-2">mdi-alert-circle-outline</v-icon>
          <div class="text-body-2">{{ dryRunError || t('submit.no_tasks') }}</div>
          <div v-if="!dryRunError" class="text-caption mt-1">{{ t('submit.no_tasks_hint') }}</div>
        </div>
      </v-card>
    </div>

    <!-- ==================== Navigation ==================== -->
    <div class="d-flex align-center mt-6 ga-2">
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
import { ref, computed, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { api } from '@/api/client'
import { useSnackbar } from '@/composables/useSnackbar'
import { usePreferences } from '@/composables/usePreferences'
import ChipInput from '@/components/ChipInput.vue'
import type { FSEntry, ParseResult, JobDetail } from '@/api/types'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const snack = useSnackbar()
const prefs = usePreferences()

interface ArgState {
  name: string
  type: string
  default?: string
  value: string
  sweep: boolean
  sweepValues: string[]
  boolValue: boolean
}

const steps = ['select', 'configure', 'review']
const step = ref(0)

// Step 1 state
const pastedPath = ref('')
const currentPath = ref('')
const entries = ref<FSEntry[]>([])
const loadingDir = ref(false)
const selectedScript = ref<FSEntry | null>(null)
const fileFilter = ref('.py')
const showHidden = ref(false)
const fromJobId = ref(route.query.from as string || '')

// Step 2 state
const parseResult = ref<ParseResult | null>(null)
const projectName = ref(prefs.lastProject.value || '')
const note = ref('')
const args = ref<ArgState[]>([])
const paramSearch = ref('')

// Step 3 state
const dryRunResult = ref<Record<string, any>[]>([])
const dryRunLoading = ref(false)
const dryRunError = ref('')
const submitting = ref(false)

const filteredEntries = computed(() => {
  let list = entries.value
  // Hide dotfiles unless toggled
  if (!showHidden.value) {
    list = list.filter(e => !e.name.startsWith('.'))
  }
  // File extension filter
  if (fileFilter.value) {
    list = list.filter(e => e.is_dir || e.name.endsWith(fileFilter.value))
  }
  return list
})

const visibleArgs = computed(() => {
  if (!paramSearch.value) return args.value
  const q = paramSearch.value.toLowerCase()
  return args.value.filter(a => a.name.toLowerCase().includes(q))
})

const estimatedTasks = computed(() => {
  let combos = 1
  for (const a of args.value) {
    if (a.sweep && a.sweepValues.length > 0) {
      combos *= a.sweepValues.length
    }
  }
  return combos
})

// On Review step, use dry-run result (authoritative). Otherwise, use estimate.
const displayTaskCount = computed(() => {
  if (step.value === 2 && !dryRunLoading.value) return dryRunResult.value.length
  return estimatedTasks.value
})

const sweepSummary = computed(() => {
  const sweeps = args.value.filter(a => a.sweep && a.sweepValues.length > 0)
  if (sweeps.length === 0) return ''
  return sweeps.map(a => `${a.name}(${a.sweepValues.length})`).join(' × ')
})

const dryRunHeaders = computed(() => {
  if (dryRunResult.value.length === 0) return []
  return Object.keys(dryRunResult.value[0]).map(k => ({ title: k, key: k }))
})

// --- Path input debounce ---

const SCRIPT_EXTS = ['.py', '.yaml', '.yml', '.sh', '.r', '.jl']
let debounceTimer: ReturnType<typeof setTimeout> | null = null

function onPathInput(value: string | null) {
  if (debounceTimer) clearTimeout(debounceTimer)
  if (!value) return
  debounceTimer = setTimeout(() => {
    const trimmed = value.trim()

    // Looks like a complete script path → auto-select it
    if (SCRIPT_EXTS.some(ext => trimmed.toLowerCase().endsWith(ext))) {
      usePastedPath()
      return
    }

    // If path ends with /, navigate to that directory
    if (trimmed.endsWith('/')) {
      loadDir(trimmed.replace(/\/+$/, ''), false)
      return
    }

    // Otherwise navigate to parent directory for prefix-match browsing
    const lastSlash = trimmed.lastIndexOf('/')
    if (lastSlash > 0) {
      const dir = trimmed.substring(0, lastSlash)
      if (dir !== currentPath.value) {
        loadDir(dir, false)
      }
    }
  }, 300)
}

// --- Preferred workspace helpers ---

const isWorkspacePinned = computed(() =>
  currentPath.value !== '' && prefs.preferredWorkspaces.value.includes(currentPath.value)
)

function togglePinWorkspace() {
  if (!currentPath.value) return
  if (isWorkspacePinned.value) {
    prefs.removePreferredWorkspace(currentPath.value)
  } else {
    prefs.addPreferredWorkspace(currentPath.value)
  }
}

// --- Directory navigation ---

async function loadDir(path: string, syncInput = true) {
  loadingDir.value = true
  try {
    currentPath.value = path
    // Only sync input when navigation came from clicking (not from typing)
    if (syncInput) {
      pastedPath.value = path ? path + '/' : ''
    }
    entries.value = await api.get<FSEntry[]>(`/fs/list?path=${encodeURIComponent(path)}`)
  } catch {
    // toast already handled by api client
  } finally {
    loadingDir.value = false
  }
}

function navigateTo(path: string) { loadDir(path, true) }

const pathSegments = computed(() => {
  if (!currentPath.value) return []
  return currentPath.value.split('/').filter(Boolean)
})

function navigateToSegment(index: number) {
  const segs = pathSegments.value.slice(0, index + 1)
  loadDir('/' + segs.join('/'), true)
}

function navigateUp() {
  const parts = currentPath.value.split('/').filter(Boolean)
  parts.pop()
  loadDir(parts.length > 0 ? '/' + parts.join('/') : '')
}

// --- Script selection ---

async function selectAndProceed(entry: FSEntry) {
  selectedScript.value = entry
  await goNext()
}

async function quickSelect(path: string, name: string) {
  selectedScript.value = { path, name, is_dir: false, size: 0 }
  await parseAndConfigure()
  step.value = 1
}

async function usePastedPath() {
  if (!pastedPath.value.trim()) return
  const path = pastedPath.value.trim()
  const name = path.split('/').pop() || path
  selectedScript.value = { path, name, is_dir: false, size: 0 }
  await parseAndConfigure()
  step.value = 1
}

async function loadFromJob() {
  if (!fromJobId.value) return
  try {
    const detail = await api.get<JobDetail>(`/jobs/${fromJobId.value}`)
    // Use first task's params as defaults
    if (detail.tasks.length > 0) {
      projectName.value = detail.job.project
      note.value = `from ${detail.job.id.slice(0, 8)}`
      // We need the script path from the job config — skip to step 2 if we have params
      snack.info(t('submit.loaded_from_job'))
    }
  } catch {
    snack.error(t('submit.load_job_failed'))
  }
}

// --- Parse & configure ---

async function parseAndConfigure() {
  if (!selectedScript.value) return
  parseResult.value = await api.post<ParseResult>('/fs/parse-script', { path: selectedScript.value.path })

  // Restore saved args for this script
  const saved = prefs.getScriptArgs(selectedScript.value.path)

  args.value = (parseResult.value.args || []).map(a => ({
    ...a,
    value: saved[a.name] || a.default || '',
    sweep: false,
    sweepValues: [],
    boolValue: (saved[a.name] || a.default || '').toLowerCase() === 'true',
  }))

  // Auto-fill project
  if (!projectName.value) {
    const parts = selectedScript.value.path.split('/')
    projectName.value = parts.length > 1 ? parts[parts.length - 2] : 'default'
  }

  // Remember script in recents
  prefs.addRecentScript(
    selectedScript.value.path,
    selectedScript.value.name,
    projectName.value,
  )
}

// --- Navigation ---

async function goNext() {
  if (step.value === 0) {
    await parseAndConfigure()
  }
  if (step.value === 1) {
    // Save args for next time
    if (selectedScript.value) {
      const argMap: Record<string, string> = {}
      for (const a of args.value) {
        argMap[a.name] = a.type === 'bool' ? String(a.boolValue) : a.value
      }
      prefs.saveScriptArgs(selectedScript.value.path, argMap)
    }
    // Remember project
    prefs.lastProject.value = projectName.value
    // Auto dry-run
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

// --- Build & submit ---

function buildJobConfig() {
  const params: Record<string, any> = {}
  for (const a of args.value) {
    if (a.type === 'bool') {
      params[a.name] = a.boolValue
    } else if (a.sweep && a.sweepValues.length > 0) {
      params[a.name] = { sweep: a.sweepValues }
    } else {
      params[a.name] = a.value
    }
  }
  return {
    project: projectName.value,
    script: selectedScript.value?.path || '',
    command: parseResult.value?.suggested_command || '',
    note: note.value,
    params,
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

// --- Helpers ---

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1048576) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1048576).toFixed(1)} MB`
}

function timeAgo(ts: number): string {
  const diff = (Date.now() - ts) / 1000
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// --- Init ---

onMounted(() => {
  // Default to first preferred workspace, or home
  const defaultDir = prefs.preferredWorkspaces.value[0] || ''
  loadDir(defaultDir)
  if (fromJobId.value) loadFromJob()
})
</script>

<style scoped>
.hover-bg:hover {
  background: rgb(var(--v-theme-surface-variant));
  transition: background 0.15s ease;
}
</style>
