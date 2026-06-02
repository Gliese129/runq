import { createRouter, createWebHashHistory, type RouteRecordRaw } from 'vue-router'

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: () => import('../layouts/DefaultLayout.vue'),
    children: [
      { path: '', name: 'overview', component: () => import('../views/Overview.vue') },
      { path: 'projects/:project', name: 'project', component: () => import('../views/ProjectJobs.vue'), props: true },
      { path: 'submit', name: 'submit', component: () => import('../views/Submit.vue') },
      { path: 'settings', name: 'settings', component: () => import('../views/Settings.vue') },
      { path: 'about', name: 'about', component: () => import('../views/About.vue') },
    ],
  },
  {
    path: '/projects/:project/:jobId',
    component: () => import('../layouts/DetailLayout.vue'),
    children: [
      { path: '', name: 'job-detail', component: () => import('../views/JobDetail.vue'), props: true },
    ],
  },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
