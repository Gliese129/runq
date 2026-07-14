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
  const CAPS = { gpu_map: false, pause_resume: false, live_log: true, retry: true, state_model: 'poll', kill_async: true, submit_preview: true, activity_heatmap: false, log_search: false }
  const respond = (path: string) => {
    // v1 contract (spec-first): targets replace /hpc-config*
    if (path.includes('/targets/presets')) return { names: ['slurm'], presets: { slurm: { name: 'slurm', submit_template: 's {{run_sh}}', submit_id_regex: 'j ([0-9]+)', kill_template: 'k {{ext_id}}' } } }
    if (path.includes('/targets')) return { items: [{ name: 'hpc-a', scheduler: 'slurm', submit_template: '', submit_id_regex: '', kill_template: '' }], placeholders: { submit_template: ['run_sh'] }, path: '/x' }
    if (path.includes('/config')) return { data_path: '', config_path: '/x', default_target: 'hpc-a', targets: [{ name: 'hpc-a', type: 'remote', scheduler: 'slurm', capabilities: CAPS }] }
    if (path.includes('/webhook')) return { url: '', events: [] }
    return {}
  }
  return {
    api: {
      get: vi.fn(async (p: string) => respond(p)),
      post: vi.fn(async (p: string) => respond(p)),
      put: vi.fn(async (p: string) => respond(p)),
      del: vi.fn(async (p: string) => respond(p)),
      getList: vi.fn(async () => []),
      getEnvelope: vi.fn(async () => ({ items: [] })),
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
