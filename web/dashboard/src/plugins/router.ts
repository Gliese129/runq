import { createRouter, createWebHashHistory, type RouteRecordRaw } from 'vue-router'

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: () => import('../layouts/DefaultLayout.vue'),
    children: [
      { path: '', name: 'overview', component: () => import('../views/Overview.vue') },
      { path: 'projects/:project', name: 'project', component: () => import('../views/ProjectJobs.vue'), props: true },
      { path: 'projects/:project/:jobId', name: 'job-detail', component: () => import('../views/job/index.vue'), props: true },
      { path: 'projects/:project/:jobId/:taskId', name: 'task-detail', component: () => import('../views/job/TaskDetail.vue'), props: true },
      { path: 'submit', name: 'submit', component: () => import('../views/submit/index.vue') },
      { path: 'settings', name: 'settings', component: () => import('../views/Settings.vue') },
      { path: 'about', name: 'about', component: () => import('../views/About.vue') },
    ],
  },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
