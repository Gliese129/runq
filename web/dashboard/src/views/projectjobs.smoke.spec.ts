// @vitest-environment happy-dom
import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createVuetify } from 'vuetify'
import * as vComponents from 'vuetify/components'
import * as vDirectives from 'vuetify/directives'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { createRouter, createMemoryHistory } from 'vue-router'
import en from '@/i18n/en.json'

function respondList(path: string) {
  if (path.includes('/projects')) return [{ name: 'p1', work_dir: '/x', job_count: 0, archived: true }]
  return []
}

// Mount smoke guard. This page once threw a ReferenceError at setup (an
// immediate watcher touching a ref still in its temporal dead zone) —
// exactly the class of bug vue-tsc cannot see and a mount catches.
vi.mock('@/apis/client', () => {
  // v1: collection endpoints go through api.getList (envelope unwrapped)
  return {
    api: {
      get: vi.fn(async () => ({})),
      post: vi.fn(async () => ({})),
      put: vi.fn(async () => ({})),
      del: vi.fn(async () => ({})),
      getList: vi.fn(async (p: string) => respondList(p)),
      getEnvelope: vi.fn(async (p: string) => ({ items: respondList(p) })),
    },
  }
})

vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} })
vi.stubGlobal('visualViewport', undefined)

import { VueQueryPlugin, QueryClient } from '@tanstack/vue-query'
import ProjectJobs from './ProjectJobs.vue'

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', name: 'overview', component: { template: '<div/>' } },
      { path: '/submit', name: 'submit', component: { template: '<div/>' } },
      { path: '/projects/:project', name: 'project', component: { template: '<div/>' }, props: true },
      { path: '/projects/:project/:jobId', name: 'job-detail', component: { template: '<div/>' }, props: true },
    ],
  })
}

describe('ProjectJobs smoke', () => {
  it('mounts (incl. the immediate project watcher) without throwing', async () => {
    const wrapper = mount(ProjectJobs, {
      props: { project: 'p1' },
      global: {
        plugins: [
          createVuetify({ components: vComponents, directives: vDirectives }),
          createPinia(),
          createI18n({ legacy: false, locale: 'en', messages: { en } as any }),
          makeRouter(),
          [VueQueryPlugin, { queryClient: makeQueryClient() }],
        ],
      },
    })
    await new Promise(r => setTimeout(r, 50))
    expect(wrapper.html()).toContain('p1')
  })
})
