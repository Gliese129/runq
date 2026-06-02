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

const compareSortDesc = ref(load('compare-desc', true))

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

export function usePreferences() {
  return {
    recentScripts,
    addRecentScript,
    lastProject,
    preferredMetrics,
    setPreferredMetric,
    lastStatusFilter,
    compareSortDesc,
    saveScriptArgs,
    getScriptArgs,
    preferredWorkspaces,
    addPreferredWorkspace,
    removePreferredWorkspace,
  }
}
