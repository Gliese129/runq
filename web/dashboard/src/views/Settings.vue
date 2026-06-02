<template>
  <div style="max-width: 640px; margin: 0 auto">
    <div class="text-h5 font-weight-bold mb-6">{{ t('settings.title') }}</div>

    <!-- System info -->
    <v-card class="mb-4 pa-5">
      <div class="text-subtitle-2 mb-3">{{ t('settings.system') }}</div>
      <div class="d-flex flex-column ga-2">
        <div class="d-flex align-center justify-space-between">
          <span class="text-on-surface-variant">{{ t('settings.mode') }}</span>
          <v-chip size="small" variant="tonal" color="primary">
            {{ config.mode === 'daemon' ? t('settings.mode_local') : t('settings.mode_hpc') }}
          </v-chip>
        </div>
        <div class="d-flex align-center justify-space-between">
          <span class="text-on-surface-variant">{{ t('settings.data_path') }}</span>
          <code class="text-body-2 cursor-pointer" @click="copyToClipboard(config.dataPath)">
            {{ config.dataPath }}
            <v-icon size="12" class="ml-1" color="on-surface-variant">mdi-content-copy</v-icon>
          </code>
        </div>
        <div class="d-flex align-center justify-space-between">
          <span class="text-on-surface-variant">{{ t('settings.config_path') }}</span>
          <code class="text-body-2 cursor-pointer" @click="copyToClipboard(config.configPath)">
            {{ config.configPath }}
            <v-icon size="12" class="ml-1" color="on-surface-variant">mdi-content-copy</v-icon>
          </code>
        </div>
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

      <!-- Anime mode -->
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
    </v-card>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useTheme } from 'vuetify'
import { useConfigStore } from '@/stores/config'
import { useSettingsStore } from '@/stores/settings'
import { useSnackbar } from '@/composables/useSnackbar'

const { t, locale } = useI18n()
const theme = useTheme()
const config = useConfigStore()
const settings = useSettingsStore()
const snack = useSnackbar()

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
  } finally {
    testing.value = false
  }
}

function copyToClipboard(text: string) {
  navigator.clipboard.writeText(text)
  snack.info(t('common.copied'))
}

onMounted(async () => {
  await settings.loadWebhook()
  webhookUrl.value = settings.webhook.url
  webhookEvents.value = [...settings.webhook.events]
})
</script>
