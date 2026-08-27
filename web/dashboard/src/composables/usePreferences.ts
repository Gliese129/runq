import { ref, watch } from 'vue'

const PREFIX = 'runq:'

function load<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(PREFIX + key)
    return raw ? JSON.parse(raw) : fallback
  } catch {
    return fallback
  }
}

function save(key: string, value: unknown) {
  localStorage.setItem(PREFIX + key, JSON.stringify(value))
}

// --- Recent scripts (ordered, max 10) ---

const recentScripts = ref<Array<{ path: string; name: string; project: string; ts: number }>>(
  load('recent-scripts', [])
)

watch(recentScripts, (v) => save('recent-scripts', v), { deep: true })

function addRecentScript(path: string, name: string, project: string) {
  const filtered = recentScripts.value.filter(s => s.path !== path)
  filtered.unshift({ path, name, project, ts: Date.now() })
  recentScripts.value = filtered.slice(0, 10)
}

// --- Last project ---

const lastProject = ref(load('last-project', ''))

watch(lastProject, (v) => save('last-project', v))

// --- Submit draft (crash/back-button insurance) ---

const submitDraft = ref<any>(load('submit-draft', null))

watch(submitDraft, (v) => save('submit-draft', v), { deep: true })

// --- One-time hints (dismissed = never show again) ---

const sugarTipDismissed = ref(load('sugar-tip-dismissed', false))

watch(sugarTipDismissed, (v) => save('sugar-tip-dismissed', v))

// --- Preferred metric key per job ---

const preferredMetrics = ref<Record<string, string>>(load('preferred-metrics', {}))

watch(preferredMetrics, (v) => save('preferred-metrics', v), { deep: true })

function setPreferredMetric(jobId: string, key: string) {
  preferredMetrics.value[jobId] = key
  // Keep map small — only last 50 jobs
  const entries = Object.entries(preferredMetrics.value)
  if (entries.length > 50) {
    preferredMetrics.value = Object.fromEntries(entries.slice(-50))
  }
}

// --- Task status filter preference ---

const lastStatusFilter = ref(load('status-filter', ''))

watch(lastStatusFilter, (v) => save('status-filter', v))

// --- Compare sort preference ---

const compareSortDesc = ref(load('compare-desc', false))

watch(compareSortDesc, (v) => save('compare-desc', v))

// --- Submit: last used args per script ---

const scriptArgs = ref<Record<string, Record<string, string>>>(load('script-args', {}))

watch(scriptArgs, (v) => save('script-args', v), { deep: true })

function saveScriptArgs(scriptPath: string, args: Record<string, string>) {
  scriptArgs.value[scriptPath] = args
  // Keep last 20 scripts
  const entries = Object.entries(scriptArgs.value)
  if (entries.length > 20) {
    scriptArgs.value = Object.fromEntries(entries.slice(-20))
  }
}

function getScriptArgs(scriptPath: string): Record<string, string> {
  return scriptArgs.value[scriptPath] || {}
}

// --- Preferred workspaces (ordered, max 5) ---

const preferredWorkspaces = ref<string[]>(load('workspaces', []))

watch(preferredWorkspaces, (v) => save('workspaces', v))

function addPreferredWorkspace(path: string) {
  if (!path) return
  const filtered = preferredWorkspaces.value.filter(p => p !== path)
  filtered.unshift(path)
  preferredWorkspaces.value = filtered.slice(0, 5)
}

function removePreferredWorkspace(path: string) {
  preferredWorkspaces.value = preferredWorkspaces.value.filter(p => p !== path)
}

// --- File browser: show hidden files (dotfiles) ---

const showHiddenFiles = ref(load('show-hidden-files', false))

watch(showHiddenFiles, (v) => save('show-hidden-files', v))

// --- Job task table: visible columns per job ---

const jobVisibleCols = ref<Record<string, string[]>>(load('job-cols', {}))

watch(jobVisibleCols, (v) => save('job-cols', v), { deep: true })

function setJobVisibleCols(jobId: string, cols: string[]) {
  jobVisibleCols.value[jobId] = cols
  const entries = Object.entries(jobVisibleCols.value)
  if (entries.length > 50) {
    jobVisibleCols.value = Object.fromEntries(entries.slice(-50))
  }
}

function getJobVisibleCols(jobId: string): string[] | null {
  return jobVisibleCols.value[jobId] || null
}

// --- Results table: row label template per PROJECT (the same sweep axes
// recur across a project's jobs, so the template follows the project) ---

const resultLabelTemplates = ref<Record<string, string>>(load('result-labels', {}))

watch(resultLabelTemplates, (v) => save('result-labels', v), { deep: true })

function setResultLabelTemplate(project: string, tpl: string) {
  resultLabelTemplates.value[project] = tpl
  const entries = Object.entries(resultLabelTemplates.value)
  if (entries.length > 50) {
    resultLabelTemplates.value = Object.fromEntries(entries.slice(-50))
  }
}

function getResultLabelTemplate(project: string): string {
  return resultLabelTemplates.value[project] || ''
}

export function usePreferences() {
  return {
    recentScripts,
    addRecentScript,
    lastProject,
    sugarTipDismissed,
    submitDraft,
    preferredMetrics,
    setPreferredMetric,
    lastStatusFilter,
    compareSortDesc,
    saveScriptArgs,
    getScriptArgs,
    preferredWorkspaces,
    addPreferredWorkspace,
    removePreferredWorkspace,
    showHiddenFiles,
    setJobVisibleCols,
    getJobVisibleCols,
    setResultLabelTemplate,
    getResultLabelTemplate,
  }
}
