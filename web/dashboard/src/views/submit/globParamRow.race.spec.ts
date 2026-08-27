// @vitest-environment happy-dom
// Regression: overlapping glob scans resolve last-writer-wins by SCAN
// ORDER, not arrival order (Codex RQ2-3 r2). A reload/draft restore mounts
// a scan while the target is still settling; the stale response landing
// late must not clobber the newer target's result.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { createVuetify } from 'vuetify'
import * as vComponents from 'vuetify/components'
import * as vDirectives from 'vuetify/directives'
import { createI18n } from 'vue-i18n'
import { reactive, nextTick } from 'vue'
import en from '@/i18n/en.json'

// One deferred response per glob call, in call order.
type Deferred = {
  resolve: (v: { items: { name: string; path: string; is_dir: boolean; size: number }[]; truncated: boolean }) => void
  reject: (e: Error) => void
  target?: string
}
const pending: Deferred[] = []

vi.mock('@/apis/files', () => ({
  filesApi: {
    glob: vi.fn((_root: string, _pattern: string, opts?: { target?: string }) =>
      new Promise((resolve, reject) => {
        pending.push({ resolve, reject, target: opts?.target })
      })),
  },
}))
vi.mock('@/apis/ui', () => ({
  uiApi: { get: vi.fn(async () => ({})), put: vi.fn(async () => ({})) },
}))
vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} })

import GlobParamRow from './GlobParamRow.vue'
import { SUBMIT_STATE_KEY } from '@/types/submit'
import type { ParamRow } from './paramTable'

const entry = (p: string) => ({ name: p, path: p, is_dir: false, size: 1 })

function mountRow() {
  const row = reactive<ParamRow>({
    name: 'ckpt', type: 'file', default: '', values: [], glob: 'ckpt-*.pt',
  })
  const state = reactive({
    projectName: 'demo',
    target: 'local',
    newProject: { workDir: '/w' },
  })
  const wrapper = mount(GlobParamRow, {
    props: { row },
    global: {
      plugins: [
        createVuetify({ components: vComponents, directives: vDirectives }),
        createI18n({ legacy: false, locale: 'en', messages: { en }, missingWarn: false, fallbackWarn: false }),
      ],
      provide: { [SUBMIT_STATE_KEY as symbol]: state },
    },
  })
  return { wrapper, row, state }
}

describe('GlobParamRow scan race', () => {
  beforeEach(() => { pending.length = 0 })

  it('a stale response landing late cannot clobber the newer scan', async () => {
    const { row, state } = mountRow()
    await nextTick()
    expect(pending).toHaveLength(1) // mount scan against 'local'

    // Target settles to its real value → second scan fires.
    state.target = 'local-review'
    await nextTick()
    await nextTick()
    expect(pending).toHaveLength(2)
    expect(pending[1].target).toBe('local-review')

    // Newer scan answers FIRST...
    pending[1].resolve({ items: [entry('review-a.pt'), entry('review-b.pt')], truncated: false })
    await vi.waitFor(() => expect(row.globState).toBe('ok'))
    expect([...row.values]).toEqual(['review-a.pt', 'review-b.pt'])

    // ...then the STALE mount scan limps in with the wrong machine's view.
    pending[0].resolve({ items: [entry('stale.pt')], truncated: false })
    await new Promise(r => setTimeout(r, 20))
    expect([...row.values]).toEqual(['review-a.pt', 'review-b.pt'])
    expect(row.globState).toBe('ok')
  })

  it('a stale FAILURE cannot flip a healthy newer scan into error', async () => {
    const { row, state } = mountRow()
    await nextTick()
    state.target = 'local-review'
    await nextTick()
    await nextTick()
    expect(pending).toHaveLength(2)

    pending[1].resolve({ items: [entry('good.pt')], truncated: false })
    await vi.waitFor(() => expect(row.globState).toBe('ok'))

    pending[0].reject(new Error('stale target unreachable'))
    await new Promise(r => setTimeout(r, 20))
    // The error path is exactly what blocked submits until a manual
    // rescan before the guard — it must be a no-op now.
    expect(row.globState).toBe('ok')
    expect([...row.values]).toEqual(['good.pt'])
  })
})
