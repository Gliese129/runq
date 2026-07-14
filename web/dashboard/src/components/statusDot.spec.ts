// @vitest-environment happy-dom
import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import StatusDot from './StatusDot.vue'
import { MDI_PATHS } from '@/plugins/mdiPaths'
import en from '@/i18n/en.json'
import zhCN from '@/i18n/zh-CN.json'

const globalEn = { plugins: [createI18n({ legacy: false, locale: 'en', messages: { en } })] }

describe('StatusDot', () => {
  it('renders DISTINCT icon glyphs for the amber-shared statuses at >= 10px', () => {
    // killed / paused / partial share the warning color by design — the
    // shape channel is the only thing telling them apart at a glance.
    const paths = (['killed', 'paused', 'partial'] as const).map((status) => {
      const w = mount(StatusDot, { props: { status, kind: 'job' as const, size: 14 }, global: globalEn })
      const path = w.find('svg path')
      expect(path.exists(), `${status} should render a glyph`).toBe(true)
      return path.attributes('d')
    })
    expect(new Set(paths).size).toBe(3)
    expect(paths[0]).toBe(MDI_PATHS['mdi-stop'])
    expect(paths[1]).toBe(MDI_PATHS['mdi-pause'])
  })

  it('keeps the plain dot below 10px (icons are illegible that small)', () => {
    const w = mount(StatusDot, { props: { status: 'killed', kind: 'job' as const, size: 6 }, global: globalEn })
    expect(w.find('svg').exists()).toBe(false)
    expect(w.find('.status-dot').exists()).toBe(true)
  })

  it('uses the localized status name as accessible name, not the raw enum', () => {
    const globalZh = {
      plugins: [createI18n({ legacy: false, locale: 'zh-CN', messages: { 'zh-CN': zhCN } })],
    }
    const w = mount(StatusDot, { props: { status: 'killed', kind: 'job' as const, size: 14 }, global: globalZh })
    expect(w.find('svg').attributes('aria-label')).toBe('已终止')
  })
})
