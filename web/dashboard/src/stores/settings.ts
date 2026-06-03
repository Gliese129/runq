import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '@/apis/client'
import type { WebhookConfig } from '@/types/api'

export const useSettingsStore = defineStore('settings', () => {
  const webhook = ref<WebhookConfig>({ url: '', events: [] })
  const theme = ref(localStorage.getItem('runq-theme') || 'light')
  const locale = ref(localStorage.getItem('runq-locale') || 'en')
  const animeMode = ref(localStorage.getItem('runq-anime') === 'true')

  async function loadWebhook() {
    try {
      webhook.value = await api.get<WebhookConfig>('/config/webhook')
    } catch {
      // endpoint may not exist yet — keep defaults
    }
  }

  async function saveWebhook(url: string, events: string[]) {
    await api.post('/config/webhook', { url, events })
    webhook.value = { url, events }
  }

  async function testWebhook() {
    await api.post('/config/webhook/test')
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
