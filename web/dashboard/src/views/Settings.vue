<template>
  <div style="max-width: 640px; margin: 0 auto">
    <div class="text-h5 font-weight-bold mb-6">{{ t('settings.title') }}</div>

    <!-- System (editable) -->
    <v-card class="mb-4 pa-5">
      <div class="text-subtitle-2 mb-3">{{ t('settings.system') }}</div>
      <div class="d-flex flex-column ga-3">
        <div class="d-flex align-center justify-space-between ga-4">
          <span class="text-on-surface-variant flex-shrink-0">{{ t('settings.default_target') }}</span>
          <v-select
            v-model="globalDefaultTarget"
            :items="config.targets.map(x => x.name)"
            density="compact" variant="outlined" hide-details
            style="max-width: 360px"
          />
        </div>
        <div class="d-flex align-center justify-space-between ga-4">
          <span class="text-on-surface-variant flex-shrink-0">{{ t('settings.data_path') }}</span>
          <v-text-field
            v-model="globalDataPath"
            :placeholder="t('settings.data_path_default')"
            density="compact" variant="outlined" hide-details
            class="font-mono" style="max-width: 360px"
          />
        </div>
        <div class="d-flex align-center justify-space-between">
          <span class="text-on-surface-variant">{{ t('settings.config_path') }}</span>
          <code class="text-body-2 cursor-pointer" @click="copyToClipboard(config.configPath)">
            {{ config.configPath }}
            <v-icon size="12" class="ml-1" color="on-surface-variant">mdi-content-copy</v-icon>
          </code>
        </div>
        <div v-if="globalDirty" class="d-flex align-center ga-2">
          <v-btn size="small" variant="tonal" color="primary" :loading="savingGlobal" @click="saveGlobal">{{ t('common.save') }}</v-btn>
          <span class="text-caption text-on-surface-variant">{{ t('settings.restart_hint') }}</span>
        </div>
      </div>
    </v-card>

    <!-- HPC cluster templates: every field is always rendered (schema-driven),
         whether or not it exists in the file yet -->
    <v-card class="mb-4 pa-5">
      <div class="d-flex align-center justify-space-between mb-1">
        <div class="d-flex align-center ga-3">
          <div class="text-subtitle-2">{{ t('settings.hpc_title') }}</div>
          <v-select
            v-model="selectedTarget"
            :items="targetNames"
            density="compact" variant="outlined" hide-details
            class="font-mono" style="min-width: 160px"
          />
        </div>
        <div class="d-flex ga-2">
          <v-btn size="x-small" variant="tonal" :disabled="!selectedTarget" @click="checkHPC">
            <v-icon start size="12">mdi-stethoscope</v-icon> {{ t('settings.hpc_check') }}
          </v-btn>
          <v-btn size="x-small" variant="tonal" color="primary" :loading="savingHPC" :disabled="!selectedTarget" @click="saveHPC">{{ t('common.save') }}</v-btn>
        </div>
      </div>
      <div class="text-caption text-on-surface-variant mb-3">
        {{ t('settings.hpc_rewrite_note') }}
      </div>

      <!-- RQ-75: previous generations of THIS target still tracking tasks -->
      <div
        v-for="g in retiringOfSelected" :key="g.generation"
        class="d-flex align-center ga-2 rounded pa-2 mb-3"
        style="background: rgb(var(--v-theme-surface-variant), 0.25)"
      >
        <v-icon size="14" color="warning">mdi-history</v-icon>
        <span class="text-caption">
          {{ t('settings.gen_retiring_row', { n: g.unfinished }) }}
        </span>
        <code class="text-caption text-on-surface-variant">{{ g.generation.slice(0, 8) }}</code>
        <span class="text-caption text-on-surface-variant ml-auto">{{ genDate(g.retired_at) }}</span>
      </div>

      <!-- Presets: same starter templates as `hpc init --scheduler` -->
      <div v-if="presetNames.length > 0" class="d-flex align-center flex-wrap ga-1 mb-4">
        <span class="text-caption text-on-surface-variant mr-1">{{ t('settings.hpc_preset_label') }}</span>
        <v-chip
          v-for="name in presetNames" :key="name"
          size="x-small" variant="outlined" class="cursor-pointer font-mono"
          @click="loadPreset(name)"
        >{{ name }}</v-chip>
      </div>

      <!-- Natural-language label + literal yaml key (Check results and CLI
           users reference the key — it must stay visible) -->
      <template v-for="f in hpcFields" :key="f.key">
        <div class="d-flex align-baseline ga-2 mb-1">
          <span class="text-body-2 font-weight-medium">{{ t(f.labelKey) }}</span>
          <code class="text-caption text-on-surface-variant">{{ f.key }}</code>
        </div>
        <v-text-field
          v-if="f.key === 'submit_id_regex'"
          v-model="hpcForm[f.key] as string"
          :placeholder="f.placeholder"
          density="compact" variant="outlined"
          class="font-mono mb-3"
          :hint="t('settings.hpc_regex_hint')"
          persistent-hint
        />
        <template v-else>
          <div class="tmpl-display rounded pa-2 mb-1 cursor-pointer d-flex align-center ga-2" @click="openEditor(f.key)">
            <span class="font-mono flex-grow-1 text-body-2" :class="{ 'opacity-50 font-italic': !hpcForm[f.key] }" style="word-break: break-all">
              {{ hpcForm[f.key] || t('settings.tmpl_not_set') }}
            </span>
            <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
          </div>
          <div class="text-caption text-on-surface-variant mb-3">{{ t(f.hintKey) }}</div>
        </template>
      </template>

      <!-- status_parser: list of pipeline stages -->
      <div class="d-flex align-baseline ga-2 mb-1">
        <span class="text-body-2 font-weight-medium">{{ t('settings.hpc_f_parser') }}</span>
        <code class="text-caption text-on-surface-variant">status_parser</code>
      </div>
      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.hpc_f_parser_hint') }}</div>
      <div v-for="(stage, i) in hpcParser" :key="i" class="d-flex ga-2 mb-1 align-center">
        <div class="tmpl-display rounded pa-2 cursor-pointer d-flex align-center ga-2 flex-grow-1" @click="openStageEditor(i)">
          <span class="font-mono flex-grow-1 text-body-2" :class="{ 'opacity-50': !stage }" style="word-break: break-all">
            {{ stage || `stage ${i + 1}` }}
          </span>
          <v-icon size="14" color="on-surface-variant" class="flex-shrink-0">mdi-pencil-outline</v-icon>
        </div>
        <v-btn icon size="x-small" variant="text" :aria-label="t('common.delete')" :title="t('common.delete')" @click="hpcParser.splice(i, 1)">
          <v-icon size="14">mdi-close</v-icon>
        </v-btn>
      </div>
      <v-btn size="x-small" variant="text" color="primary" class="mb-3" @click="hpcParser.push(''); openStageEditor(hpcParser.length - 1)">
        <v-icon start size="12">mdi-plus</v-icon> {{ t('settings.hpc_add_stage') }}
      </v-btn>

      <ShellTemplateEditor
        v-model="editorOpen"
        :value="editorValue"
        :title="editorTitle"
        :placeholders="editorPlaceholders"
        :hint="editorHint"
        @apply="onEditorApply"
      />

      <!-- Check results: same three-state grammar as preflight -->
      <div v-if="hpcResults.length > 0" class="mt-2">
        <div v-for="r in hpcResults" :key="r.name" class="d-flex align-start ga-2 py-1" style="font-size: 12px">
          <v-icon size="14" :color="r.status === 'ok' ? 'success' : r.status === 'fail' ? 'error' : 'grey'">
            {{ r.status === 'ok' ? 'mdi-check' : r.status === 'fail' ? 'mdi-alert-circle' : 'mdi-minus' }}
          </v-icon>
          <code class="flex-shrink-0" style="width: 140px">{{ r.name }}</code>
          <span class="text-on-surface-variant font-mono" style="word-break: break-all">{{ r.detail }}</span>
        </div>
      </div>
    </v-card>

    <!-- RQ-75: archived targets — removed from config.yaml, shown collapsed.
         Still-retiring entries keep tracking their in-flight tasks. -->
    <v-expansion-panels v-if="archivedGenerations.length > 0" variant="accordion" class="mb-4">
      <v-expansion-panel>
        <v-expansion-panel-title class="text-caption text-on-surface-variant">
          <v-icon size="14" start>mdi-archive-outline</v-icon>
          {{ t('settings.gen_archived_title', { n: archivedGenerations.length }) }}
        </v-expansion-panel-title>
        <v-expansion-panel-text>
          <div
            v-for="g in archivedGenerations" :key="g.target + g.generation"
            class="d-flex align-center ga-2 py-1" style="font-size: 12px"
          >
            <v-icon size="14" :color="g.done_at ? 'grey' : 'warning'">
              {{ g.done_at ? 'mdi-check' : 'mdi-progress-clock' }}
            </v-icon>
            <code>{{ genLabel(g) }}</code>
            <span class="text-on-surface-variant">
              {{ g.done_at ? t('settings.gen_done') : t('settings.gen_tracking', { n: g.unfinished }) }}
            </span>
            <span class="text-on-surface-variant ml-auto">{{ genDate(g.retired_at) }}</span>
          </div>
        </v-expansion-panel-text>
      </v-expansion-panel>
    </v-expansion-panels>

    <!-- Webhook -->
    <v-card class="mb-4 pa-5">
      <div class="text-subtitle-2 mb-3">{{ t('settings.webhook') }}</div>
      <div class="text-caption text-on-surface-variant mb-3">{{ t('settings.webhook_hint') }}</div>

      <v-text-field
        v-model="webhookUrl"
        :label="t('settings.webhook_url')"
        placeholder="https://hooks.slack.com/services/..."
        prepend-inner-icon="mdi-webhook"
        class="mb-3"
      />

      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.webhook_events') }}</div>
      <div class="d-flex flex-wrap ga-2 mb-4">
        <v-chip
          v-for="evt in availableEvents"
          :key="evt.value"
          :variant="webhookEvents.includes(evt.value) ? 'flat' : 'outlined'"
          :color="webhookEvents.includes(evt.value) ? 'primary' : undefined"
          @click="toggleEvent(evt.value)"
        >
          <v-icon start size="14">{{ evt.icon }}</v-icon>
          {{ evt.label }}
        </v-chip>
      </div>

      <div class="d-flex ga-2">
        <v-btn variant="flat" color="primary" @click="saveWebhook" :loading="saving">
          {{ t('common.save') }}
        </v-btn>
        <v-btn variant="tonal" @click="testWebhook" :loading="testing" :disabled="!webhookUrl">
          {{ t('settings.webhook_test') }}
        </v-btn>
      </div>
    </v-card>

    <!-- Appearance -->
    <v-card class="pa-5">
      <div class="text-subtitle-2 mb-4">{{ t('settings.appearance') }}</div>

      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.theme') }}</div>
      <div class="d-flex ga-2 mb-5">
        <v-card
          v-for="th in themes"
          :key="th.value"
          class="pa-3 text-center flex-grow-1 cursor-pointer"
          :elevation="currentTheme === th.value ? 3 : 0"
          :color="currentTheme === th.value ? 'primary' : 'surface-variant'"
          :variant="currentTheme === th.value ? 'tonal' : 'flat'"
          @click="currentTheme = th.value"
        >
          <v-icon size="20" class="mb-1">{{ th.icon }}</v-icon>
          <div class="text-caption">{{ th.label }}</div>
        </v-card>
      </div>

      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.language') }}</div>
      <div class="d-flex ga-2 mb-5">
        <v-card
          v-for="lang in locales"
          :key="lang.value"
          class="pa-3 text-center flex-grow-1 cursor-pointer"
          :elevation="currentLocale === lang.value ? 3 : 0"
          :color="currentLocale === lang.value ? 'primary' : 'surface-variant'"
          :variant="currentLocale === lang.value ? 'tonal' : 'flat'"
          @click="currentLocale = lang.value"
        >
          <div class="text-body-2">{{ lang.flag }}</div>
          <div class="text-caption">{{ lang.label }}</div>
        </v-card>
      </div>

      <!-- Appearance levers (RQ2-2): density / surface / accent. State +
           persistence (ui.json roaming, localStorage fallback) live in
           useAppearance; these are plain selectors. -->
      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.density') }}</div>
      <div class="d-flex ga-2 mb-5">
        <v-card
          v-for="d in densities"
          :key="d.value"
          class="pa-3 text-center flex-grow-1 cursor-pointer"
          :color="appearance.density.value === d.value ? 'primary' : 'surface-variant'"
          :variant="appearance.density.value === d.value ? 'tonal' : 'flat'"
          @click="appearance.density.value = d.value"
        >
          <v-icon size="20" class="mb-1">{{ d.icon }}</v-icon>
          <div class="text-caption">{{ d.label }}</div>
        </v-card>
      </div>

      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.surface') }}</div>
      <div class="d-flex ga-2 mb-5">
        <v-card
          v-for="s in surfaces"
          :key="s.value"
          class="pa-3 text-center flex-grow-1 cursor-pointer"
          :color="appearance.surface.value === s.value ? 'primary' : 'surface-variant'"
          :variant="appearance.surface.value === s.value ? 'tonal' : 'flat'"
          @click="appearance.surface.value = s.value"
        >
          <v-icon size="20" class="mb-1">{{ s.icon }}</v-icon>
          <div class="text-caption">{{ s.label }}</div>
        </v-card>
      </div>

      <div class="text-caption text-on-surface-variant mb-2">{{ t('settings.accent') }}</div>
      <div class="d-flex ga-2 mb-5">
        <v-card
          v-for="(a, hex) in ACCENTS"
          :key="hex"
          class="pa-3 text-center flex-grow-1 cursor-pointer"
          :color="appearance.accent.value === hex ? 'primary' : 'surface-variant'"
          :variant="appearance.accent.value === hex ? 'tonal' : 'flat'"
          @click="appearance.accent.value = hex"
        >
          <span
            class="accent-swatch mb-1"
            :style="{ background: currentTheme === 'dark' ? a.dark : hex }"
          />
          <div class="text-caption">{{ a.name }}</div>
        </v-card>
      </div>

      <!-- Experimental (anime mode tucked away from the default view) -->
      <v-expansion-panels variant="accordion" class="mt-2">
        <v-expansion-panel>
          <v-expansion-panel-title class="text-caption text-on-surface-variant">
            <v-icon size="14" start>mdi-flask-outline</v-icon>
            {{ t('settings.experimental') }}
          </v-expansion-panel-title>
          <v-expansion-panel-text>
            <div class="d-flex align-center justify-space-between rounded-lg pa-3" style="background: rgb(var(--v-theme-surface-variant), 0.3)">
              <div>
                <div class="text-body-2 font-weight-medium">{{ t('settings.anime_mode') }}</div>
                <div class="text-caption text-on-surface-variant">{{ t('settings.anime_hint') }}</div>
              </div>
              <v-switch
                :model-value="settings.animeMode"
                @update:model-value="settings.setAnimeMode(Boolean($event))"
                hide-details
                color="primary"
                density="compact"
              />
            </div>
          </v-expansion-panel-text>
        </v-expansion-panel>
      </v-expansion-panels>
    </v-card>

    <!-- RQ-75: config.yaml changed on disk mid-edit — human arbitrates -->
    <ConfigConflictDialog
      v-model="conflictOpen"
      :fields="conflictFields"
      :saving="savingGlobal || savingHPC"
      @use-disk="conflictUseDisk"
      @use-mine="conflictUseMine"
    />

    <!-- RQ-74: runq self-logs — deaths the UI can't push still land in
         daemon.log; read it here instead of grepping files. -->
    <DaemonLogPanel />
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { onBeforeRouteLeave } from 'vue-router'
import { useTheme } from 'vuetify'
import { useConfigStore } from '@/stores/config'
import { useSettingsStore } from '@/stores/settings'
import { useSnackbar } from '@/composables/useSnackbar'
import { useAppearance, ACCENTS, type Density, type Surface } from '@/composables/useAppearance'
import { isGenerationConflict } from '@/apis/client'
import { configApi, type TargetConfig, type TargetGenerationView, type HPCCheckResult } from '@/apis/config'
import ShellTemplateEditor from '@/components/ShellTemplateEditor.vue'
import ConfigConflictDialog, { type ConflictField } from '@/components/ConfigConflictDialog.vue'
import DaemonLogPanel from '@/components/DaemonLogPanel.vue'

const { t, locale } = useI18n()
const theme = useTheme()
const config = useConfigStore()
const settings = useSettingsStore()
const snack = useSnackbar()

// ── Global config (default_target / data_path — v1: mode is gone) ──
const globalDefaultTarget = ref('')
const globalDataPath = ref('')
const savingGlobal = ref(false)
const globalDirty = computed(() =>
  config.loaded && (globalDefaultTarget.value !== config.defaultTarget || globalDataPath.value !== config.dataPath),
)

async function saveGlobal() {
  savingGlobal.value = true
  try {
    await configApi.putGlobal(globalDataPath.value, globalDefaultTarget.value, configGeneration.value)
    snack.success(t('settings.global_saved'))
    await config.fetchConfig()
    syncGlobal()
    await reloadTargets() // refresh config_generation after our own write
  } catch (e: any) {
    if (isGenerationConflict(e)) {
      await openConflict('global')
      return
    }
    snack.error(e?.message || t('common.error'))
  } finally {
    savingGlobal.value = false
  }
}

function syncGlobal() {
  globalDefaultTarget.value = config.defaultTarget
  globalDataPath.value = config.dataPath
}

// ── Target scheduler templates (v1: /hpc-config* is retired — templates
// live on the selected target; read-modify-write preserves unknown fields) ──
type HPCFieldKey = 'submit_template' | 'submit_id_regex' | 'status_template' | 'kill_template'

const targetItems = ref<TargetConfig[]>([])
const targetNames = computed(() => targetItems.value.map(x => x.name))
const selectedTarget = ref('')
/** config.yaml semantic hash the form was loaded from (RQ-75 If-Match). */
const configGeneration = ref('')

/** Populate the form from the selected target's stored config. */
function populateForm(name: string) {
  const item = targetItems.value.find(x => x.name === name)
  hpcResults.value = []
  hpcForm.value = {
    submit_template: (item?.submit_template as string) || '',
    submit_id_regex: (item?.submit_id_regex as string) || '',
    status_template: (item?.status_template as string) || '',
    kill_template: (item?.kill_template as string) || '',
  }
  hpcParser.value = [...((item?.status_parser as string[]) || [])]
}
watch(selectedTarget, populateForm)
const hpcFields: { key: HPCFieldKey; labelKey: string; hintKey: string; placeholder: string }[] = [
  { key: 'submit_template', labelKey: 'settings.hpc_f_submit', hintKey: 'settings.hpc_f_submit_hint', placeholder: 'sbatch --gpus={{gpus}} {{run_sh}}' },
  { key: 'submit_id_regex', labelKey: 'settings.hpc_f_regex', hintKey: 'settings.hpc_regex_hint', placeholder: 'Submitted batch job ([0-9]+)' },
  { key: 'status_template', labelKey: 'settings.hpc_f_status', hintKey: 'settings.hpc_f_status_hint', placeholder: 'sacct -n -X -j {{ext_id}} -o State' },
  { key: 'kill_template', labelKey: 'settings.hpc_f_kill', hintKey: 'settings.hpc_f_kill_hint', placeholder: 'scancel {{ext_id}}' },
]
const hpcForm = ref<Record<HPCFieldKey, string>>({
  submit_template: '', submit_id_regex: '', status_template: '', kill_template: '',
})
const hpcParser = ref<string[]>([])
const hpcPlaceholders = ref<Record<string, string[]>>({})
const hpcResults = ref<HPCCheckResult[]>([])
const savingHPC = ref(false)

function placeholdersFor(key: string): string[] {
  const base = hpcPlaceholders.value[key] ?? []
  return key === 'submit_template' ? [...base, 'param.*'] : base
}

// ── Shell editor dialog (one instance, retargeted per field/stage) ──
const editorOpen = ref(false)
const editorTitle = ref('')
const editorValue = ref('')
const editorPlaceholders = ref<string[]>([])
const editorHint = ref('')
let editorTarget: { kind: 'field'; key: HPCFieldKey } | { kind: 'stage'; index: number } | null = null

function openEditor(key: HPCFieldKey) {
  editorTarget = { kind: 'field', key }
  editorTitle.value = key
  editorValue.value = hpcForm.value[key]
  editorPlaceholders.value = placeholdersFor(key)
  editorHint.value = hpcFields.find(x => x.key === key)?.placeholder ?? ''
  editorOpen.value = true
}

function openStageEditor(index: number) {
  editorTarget = { kind: 'stage', index }
  editorTitle.value = `status_parser[${index}]`
  editorValue.value = hpcParser.value[index] ?? ''
  editorPlaceholders.value = placeholdersFor('status_parser')
  editorHint.value = ''
  editorOpen.value = true
}

function onEditorApply(value: string) {
  if (!editorTarget) return
  if (editorTarget.kind === 'field') hpcForm.value[editorTarget.key] = value
  else hpcParser.value[editorTarget.index] = value
  editorTarget = null
}

// ── Presets (same source as `runq target config add --preset`) ──
const presetNames = ref<string[]>([])
const presetMap = ref<Record<string, TargetConfig>>({})

function loadPreset(name: string) {
  const p = presetMap.value[name]
  if (!p) return
  hpcForm.value = {
    submit_template: (p.submit_template as string) || '',
    submit_id_regex: (p.submit_id_regex as string) || '',
    status_template: (p.status_template as string) || '',
    kill_template: (p.kill_template as string) || '',
  }
  hpcParser.value = [...((p.status_parser as string[]) || [])]
  hpcResults.value = []
  snack.info(t('settings.hpc_preset_loaded', { name }))
}

/** Merge form fields over the stored TargetConfig — unknown fields survive. */
function collectTarget(): TargetConfig {
  const base = targetItems.value.find(x => x.name === selectedTarget.value) ?? { name: selectedTarget.value }
  return {
    ...base,
    name: selectedTarget.value,
    submit_template: hpcForm.value.submit_template,
    submit_id_regex: hpcForm.value.submit_id_regex,
    status_template: hpcForm.value.status_template || undefined,
    status_parser: hpcParser.value.filter(s => s.trim()),
    kill_template: hpcForm.value.kill_template,
  }
}

async function checkHPC() {
  if (!selectedTarget.value) return
  try {
    const res = await configApi.checkTarget(selectedTarget.value, collectTarget())
    hpcResults.value = res.results
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  }
}

async function saveHPC() {
  if (!selectedTarget.value) return
  savingHPC.value = true
  try {
    await configApi.putTarget(selectedTarget.value, collectTarget(), configGeneration.value)
    await reloadTargets()
    populateForm(selectedTarget.value)
    await checkHPC()
    snack.success(t('settings.hpc_saved'))
  } catch (e: any) {
    if (isGenerationConflict(e)) {
      await openConflict('hpc')
      return
    }
    snack.error(e?.message || t('common.error'))
  } finally {
    savingHPC.value = false
  }
}

async function reloadTargets() {
  const res = await configApi.listTargets()
  targetItems.value = res.items ?? []
  hpcPlaceholders.value = res.placeholders ?? {}
  configGeneration.value = res.config_generation ?? ''
  targetGenerations.value = res.generations ?? []
}

// ── RQ-75: retired/retiring lane generations ──
const targetGenerations = ref<TargetGenerationView[]>([])
/** Same-name retiring generations: sub-rows of the SELECTED active target. */
const retiringOfSelected = computed(() =>
  targetGenerations.value.filter(
    g => !g.done_at && g.target === selectedTarget.value && targetNames.value.includes(g.target),
  ),
)
/** Generations of REMOVED targets: the collapsed archived section. */
const archivedGenerations = computed(() =>
  targetGenerations.value.filter(g => !targetNames.value.includes(g.target)),
)
function genLabel(g: TargetGenerationView): string {
  return `${g.target}-${g.generation.slice(0, 8)}`
}
function genDate(unix: number): string {
  return new Date(unix * 1000).toLocaleString()
}

// ── Dirty state (RQ-75): the form diverges from what was loaded ──
const HPC_KEYS: HPCFieldKey[] = ['submit_template', 'submit_id_regex', 'status_template', 'kill_template']
const hpcDirty = computed(() => {
  if (!selectedTarget.value) return false
  const item = targetItems.value.find(x => x.name === selectedTarget.value)
  const fieldChanged = HPC_KEYS.some(k => hpcForm.value[k] !== (((item?.[k] as string) ?? '') || ''))
  const parserChanged =
    JSON.stringify(hpcParser.value) !== JSON.stringify((item?.status_parser as string[]) ?? [])
  return fieldChanged || parserChanged
})
const anyDirty = computed(() => globalDirty.value || hpcDirty.value)

// ── Generation conflict resolution (RQ-75): human arbitrates ──
const conflictOpen = ref(false)
const conflictFields = ref<ConflictField[]>([])
let conflictKind: 'global' | 'hpc' = 'hpc'

/**
 * Build the conflict dialog from FRESH disk state. targetItems /
 * configGeneration are refreshed here (safe: the form fields are separate
 * refs), so "keep mine" retries against the current generation and merges
 * unknown fields over the disk version — the other writer's untouched
 * edits survive.
 */
async function openConflict(kind: 'global' | 'hpc') {
  conflictKind = kind
  try {
    if (kind === 'global') await config.fetchConfig()
    await reloadTargets()
  } catch {
    snack.error(t('common.error'))
    return
  }
  if (kind === 'global') {
    conflictFields.value = (
      [
        { key: 'default_target', disk: config.defaultTarget, mine: globalDefaultTarget.value },
        { key: 'data_path', disk: config.dataPath, mine: globalDataPath.value },
      ] as ConflictField[]
    ).filter(f => f.disk !== f.mine)
  } else {
    const item = targetItems.value.find(x => x.name === selectedTarget.value)
    const rows: ConflictField[] = HPC_KEYS.map(k => ({
      key: k,
      disk: ((item?.[k] as string) ?? '') || '',
      mine: hpcForm.value[k],
    }))
    rows.push({
      key: 'status_parser',
      disk: ((item?.status_parser as string[]) ?? []).join('\n'),
      mine: hpcParser.value.filter(s => s.trim()).join('\n'),
    })
    conflictFields.value = rows.filter(f => f.disk !== f.mine)
  }
  conflictOpen.value = true
}

/** Adopt the disk version: drop my edits, reload the form. */
function conflictUseDisk() {
  conflictOpen.value = false
  if (conflictKind === 'global') syncGlobal()
  else populateForm(selectedTarget.value)
}

/** Keep my edits: retry the save against the fresh generation. */
async function conflictUseMine() {
  conflictOpen.value = false
  if (conflictKind === 'global') await saveGlobal()
  else await saveHPC()
}

// ── Disk watch on window focus (RQ-75): clean form follows the file;
// a dirty form is only WARNED — never clobbered. ──
async function onWindowFocus() {
  try {
    const res = await configApi.listTargets({ silent: true })
    const gen = res.config_generation ?? ''
    if (!gen || gen === configGeneration.value) return
    if (anyDirty.value) {
      snack.info(t('settings.config_changed_on_disk'))
      return
    }
    targetItems.value = res.items ?? []
    hpcPlaceholders.value = res.placeholders ?? {}
    configGeneration.value = gen
    await config.fetchConfig()
    syncGlobal()
    populateForm(selectedTarget.value)
  } catch {
    /* unreachable — the health banner owns connectivity reporting */
  }
}

// ── Unsaved-changes guards ──
function onBeforeUnload(e: BeforeUnloadEvent) {
  if (!anyDirty.value) return
  e.preventDefault()
  e.returnValue = '' // legacy engines require a set returnValue
}

onBeforeRouteLeave(() => {
  if (!anyDirty.value) return true
  return window.confirm(t('settings.unsaved_leave_confirm'))
})

const webhookUrl = ref('')
const webhookEvents = ref<string[]>([])
const saving = ref(false)
const testing = ref(false)

const currentTheme = ref(settings.theme)
const currentLocale = ref(settings.locale)

const availableEvents = computed(() => [
  { value: 'job_done', label: t('settings.event_job_done'), icon: 'mdi-check-circle' },
  { value: 'task_failed', label: t('settings.event_task_failed'), icon: 'mdi-alert-circle' },
  { value: 'task_killed', label: t('settings.event_task_killed'), icon: 'mdi-stop-circle' },
])

const themes = computed(() => [
  { value: 'light', label: t('settings.theme_light'), icon: 'mdi-white-balance-sunny' },
  { value: 'dark', label: t('settings.theme_dark'), icon: 'mdi-moon-waning-crescent' },
])

const appearance = useAppearance()

const densities = computed((): Array<{ value: Density; label: string; icon: string }> => [
  { value: 'compact', label: t('settings.density_compact'), icon: 'mdi-view-headline' },
  { value: 'regular', label: t('settings.density_regular'), icon: 'mdi-view-day-outline' },
  { value: 'comfy', label: t('settings.density_comfy'), icon: 'mdi-view-agenda-outline' },
])

const surfaces = computed((): Array<{ value: Surface; label: string; icon: string }> => [
  { value: 'hairline', label: t('settings.surface_hairline'), icon: 'mdi-square-outline' },
  { value: 'elevated', label: t('settings.surface_elevated'), icon: 'mdi-checkbox-multiple-blank-outline' },
  { value: 'grid', label: t('settings.surface_grid'), icon: 'mdi-grid' },
])

const locales = [
  { value: 'en', label: 'English', flag: 'EN' },
  { value: 'ja', label: '日本語', flag: 'JA' },
  { value: 'zh-CN', label: '中文', flag: 'ZH' },
]

function toggleEvent(evt: string) {
  const i = webhookEvents.value.indexOf(evt)
  if (i >= 0) webhookEvents.value.splice(i, 1)
  else webhookEvents.value.push(evt)
}

watch(currentTheme, (val) => {
  settings.setTheme(val)
  theme.global.name.value = val
})

watch(currentLocale, (val) => {
  settings.setLocale(val)
  locale.value = val
})

async function saveWebhook() {
  saving.value = true
  try {
    await settings.saveWebhook(webhookUrl.value, webhookEvents.value)
    snack.success(t('settings.webhook_saved'))
  } finally {
    saving.value = false
  }
}

async function testWebhook() {
  testing.value = true
  try {
    await settings.testWebhook()
    snack.success(t('settings.webhook_test_ok'))
  } catch (e: any) {
    snack.error(e?.message || t('common.error'))
  } finally {
    testing.value = false
  }
}

function copyToClipboard(text: string) {
  navigator.clipboard.writeText(text)
  snack.info(t('common.copied'))
}

onMounted(async () => {
  window.addEventListener('focus', onWindowFocus)
  window.addEventListener('beforeunload', onBeforeUnload)

  // Every load is independently fault-tolerant: one failing endpoint
  // (daemon down, version mismatch) must not take the whole page down.
  try {
    await settings.loadWebhook()
    webhookUrl.value = settings.webhook.url
    webhookEvents.value = [...settings.webhook.events]
  } catch { /* webhook config unavailable */ }

  try {
    if (!config.loaded) await config.fetchConfig()
  } catch { /* backend unreachable — show defaults */ }
  syncGlobal()

  try {
    const presets = await configApi.targetPresets()
    presetNames.value = presets.names ?? []
    presetMap.value = presets.presets ?? {}
  } catch { /* presets unavailable */ }

  try {
    await reloadTargets()
    // Preselect the default target (watch populates the form).
    if (!selectedTarget.value) {
      selectedTarget.value = targetNames.value.includes(config.defaultTarget)
        ? config.defaultTarget
        : (targetNames.value[0] ?? '')
    }
  } catch { /* endpoint unavailable — leave schema-rendered empty fields */ }
})

onUnmounted(() => {
  window.removeEventListener('focus', onWindowFocus)
  window.removeEventListener('beforeunload', onBeforeUnload)
})
</script>

<style scoped>
.font-mono, .font-mono :deep(input), .font-mono :deep(textarea) { font-family: monospace; font-size: 13px; }
.tmpl-display {
  border: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface-variant), 0.2);
  transition: border-color 0.15s ease, background 0.15s ease;
  min-height: 38px;
}
.tmpl-display:hover {
  border-color: rgb(var(--v-theme-primary));
  background: rgb(var(--v-theme-surface-variant), 0.35);
}
.accent-swatch {
  display: inline-block;
  width: 20px;
  height: 20px;
  border-radius: 50%;
}
</style>
