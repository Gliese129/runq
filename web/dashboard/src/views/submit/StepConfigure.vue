<template>
  <div class="mx-auto" style="max-width: 760px">
    <!-- Project + note -->
    <v-card class="pa-3 mb-3">
      <div class="d-flex align-center ga-2">
        <v-icon size="16" color="primary">mdi-folder-outline</v-icon>
        <span class="text-body-2 font-weight-medium flex-shrink-0">{{ state.projectName }}</span>
        <v-text-field
          v-model="state.note"
          :label="t('submit.note')"
          :placeholder="t('submit.note_placeholder')"
          prepend-inner-icon="mdi-text-short"
          density="compact" variant="outlined" hide-details class="ml-4"
        />
      </div>
    </v-card>

    <!-- Flat param table -->
    <v-card class="pa-0">
      <div class="d-flex align-center justify-space-between px-4 py-3 border-b">
        <span class="text-subtitle-2">Parameters</span>
        <code class="text-body-2" :class="validation.ok ? 'text-on-surface-variant' : 'text-error'">
          {{ formula }}
        </code>
      </div>

      <!-- Speed tip (one-time, dismissible) -->
      <div v-if="!prefs.sugarTipDismissed.value" class="d-flex align-center ga-2 px-4 py-2 border-b text-caption text-on-surface-variant">
        <v-icon size="12" color="primary">mdi-lightning-bolt-outline</v-icon>
        <span>
          Speed tip: type <code>log 1e-4 1e-1 4</code>, <code>linear 1 5 1</code> or <code>seeds 3</code>
          directly in a numeric cell — preview appears, Enter expands.
        </span>
        <v-spacer />
        <v-btn size="x-small" variant="text" @click="prefs.sugarTipDismissed.value = true">Got it</v-btn>
      </div>

      <div
        v-for="row in state.rows" :key="row.name"
        class="row-line d-flex px-3 py-2 border-b"
        :style="rowStyle(row.name)"
      >
        <!-- select -->
        <div class="flex-shrink-0 d-flex align-start pt-1">
          <v-checkbox-btn
            :model-value="selected.has(row.name)"
            density="compact" class="row-check"
            :class="{ 'row-check--visible': selected.size > 0 || isLinked(row.name) }"
            @update:model-value="toggleSelect(row.name)"
          />
        </div>

        <!-- name + type + link icon -->
        <div class="flex-shrink-0 pt-2" style="width: 170px">
          <div class="d-flex align-center ga-1">
            <v-icon
              v-if="isLinked(row.name)" size="13"
              :style="{ color: linkColorOf(row.name) }"
              class="cursor-pointer"
              :title="'Show aligned view'"
              @click="toggleAligned(setIdOf(row.name))"
            >mdi-link</v-icon>
            <span class="text-body-2 font-weight-medium text-truncate font-mono">{{ row.name }}</span>
          </div>
          <div class="mt-1">
            <span class="text-caption text-on-surface-variant">{{ row.type }}</span>
          </div>
        </div>

        <!-- values -->
        <div class="flex-grow-1 min-w-0 px-2">
          <ParamValueEditor
            v-model="row.values"
            :type="row.type || 'str'"
            placeholder="Type value + Enter"
            color="primary"
            :default-value="row.default"
            :suggestions="suggestionsFor(row.name)"
            :collapsible="!isLinked(row.name)"
          />
        </div>

        <!-- effect + custom-row delete -->
        <div class="flex-shrink-0 d-flex align-center justify-end ga-1 pt-1" style="width: 86px">
          <span class="text-caption font-mono" :style="effectStyle(row)">{{ effectLabel(row) }}</span>
          <v-btn
            v-if="customNames.has(row.name)"
            icon size="x-small" variant="text" class="row-delete"
            :aria-label="`Remove ${row.name}`" :title="`Remove ${row.name}`"
            @click="removeCustomParam(row.name)"
          >
            <v-icon size="13" color="on-surface-variant">mdi-close</v-icon>
          </v-btn>
        </div>
      </div>

      <!-- Aligned view for an expanded link set -->
      <div v-if="alignedSet" class="px-4 py-3 border-b" :style="{ background: tint(alignedColor, '12') }">
        <div class="d-flex align-center ga-2 mb-2">
          <v-icon size="13" :style="{ color: alignedColor }">mdi-link</v-icon>
          <span class="text-caption" :style="{ color: alignedColor }">Aligned view — values pair up row by row</span>
          <v-spacer />
          <v-btn size="x-small" variant="text" @click="alignedSetId = null">Close</v-btn>
        </div>
        <table class="w-100 data-mono" style="font-size: 12px">
          <thead>
            <tr>
              <th class="text-caption" style="width: 32px">#</th>
              <th v-for="m in alignedSet.members" :key="m" class="text-caption font-weight-medium">{{ m }}</th>
              <th style="width: 32px"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(_, i) in alignedRowCount" :key="i">
              <td class="text-caption text-on-surface-variant">{{ i + 1 }}</td>
              <td v-for="m in alignedSet.members" :key="m">
                <input
                  class="aligned-cell"
                  :value="rowByName(m)?.values[i] ?? ''"
                  @input="setAlignedCell(m, i, ($event.target as HTMLInputElement).value)"
                />
              </td>
              <td>
                <v-btn icon size="x-small" variant="text" @click="removeAlignedRow(i)">
                  <v-icon size="12" color="on-surface-variant">mdi-close</v-icon>
                </v-btn>
              </td>
            </tr>
          </tbody>
        </table>
        <v-btn size="x-small" variant="text" class="mt-1" :style="{ color: alignedColor }" @click="addAlignedRow">
          <v-icon start size="12">mdi-plus</v-icon> Add pair
        </v-btn>
      </div>

      <!-- Custom param add — suggests project params hidden by curation -->
      <div class="d-flex align-center ga-2 px-4 py-2">
        <v-combobox
          v-model="newParamName"
          :items="hiddenParamSuggestions"
          placeholder="custom param..."
          density="compact" variant="underlined" hide-details
          class="font-mono" style="max-width: 260px; font-size: 13px"
          @keydown.enter="addCustomParam"
        >
          <template #item="{ item, props: itemProps }">
            <v-list-item v-bind="itemProps" density="compact">
              <template #append>
                <span class="text-caption text-on-surface-variant">project param</span>
              </template>
            </v-list-item>
          </template>
        </v-combobox>
        <v-btn icon size="x-small" variant="text" color="primary" :disabled="!(newParamName || '').trim()" @click="addCustomParam">
          <v-icon size="14">mdi-plus</v-icon>
        </v-btn>
        <v-spacer />
        <span v-if="!validation.ok" class="text-caption text-error">{{ validation.message }}</span>
      </div>
    </v-card>

    <!-- Selection action bar -->
    <v-slide-y-reverse-transition>
      <v-card v-if="selected.size > 0" class="mt-3 px-4 py-2 d-flex align-center ga-3">
        <span class="text-caption text-on-surface-variant">{{ selected.size }} selected</span>
        <v-btn
          v-if="canLink" size="small" variant="tonal" color="primary"
          @click="linkSelected"
        >
          <v-icon start size="14">mdi-link</v-icon> Link
        </v-btn>
        <v-btn
          v-if="canUnlink" size="small" variant="tonal"
          @click="unlinkSelected"
        >
          <v-icon start size="14">mdi-link-off</v-icon> Unlink
        </v-btn>
        <span v-if="linkPreview" class="text-caption" :class="linkPreview.warn ? 'text-error' : 'text-on-surface-variant'">
          {{ linkPreview.text }}
        </span>
        <v-spacer />
        <v-btn size="x-small" variant="text" @click="selected.clear()">Clear</v-btn>
      </v-card>
    </v-slide-y-reverse-transition>

    <p v-if="state.rows.length > 1 && state.linkSets.length === 0 && !hintDismissed" class="text-caption text-on-surface-variant mt-2 px-1">
      <v-icon size="12">mdi-lightbulb-outline</v-icon>
      Want some params to vary together (zip)? Select their rows, then Link.
      <v-btn size="x-small" variant="text" @click="hintDismissed = true">Got it</v-btn>
    </p>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import ParamValueEditor from '@/components/ParamValueEditor.vue'
import { usePreferences } from '@/composables/usePreferences'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import { inject } from 'vue'
import {
  activeValues, linkColor, rowEffect, validateTable, taskCount,
  type LinkSet, type ParamRow,
} from './paramTable'
import { sweepSummary } from './submitFlow'

const { t } = useI18n()
const state = inject(SUBMIT_STATE_KEY)!
const prefs = usePreferences()

const selected = reactive(new Set<string>())
const newParamName = ref('')
const customNames = reactive(new Set<string>())
const hintDismissed = ref(false)
let setCounter = 0

// ── Row sync: project params (include) → rows, preserving user edits ──
watch(
  () => state.newProject.params,
  () => {
    const byName = new Map(state.rows.map(r => [r.name, r]))
    const next: ParamRow[] = []
    for (const p of state.newProject.params.filter(p => p.include)) {
      const existing = byName.get(p.name)
      if (existing) {
        existing.type = p.type || existing.type
        existing.default = p.default || ''
        next.push(existing)
      } else {
        const meta: ParamRow['meta'] = {}
        if (p.min != null) meta.min = p.min
        if (p.max != null) meta.max = p.max
        next.push({ name: p.name, type: p.type || 'str', default: p.default || '', values: [], meta })
      }
      byName.delete(p.name)
    }
    for (const [name, row] of byName) {
      // Keep rows added by hand AND rows carrying values (e.g. decompiled
      // from a re-run template before the project palette has loaded).
      if (customNames.has(name) || row.values.length > 0) next.push(row)
    }
    state.rows = next
    pruneLinkSets()
  },
  { deep: true, immediate: true },
)

function pruneLinkSets() {
  const names = new Set(state.rows.map(r => r.name))
  state.linkSets = state.linkSets
    .map(s => ({ ...s, members: s.members.filter(m => names.has(m)) }))
    .filter(s => s.members.length >= 2)
}

// ── Link sets ──

function setOf(name: string): LinkSet | undefined {
  return state.linkSets.find(s => s.members.includes(name))
}
function isLinked(name: string): boolean { return !!setOf(name) }
function setIdOf(name: string): string | null { return setOf(name)?.id ?? null }
function setIndexOf(name: string): number {
  return state.linkSets.findIndex(s => s.members.includes(name))
}
function linkColorOf(name: string): string {
  const i = setIndexOf(name)
  return i >= 0 ? linkColor(i) : 'inherit'
}

// A link set is atomic in selection: selecting any member selects the whole
// set, deselecting deselects the whole set. Linking a selection that already
// contains linked rows MERGES everything into one new set.
const canLink = computed(() => {
  if (selected.size < 2) return false
  // No-op guard: selection exactly equals one existing set → nothing to merge
  return !state.linkSets.some(s =>
    s.members.length === selected.size && s.members.every(m => selected.has(m)),
  )
})
const canUnlink = computed(() =>
  selected.size > 0 && [...selected].some(n => isLinked(n)),
)

const linkPreview = computed(() => {
  if (!canLink.value) return null
  const lengths = [...selected].map(n => activeValues(rowByName(n) ?? { values: [] }).length)
  const merging = [...selected].some(n => isLinked(n))
  const verb = merging ? 'merge into one zip' : 'zip'
  const uniq = new Set(lengths)
  if (uniq.size === 1) return { text: `${lengths.join(' + ')} values → ${verb} ×${lengths[0]}`, warn: false }
  return { text: `${lengths.join(' + ')} values → lengths must match after linking`, warn: true }
})

function linkSelected() {
  if (!canLink.value) return
  // Dissolve any sets touched by the selection, then create one merged set
  // in table row order (deterministic zip column order).
  const touched = new Set([...selected].map(n => setIdOf(n)).filter(Boolean))
  state.linkSets = state.linkSets.filter(s => !touched.has(s.id))
  const members = state.rows.filter(r => selected.has(r.name)).map(r => r.name)
  state.linkSets.push({ id: `ls${setCounter++}`, members })
  if (alignedSetId.value && touched.has(alignedSetId.value)) alignedSetId.value = null
  selected.clear()
}

function unlinkSelected() {
  const hit = new Set([...selected].map(n => setIdOf(n)).filter(Boolean))
  state.linkSets = state.linkSets.filter(s => !hit.has(s.id))
  selected.clear()
  if (alignedSetId.value && hit.has(alignedSetId.value)) alignedSetId.value = null
}

function toggleSelect(name: string) {
  // Atomic set selection: toggling any member toggles all of them.
  const names = setOf(name)?.members ?? [name]
  const on = !selected.has(name)
  for (const n of names) {
    if (on) selected.add(n)
    else selected.delete(n)
  }
}

// ── Aligned view ──

const alignedSetId = ref<string | null>(null)
const alignedSet = computed(() => state.linkSets.find(s => s.id === alignedSetId.value) ?? null)
const alignedColor = computed(() => {
  const i = state.linkSets.findIndex(s => s.id === alignedSetId.value)
  return i >= 0 ? linkColor(i) : 'inherit'
})
const alignedRowCount = computed(() => {
  if (!alignedSet.value) return 0
  return Math.max(1, ...alignedSet.value.members.map(m => rowByName(m)?.values.length ?? 0))
})

function toggleAligned(id: string | null) {
  alignedSetId.value = alignedSetId.value === id ? null : id
}
function setAlignedCell(member: string, idx: number, value: string) {
  const row = rowByName(member)
  if (!row) return
  while (row.values.length <= idx) row.values.push('')
  row.values[idx] = value
}
function addAlignedRow() {
  for (const m of alignedSet.value?.members ?? []) rowByName(m)?.values.push('')
}
function removeAlignedRow(idx: number) {
  for (const m of alignedSet.value?.members ?? []) {
    const row = rowByName(m)
    if (row && idx < row.values.length) row.values.splice(idx, 1)
  }
}

// ── Row helpers ──

function rowByName(name: string): ParamRow | undefined {
  return state.rows.find(r => r.name === name)
}

function suggestionsFor(name: string): string[] {
  const p = (state.newProject.params || []).find(p => p.name === name)
  return (p as any)?.values || []
}

// Project params hidden by curation (include=false) — surfaced as
// suggestions so step 2 users can pull one in without going back to step 1.
const hiddenParamSuggestions = computed(() =>
  (state.newProject.params || [])
    .filter(p => !p.include && !state.rows.some(r => r.name === p.name))
    .map(p => p.name),
)

function addCustomParam() {
  const name = (newParamName.value || '').trim()
  if (!name || state.rows.some(r => r.name === name)) { newParamName.value = ''; return }
  // If it's a known (but unchecked) project param, inherit its definition.
  const def = (state.newProject.params || []).find(p => p.name === name)
  customNames.add(name)
  if (def) {
    const meta: import('./paramTable').ParamRow['meta'] = {}
    if (def.min != null) meta.min = def.min
    if (def.max != null) meta.max = def.max
    state.rows.push({ name, type: def.type || 'str', default: def.default || '', values: [], meta })
  } else {
    state.rows.push({ name, type: 'str', default: '', values: [] })
  }
  newParamName.value = ''
}

/** Custom rows are deletable (project rows are managed by StepProject's
 *  include toggle — deleting them here would just resurrect on next sync). */
function removeCustomParam(name: string) {
  if (!customNames.has(name)) return
  customNames.delete(name)
  selected.delete(name)
  state.rows = state.rows.filter(r => r.name !== name)
  pruneLinkSets()
  if (alignedSetId.value && !state.linkSets.some(s => s.id === alignedSetId.value)) {
    alignedSetId.value = null
  }
}

// ── Display ──

function tint(color: string, alpha: string): string {
  return color === 'inherit' ? 'transparent' : `${color}${alpha}`
}

function rowStyle(name: string) {
  const i = setIndexOf(name)
  if (i < 0) return {}
  const c = linkColor(i)
  return { borderLeft: `3px solid ${c}`, background: tint(c, '0D') }
}

function effectLabel(row: ParamRow): string {
  switch (rowEffect(row, state.linkSets)) {
    case 'linked': {
      const set = setOf(row.name)!
      const lengths = set.members.map(m => activeValues(rowByName(m) ?? { values: [] }).length)
      const n = activeValues(row).length
      return new Set(lengths).size > 1 ? `${n} ≠` : `zip ×${n}`
    }
    case 'sweep': return `×${activeValues(row).length}`
    case 'fixed': return 'fixed'
    case 'fixed-default': return 'default'
    default: return '—'
  }
}

function effectStyle(row: ParamRow) {
  const effect = rowEffect(row, state.linkSets)
  if (effect === 'linked') {
    const set = setOf(row.name)!
    const lengths = set.members.map(m => activeValues(rowByName(m) ?? { values: [] }).length)
    if (new Set(lengths).size > 1) return { color: 'rgb(var(--v-theme-error))' }
    return { color: linkColorOf(row.name) }
  }
  if (effect === 'sweep') return { color: 'rgb(var(--v-theme-primary))' }
  return { color: 'rgb(var(--v-theme-on-surface-variant))' }
}

const validation = computed(() => validateTable(state.rows, state.linkSets))

const formula = computed(() => {
  const summary = sweepSummary(state.rows, state.linkSets)
  const n = taskCount(state.rows, state.linkSets)
  return summary ? `${summary} = ${n} ${n === 1 ? 'task' : 'tasks'}` : '1 task (no sweep)'
})
</script>

<style scoped>
.border-b { border-bottom: 0.5px solid rgb(var(--v-theme-outline-variant)); }
.font-mono { font-family: monospace; }
.min-w-0 { min-width: 0; }
.row-line { transition: background 0.15s ease; }
.row-line:hover { background: rgb(var(--v-theme-surface-variant), 0.3); }
.row-check { opacity: 0; transition: opacity 0.15s ease; }
.row-line:hover .row-check, .row-check--visible { opacity: 1; }
.row-delete { opacity: 0; transition: opacity 0.15s ease; }
.row-line:hover .row-delete { opacity: 1; }
.aligned-cell { width: 100%; border: none; background: transparent; outline: none; font-family: inherit; font-size: inherit; padding: 2px 0; border-bottom: 1px solid transparent; color: inherit; }
.aligned-cell:focus { border-bottom-color: rgb(var(--v-theme-primary)); }
</style>
