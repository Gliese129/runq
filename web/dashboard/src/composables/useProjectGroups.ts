// useProjectGroups (RQ2-4 ④) — Vue state + persistence for the sidebar
// project groups. Same storage contract as the appearance levers and
// param groups (RQ2-1 #12): ui.json is the roaming truth (opaque blob to
// the backend, `projectGroups` key is ours), localStorage the offline
// cache. Seeding stays in memory until the first user mutation — a
// suggestion must never write itself.
import { ref } from 'vue'
import { uiApi } from '@/apis/ui'
import {
  type ProjectGroupsState, emptyState, seedGroups, sanitizeGroups,
  assign as assignFn, renameGroup as renameFn, removeGroup as removeFn,
  createGroup as createFn, toggleCollapsed as toggleFn,
} from './projectGroups'

const STORAGE_KEY = 'runq-project-groups'

const state = ref<ProjectGroupsState>(readLocal() ?? emptyState())
const hasStored = ref(readLocal() !== null)
let loaded = false

function readLocal(): ProjectGroupsState | null {
  try {
    return sanitizeGroups(JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null'))
  } catch {
    return null
  }
}

/** Load the roaming copy once per session; the local cache already applied. */
async function load() {
  if (loaded) return
  loaded = true
  try {
    const doc = await uiApi.get()
    const remote = sanitizeGroups((doc as Record<string, unknown> | null)?.projectGroups)
    if (remote) {
      state.value = remote
      hasStored.value = true
      localStorage.setItem(STORAGE_KEY, JSON.stringify(remote))
    }
  } catch {
    // Offline / daemon down: the local cache is the answer.
  }
}

async function persist() {
  const { seeded: _drop, ...bare } = state.value
  localStorage.setItem(STORAGE_KEY, JSON.stringify(bare))
  hasStored.value = true
  try {
    // Whole-document replace with no CAS (RQ2-1 #12) — read-merge-write so
    // the appearance levers and future keys survive.
    const doc = (await uiApi.get()) ?? {}
    await uiApi.put({ ...doc, projectGroups: bare })
  } catch {
    // Local copy stands; the next successful save pushes the merged state.
  }
}

export function useProjectGroups() {
  /** Offer the working_dir seed when nothing was ever stored. */
  function seedIfEmpty(projects: { name: string; work_dir?: string }[]) {
    if (hasStored.value || state.value.order.length > 0) return
    state.value = seedGroups(projects)
  }

  async function apply(next: ProjectGroupsState) {
    if (next === state.value) return
    state.value = next
    await persist()
  }

  return {
    state,
    load,
    seedIfEmpty,
    assign: (project: string, group: string) => apply(assignFn(state.value, project, group)),
    rename: (from: string, to: string) => apply(renameFn(state.value, from, to)),
    remove: (group: string) => apply(removeFn(state.value, group)),
    create: async () => {
      const { next, name } = createFn(state.value)
      await apply(next)
      return name
    },
    toggleCollapsed: (group: string) => apply(toggleFn(state.value, group)),
  }
}
