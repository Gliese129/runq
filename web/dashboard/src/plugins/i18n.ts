import { createI18n } from 'vue-i18n'
import en from '../i18n/en.json'
import ja from '../i18n/ja.json'
import zhCN from '../i18n/zh-CN.json'

function detectLocale(): string {
  const saved = localStorage.getItem('runq-locale')
  if (saved) return saved
  const nav = navigator.language.toLowerCase()
  if (nav.startsWith('ja')) return 'ja'
  if (nav.startsWith('zh')) return 'zh-CN'
  return 'en'
}

export default createI18n({
  legacy: false,
  locale: detectLocale(),
  fallbackLocale: 'en',
  messages: { en, ja, 'zh-CN': zhCN },
})
