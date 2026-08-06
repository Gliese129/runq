// @vitest-environment happy-dom
import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createVuetify } from 'vuetify'
import * as vComponents from 'vuetify/components'
import * as vDirectives from 'vuetify/directives'
import { createI18n } from 'vue-i18n'
import en from '@/i18n/en.json'
import ConfigConflictDialog from './ConfigConflictDialog.vue'

// Vuetify's overlay location strategy touches these browser globals that
// happy-dom doesn't ship.
vi.stubGlobal(
  'ResizeObserver',
  class {
    observe() {}
    unobserve() {}
    disconnect() {}
  },
)
vi.stubGlobal('visualViewport', undefined)

// RQ-75: the conflict dialog is the human-arbitration surface for CAS
// write conflicts — it must show BOTH versions verbatim and emit exactly
// what the user chose.

function mountDialog(fields: { key: string; disk: string; mine: string }[]) {
  return mount(ConfigConflictDialog, {
    props: { modelValue: true, fields },
    global: {
      plugins: [
        createVuetify({ components: vComponents, directives: vDirectives }),
        createI18n({ legacy: false, locale: 'en', messages: { en } as any }),
      ],
    },
  })
}

describe('ConfigConflictDialog', () => {
  it('renders disk and form values side by side, verbatim', async () => {
    const wrapper = mountDialog([
      { key: 'submit_template', disk: 'sbatch --old {{run_sh}}', mine: 'sbatch --mine {{run_sh}}' },
    ])
    await new Promise((r) => setTimeout(r, 20))
    // v-dialog teleports; assert against the document body.
    const html = document.body.innerHTML
    expect(html).toContain('submit_template')
    expect(html).toContain('sbatch --old {{run_sh}}')
    expect(html).toContain('sbatch --mine {{run_sh}}')
    wrapper.unmount()
  })

  it('empty diff shows the no-overlap explanation instead of a bare table', async () => {
    const wrapper = mountDialog([])
    await new Promise((r) => setTimeout(r, 20))
    expect(document.body.innerHTML).toContain((en as any)['settings.conflict_no_overlap'])
    wrapper.unmount()
  })

  it('emits the chosen resolution', async () => {
    const wrapper = mountDialog([{ key: 'data_path', disk: '/a', mine: '/b' }])
    await new Promise((r) => setTimeout(r, 20))
    const buttons = Array.from(document.body.querySelectorAll('button'))
    const useDisk = buttons.find((b) =>
      b.textContent?.includes((en as any)['settings.conflict_use_disk']),
    )
    const useMine = buttons.find((b) =>
      b.textContent?.includes((en as any)['settings.conflict_use_mine']),
    )
    expect(useDisk).toBeTruthy()
    expect(useMine).toBeTruthy()
    useDisk!.click()
    useMine!.click()
    await new Promise((r) => setTimeout(r, 10))
    expect(wrapper.emitted('use-disk')).toBeTruthy()
    expect(wrapper.emitted('use-mine')).toBeTruthy()
    wrapper.unmount()
  })
})
