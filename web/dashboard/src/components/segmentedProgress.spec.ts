// @vitest-environment happy-dom
import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import SegmentedProgress from './SegmentedProgress.vue'
import { statusStyle } from './statusGrammar'
import en from '@/i18n/en.json'
import type { TaskCountGroup } from '@/types/api'

const counts = (partial: Partial<TaskCountGroup>): TaskCountGroup => ({
  total: 0, pending: 0, running: 0, completed: 0, failed: 0, ...partial,
})

const i18n = createI18n({ legacy: false, locale: 'en', messages: { en } })
const global = { plugins: [i18n] }

describe('SegmentedProgress', () => {
  it('renders one segment per non-zero bucket, proportionally, colors from the grammar', () => {
    const wrapper = mount(SegmentedProgress, {
      props: { counts: counts({ total: 10, completed: 5, failed: 2, running: 2, pending: 1 }) },
      global,
    })
    const segs = wrapper.findAll('.seg-progress > div')
    expect(segs).toHaveLength(4)
    // Order: success, failed, running, pending (CI readout order).
    expect(segs[0].attributes('style')).toContain('width: 50%')
    expect(segs[0].attributes('style')).toContain(statusStyle('task', 'success').css)
    expect(segs[1].attributes('style')).toContain('width: 20%')
    expect(segs[1].attributes('style')).toContain(statusStyle('task', 'failed').css)
    expect(segs[3].attributes('style')).toContain('width: 10%')
    expect(segs[3].attributes('style')).toContain(statusStyle('task', 'pending').css)
  })

  it('skips zero buckets', () => {
    const wrapper = mount(SegmentedProgress, {
      props: { counts: counts({ total: 4, completed: 4 }) },
      global,
    })
    const segs = wrapper.findAll('.seg-progress > div')
    expect(segs).toHaveLength(1)
    expect(segs[0].attributes('style')).toContain('width: 100%')
  })

  it('renders an empty track for 0 tasks, with an accessible summary', () => {
    const wrapper = mount(SegmentedProgress, { props: { counts: counts({}) }, global })
    expect(wrapper.findAll('.seg-progress > div')).toHaveLength(0)
    const root = wrapper.find('.seg-progress')
    expect(root.attributes('role')).toBe('img')
    expect(root.attributes('aria-label')).toContain('0 pending of 0 tasks')
  })

  it('reads the red bucket honestly as "failed or killed" (backend folds killed)', () => {
    const wrapper = mount(SegmentedProgress, {
      props: { counts: counts({ total: 4, failed: 4 }) },
      global,
    })
    expect(wrapper.find('.seg-progress').attributes('aria-label')).toContain('4 failed or killed')
  })

  it('pluralizes the accessible summary ("1 task", not "1 tasks")', () => {
    const wrapper = mount(SegmentedProgress, {
      props: { counts: counts({ total: 1, completed: 1 }) },
      global,
    })
    const label = wrapper.find('.seg-progress').attributes('aria-label') ?? ''
    expect(label).toContain('of 1 task')
    expect(label).not.toContain('1 tasks')
  })
})
