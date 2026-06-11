// @vitest-environment happy-dom
import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createVuetify } from 'vuetify'
import * as vComponents from 'vuetify/components'
import * as vDirectives from 'vuetify/directives'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import en from '@/i18n/en.json'

// Mount smoke guard: "the page is blank" is the worst failure mode.
// The api client is mocked at module level (axios never runs).
vi.mock('@/apis/client', () => {
  const respond = (path: string) => {
    if (path.includes('/hpc-config/presets')) return { names: ['slurm'], presets: { slurm: { submit_template: 's {{run_sh}}', submit_id_regex: 'j ([0-9]+)', kill_template: 'k {{ext_id}}' } } }
    if (path.includes('/hpc-config')) return { exists: false, config: {}, placeholders: { submit_template: ['run_sh'] }, path: '/x' }
    if (path.includes('/config')) return { mode: 'hpc', data_path: '', config_path: '/x', capabilities: { gpu_map: false, pause_resume: false, live_log: true, retry: true, state_model: 'poll', kill_async: true } }
    if (path.includes('/webhook')) return { url: '', events: [] }
    return {}
  }
  return {
    api: {
      get: vi.fn(async (p: string) => respond(p)),
      post: vi.fn(async (p: string) => respond(p)),
      put: vi.fn(async (p: string) => respond(p)),
      delete: vi.fn(async (p: string) => respond(p)),
    },
  }
})

vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} })
vi.stubGlobal('visualViewport', undefined)

import Settings from './Settings.vue'

describe('Settings smoke', () => {
  it('mounts and renders without throwing', async () => {
    const wrapper = mount(Settings, {
      global: {
        plugins: [
          createVuetify({ components: vComponents, directives: vDirectives }),
          createPinia(),
          createI18n({ legacy: false, locale: 'en', messages: { en } as any }),
        ],
      },
    })
    await new Promise(r => setTimeout(r, 50))
    expect(wrapper.html().toLowerCase()).toContain('settings')
  })

  it('survives every endpoint failing (version mismatch / daemon down)', async () => {
    const { api } = await import('@/apis/client')
    ;(api.get as any).mockRejectedValue(new Error('boom'))
    const wrapper = mount(Settings, {
      global: {
        plugins: [
          createVuetify({ components: vComponents, directives: vDirectives }),
          createPinia(),
          createI18n({ legacy: false, locale: 'en', messages: { en } as any }),
        ],
      },
    })
    await new Promise(r => setTimeout(r, 50))
    expect(wrapper.html().length).toBeGreaterThan(100)
  })
})
