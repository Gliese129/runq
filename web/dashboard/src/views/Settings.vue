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

    <!-- Per-target config (scheduler templates, SSH, generations) moved to
         the Target page (RQ2-4 ③) — different owner, different blast
         radius than dashboard preferences. -->
    <v-card class="mb-4 pa-5">
      <div class="d-flex align-center ga-3">
        <v-icon size="18" color="primary">mdi-server-outline</v-icon>
        <div class="flex-grow-1">
          <div class="text-subtitle-2">{{ t('settings.targets_moved_title') }}</div>
          <div class="text-caption text-on-surface-variant">{{ t('settings.targets_moved_hint') }}</div>
        </div>
        <v-btn size="small" variant="tonal" color="primary" :to="{ name: 'target' }">
          {{ t('nav.targets') }}
        </v-btn>
      </div>
    </v-card>

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
              <!-- Logo switch group: pick the face, not a bare toggle — the
                   choice IS which logo the app wears. -->
              <div class="d-flex ga-2">
                <v-card
                  class="pa-2 d-flex align-center justify-center cursor-pointer"
                  :color="!settings.animeMode ? 'primary' : 'surface-variant'"
                  :variant="!settings.animeMode ? 'tonal' : 'flat'"
                  role="button"
                  :aria-pressed="!settings.animeMode"
                  :aria-label="t('settings.anime_mode') + ': off'"
                  @click="settings.setAnimeMode(false)"
                >
                  <RunqLogo :size="32" variant="normal" />
                </v-card>
                <v-card
                  class="pa-2 d-flex align-center justify-center cursor-pointer"
                  :color="settings.animeMode ? 'primary' : 'surface-variant'"
                  :variant="settings.animeMode ? 'tonal' : 'flat'"
                  role="button"
                  :aria-pressed="settings.animeMode"
                  :aria-label="t('settings.anime_mode') + ': on'"
                  @click="settings.setAnimeMode(true)"
                >
                  <RunqLogo :size="32" variant="anime" />
                </v-card>
              </div>
            </div>
          </v-expansion-panel-text>
        </v-expansion-panel>
      </v-expansion-panels>
    </v-card>

    <!-- RQ-75: config.yaml changed on disk mid-edit — human arbitrates -->
    <ConfigConflictDialog
      v-model="conflictOpen"
      :fields="conflictFields"
      :saving="savingGlobal"
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
import { configApi } from '@/apis/config'
import ConfigConflictDialog, { type ConflictField } from '@/components/ConfigConflictDialog.vue'
import DaemonLogPanel from '@/components/DaemonLogPanel.vue'
import RunqLogo from '@/components/RunqLogo.vue'

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

// ── config.yaml generation (RQ-75 If-Match): still needed here — the
// GLOBAL card writes the same file the Target page does. Per-target
// forms and their conflict/merge flow live on the Target page (RQ2-4 ③). ──
/** config.yaml semantic hash the form was loaded from (RQ-75 If-Match). */
const configGeneration = ref('')

async function reloadTargets() {
  const res = await configApi.listTargets()
  configGeneration.value = res.config_generation ?? ''
}

// ── Dirty state (RQ-75): the form diverges from what was loaded ──
const anyDirty = computed(() => globalDirty.value)

// ── Generation conflict resolution (RQ-75): human arbitrates ──
const conflictOpen = ref(false)
const conflictFields = ref<ConflictField[]>([])

/** Build the conflict dialog from FRESH disk state — "keep mine" retries
 *  against the current generation. */
async function openConflict(_kind: 'global') {
  try {
    await config.fetchConfig()
    await reloadTargets()
  } catch {
    snack.error(t('common.error'))
    return
  }
  conflictFields.value = (
    [
      { key: 'default_target', disk: config.defaultTarget, mine: globalDefaultTarget.value },
      { key: 'data_path', disk: config.dataPath, mine: globalDataPath.value },
    ] as ConflictField[]
  ).filter(f => f.disk !== f.mine)
  conflictOpen.value = true
}

/** Adopt the disk version: drop my edits, reload the form. */
function conflictUseDisk() {
  conflictOpen.value = false
  syncGlobal()
}

/** Keep my edits: retry the save against the fresh generation. */
async function conflictUseMine() {
  conflictOpen.value = false
  await saveGlobal()
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
    configGeneration.value = gen
    await config.fetchConfig()
    syncGlobal()
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
    await reloadTargets() // config_generation for the global card's If-Match
  } catch { /* endpoint unavailable */ }
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
