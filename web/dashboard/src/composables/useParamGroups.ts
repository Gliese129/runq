// Saved value groups for glob params (RQ2-3 c4).
//
// A glob param resolves to today's matches; which of them a submit uses is a
// per-submit choice (that is the whole reason freeze was dropped from
// project.yaml — a selection is not project config). But some selections are
// worth keeping: "the 8 checkpoints the paper used", "the clean shards".
// Those live here: named lists in ui.json, so they roam with the daemon the
// same way the appearance levers do, with localStorage as the offline cache.
//
// Storage shape (ui.json is an opaque blob to the backend — this key is ours):
//   paramGroups: { "<project>::<param>": { "<group name>": ["path", ...] } }

import { ref } from 'vue'
import { uiApi } from '@/apis/ui'

export type ParamGroups = Record<string, Record<string, string[]>>

const STORAGE_KEY = 'runq-param-groups'
const groups = ref<ParamGroups>(readLocal())
let loaded = false

function readLocal(): ParamGroups {
  try {
    return sanitize(JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}'))
  } catch {
    return {}
  }
}

/** ui.json is hand-editable and roams between versions: validate shape
 *  key-by-key rather than trusting it. */
function sanitize(raw: unknown): ParamGroups {
  if (typeof raw !== 'object' || raw === null) return {}
  const out: ParamGroups = {}
  for (const [key, named] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof named !== 'object' || named === null) continue
    const bucket: Record<string, string[]> = {}
    for (const [name, values] of Object.entries(named as Record<string, unknown>)) {
      if (Array.isArray(values)) bucket[name] = values.filter(v => typeof v === 'string')
    }
    if (Object.keys(bucket).length > 0) out[key] = bucket
  }
  return out
}

export function groupKey(project: string, param: string): string {
  return `${project}::${param}`
}

/** Load the roaming copy once per session; the local cache already applied. */
async function load() {
  if (loaded) return
  loaded = true
  try {
    const doc = await uiApi.get()
    const remote = sanitize((doc as Record<string, unknown> | null)?.paramGroups)
    if (Object.keys(remote).length > 0) {
      groups.value = remote
      localStorage.setItem(STORAGE_KEY, JSON.stringify(remote))
    }
  } catch {
    // Offline / daemon down: the local cache is the answer.
  }
}

async function persist() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(groups.value))
  try {
    // Whole-document replace with no CAS (RQ2-1 #12) — read-merge-write so
    // the appearance levers and future keys survive.
    const doc = (await uiApi.get()) ?? {}
    await uiApi.put({ ...doc, paramGroups: groups.value })
  } catch {
    // Local copy stands; the next successful save pushes the merged state.
  }
}

export function useParamGroups() {
  function groupsFor(project: string, param: string): Record<string, string[]> {
    return groups.value[groupKey(project, param)] ?? {}
  }

  async function saveGroup(project: string, param: string, name: string, values: string[]) {
    const key = groupKey(project, param)
    const trimmed = name.trim()
    if (!trimmed || values.length === 0) return
    groups.value = {
      ...groups.value,
      [key]: { ...groups.value[key], [trimmed]: [...values] },
    }
    await persist()
  }

  async function deleteGroup(project: string, param: string, name: string) {
    const key = groupKey(project, param)
    const bucket = { ...groups.value[key] }
    delete bucket[name]
    const next = { ...groups.value }
    if (Object.keys(bucket).length > 0) next[key] = bucket
    else delete next[key]
    groups.value = next
    await persist()
  }

  return { groups, load, groupsFor, saveGroup, deleteGroup }
}
