import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { WebhookConfig } from '@/types/api'

const WEBHOOK_STORAGE_KEY = 'runq-webhook'

function readStoredWebhook(): WebhookConfig {
  try {
    const raw = localStorage.getItem(WEBHOOK_STORAGE_KEY)
    if (!raw) return { url: '', events: [] }
    const parsed = JSON.parse(raw)
    return {
      url: typeof parsed.url === 'string' ? parsed.url : '',
      events: Array.isArray(parsed.events) ? parsed.events.filter((e: unknown) => typeof e === 'string') : [],
    }
  } catch {
    return { url: '', events: [] }
  }
}

async function testWebhook() {
  throw new Error('Webhook test is not supported by the dashboard backend yet')
}

export const useSettingsStore = defineStore('settings', () => {
  const webhook = ref<WebhookConfig>(readStoredWebhook())
  const theme = ref(localStorage.getItem('runq-theme') || 'light')
  const locale = ref(localStorage.getItem('runq-locale') || 'en')
  const animeMode = ref(localStorage.getItem('runq-anime') === 'true')

  async function loadWebhook() {
    webhook.value = readStoredWebhook()
  }

  async function saveWebhook(url: string, events: string[]) {
    webhook.value = { url, events }
    localStorage.setItem(WEBHOOK_STORAGE_KEY, JSON.stringify(webhook.value))
  }

  function setTheme(t: string) {
    theme.value = t
    localStorage.setItem('runq-theme', t)
  }

  function setLocale(l: string) {
    locale.value = l
    localStorage.setItem('runq-locale', l)
  }

  function setAnimeMode(v: boolean) {
    animeMode.value = v
    localStorage.setItem('runq-anime', String(v))
  }

  return { webhook, theme, locale, animeMode, loadWebhook, saveWebhook, testWebhook, setTheme, setLocale, setAnimeMode }
})
