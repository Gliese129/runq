<template>
  <div>
    <!-- Pattern line: what it is, how many it found, where, and when -->
    <div class="d-flex align-center flex-wrap ga-2">
      <code class="text-primary text-body-2">{{ row.glob }}</code>
      <v-chip size="x-small" variant="tonal" color="primary" :title="t('submit.glob_live_hint')">
        {{ t('submit.glob_live') }}
      </v-chip>
      <span v-if="resolving" class="text-caption text-on-surface-variant">{{ t('submit.glob_resolving') }}</span>
      <span v-else-if="error" class="text-caption text-error">{{ error }}</span>
      <span v-else class="text-caption text-on-surface-variant">
        {{ t('submit.glob_n_resolved', { n: candidates.length, target: state.target }) }}
      </span>
      <v-btn
        icon size="x-small" variant="text" :loading="resolving"
        :aria-label="t('submit.glob_rescan')" :title="t('submit.glob_rescan')"
        @click="resolve"
      >
        <v-icon size="13" color="on-surface-variant">mdi-refresh</v-icon>
      </v-btn>

      <!-- Saved groups: a selection worth keeping, kept in ui.json (never in
           project.yaml — a selection is not project config) -->
      <v-menu :close-on-content-click="false" location="bottom start">
        <template #activator="{ props: menuProps }">
          <v-btn v-bind="menuProps" size="x-small" variant="text" class="text-none">
            <v-icon start size="13">mdi-bookmark-outline</v-icon>{{ t('submit.glob_groups') }}
          </v-btn>
        </template>
        <v-card class="pa-3" min-width="280">
          <div class="text-caption text-on-surface-variant mb-2">{{ t('submit.glob_saved_groups') }}</div>
          <div v-if="Object.keys(savedGroups).length === 0" class="text-caption text-on-surface-variant mb-2">
            {{ t('submit.glob_no_groups') }}
          </div>
          <div
            v-for="(values, name) in savedGroups" :key="name"
            class="d-flex align-center ga-2 mb-1"
          >
            <v-btn size="x-small" variant="tonal" class="text-none flex-grow-1 justify-start" @click="applyGroup(name, values)">
              {{ name }}
              <span class="text-caption text-on-surface-variant ml-2">{{ values.length }}</span>
            </v-btn>
            <v-btn icon size="x-small" variant="text" :aria-label="t('common.delete')" @click="groupsApi.deleteGroup(state.projectName, row.name, String(name))">
              <v-icon size="12" color="on-surface-variant">mdi-close</v-icon>
            </v-btn>
          </div>
          <v-divider class="my-2" />
          <div class="d-flex ga-1">
            <v-text-field
              v-model="newGroupName"
              :placeholder="t('submit.glob_group_name')"
              density="compact" variant="underlined" hide-details
              style="font-size: 12px"
              @keydown.enter.prevent="saveCurrentSelection"
            />
            <v-btn
              size="x-small" variant="text" color="primary" class="text-none"
              :disabled="!newGroupName.trim() || selectedCount === 0"
              @click="saveCurrentSelection"
            >
              {{ t('submit.glob_save_group', { n: selectedCount }) }}
            </v-btn>
          </div>
        </v-card>
      </v-menu>
    </div>

    <!-- Candidates: click to include / exclude THIS submit -->
    <div v-if="candidates.length > 0" class="d-flex align-center flex-wrap ga-1 mt-1">
      <v-chip
        v-for="c in candidates" :key="c.path"
        size="x-small" class="cursor-pointer font-mono"
        :variant="isSelected(c.path) ? 'tonal' : 'outlined'"
        :color="isSelected(c.path) ? 'primary' : undefined"
        :class="{ 'chip-off': !isSelected(c.path) }"
        :title="c.path"
        @click="toggle(c.path)"
      >{{ shortName(c.path) }}</v-chip>
    </div>
    <div v-else-if="!resolving && !error" class="text-caption text-on-surface-variant mt-1">
      {{ t('submit.glob_nothing_matched') }}
    </div>

    <!-- Selection summary + bulk actions -->
    <div class="d-flex align-center flex-wrap ga-2 mt-1">
      <span class="text-caption text-on-surface-variant">
        {{ t('submit.glob_n_selected', { n: selectedCount, total: candidates.length }) }}
      </span>
      <v-btn size="x-small" variant="text" class="text-none" :disabled="selectedCount === candidates.length" @click="selectAll">
        {{ t('submit.glob_all') }}
      </v-btn>
      <v-btn size="x-small" variant="text" class="text-none" :disabled="selectedCount === 0" @click="selectNone">
        {{ t('submit.glob_none') }}
      </v-btn>
      <span v-if="truncated" class="text-caption text-warning">{{ t('submit.glob_truncated') }}</span>
      <span v-if="missing.length > 0" class="text-caption text-warning">
        {{ t('submit.glob_missing', { n: missing.length }) }}
      </span>
    </div>
  </div>
</template>

<script setup lang="ts">
// One glob param's row body (RQ2-3 c4). The pattern lives in project.yaml;
// everything here — which matches to include, saved groups — is a UI choice
// that never writes back to the project config.
//
// row.values IS the submitted selection; `candidates` is the resolved set it
// is chosen from. Resolution happens on the owning side (fs/glob).
import { computed, inject, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { filesApi } from '@/apis/files'
import { useParamGroups } from '@/composables/useParamGroups'
import { mergeGlobSelection } from './globSelection'
import { useSnackbar } from '@/composables/useSnackbar'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import type { ParamRow } from './paramTable'
import type { FSEntry } from '@/types/api'

const props = defineProps<{ row: ParamRow }>()

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const snack = useSnackbar()
const groupsApi = useParamGroups()

const candidates = ref<FSEntry[]>([])
const resolving = ref(false)
const truncated = ref(false)
const error = ref('')
/** Selected paths that the latest scan no longer matches (deleted files). */
const missing = ref<string[]>([])
const newGroupName = ref('')

const savedGroups = computed(() => groupsApi.groupsFor(state.projectName, props.row.name))
const selectedCount = computed(() => props.row.values.length)

function shortName(p: string): string {
  return p.split('/').pop() || p
}
function isSelected(p: string): boolean {
  return props.row.values.includes(p)
}

function toggle(p: string) {
  const i = props.row.values.indexOf(p)
  if (i >= 0) props.row.values.splice(i, 1)
  else props.row.values.push(p)
}
function selectAll() {
  props.row.values.splice(0, props.row.values.length, ...candidates.value.map(c => c.path))
}
function selectNone() {
  props.row.values.splice(0, props.row.values.length)
}

// Monotonic scan id: reload/draft-restore mounts a scan while the target
// is still settling, so overlapping resolves are ROUTINE, not a corner
// case — and responses may land out of order. Only the LATEST scan may
// write anything (candidates, values, globState); a stale response that
// lands late is discarded wholesale (Codex r2).
let resolveSeq = 0

async function resolve() {
  if (!props.row.glob) return
  const seq = ++resolveSeq
  resolving.value = true
  props.row.globState = 'pending'
  error.value = ''
  const before = candidates.value.map(c => c.path)
  try {
    const res = await filesApi.glob(state.newProject.workDir || '', props.row.glob, { target: state.target })
    if (seq !== resolveSeq) return // a newer scan owns the outcome
    candidates.value = res.items
    truncated.value = res.truncated
    // Fresh row → all; hydrated snapshot (?fromJob / draft) → keep the
    // frozen subset; rescan → keep + adopt new (see globSelection.ts).
    const { next, missing: gone } = mergeGlobSelection(
      before, [...props.row.values], res.items.map(c => c.path))
    missing.value = gone
    props.row.values.splice(0, props.row.values.length, ...next)
    props.row.globState = 'ok'
  } catch (e: any) {
    if (seq !== resolveSeq) return
    error.value = e?.message || t('common.error')
    candidates.value = []
    truncated.value = false
    // Stale values must not ride into a submit the resolver can't vouch
    // for — the error state gates validateTable (Codex r1 F2).
    props.row.globState = 'error'
  } finally {
    if (seq === resolveSeq) resolving.value = false
  }
}

function applyGroup(name: string | number, values: string[]) {
  const present = candidates.value.map(c => c.path)
  const hit = values.filter(v => present.includes(v))
  const gone = values.length - hit.length
  props.row.values.splice(0, props.row.values.length, ...hit)
  if (gone > 0) snack.warn(t('submit.glob_group_partial', { name: String(name), n: gone }))
}

async function saveCurrentSelection() {
  const name = newGroupName.value.trim()
  if (!name || selectedCount.value === 0) return
  await groupsApi.saveGroup(state.projectName, props.row.name, name, [...props.row.values])
  newGroupName.value = ''
  snack.success(t('submit.glob_group_saved', { name }))
}

onMounted(() => {
  void groupsApi.load()
  void resolve()
})

// The pattern is project config and the target decides whose filesystem
// answers — either changing invalidates the current candidate set.
watch(() => [props.row.glob, state.target, state.newProject.workDir], () => void resolve())
</script>

<style scoped>
.font-mono { font-family: var(--font-mono); }
.chip-off { opacity: 0.55; }
.chip-off:hover { opacity: 0.85; }
</style>
