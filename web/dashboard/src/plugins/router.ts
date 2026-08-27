import { createRouter, createWebHashHistory, type RouteRecordRaw } from 'vue-router'

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: () => import('../layouts/DefaultLayout.vue'),
    children: [
      { path: '', name: 'overview', component: () => import('../views/Overview.vue') },
      // NOTE: 'new' and the '/edit' suffix are effectively reserved words in
      // the projects namespace — vue-router ranks static segments above
      // params, so a project literally named "new" can't be opened by URL.
      { path: 'projects/new', name: 'project-new', component: () => import('../views/project-edit/index.vue') },
      { path: 'projects/:project/edit', name: 'project-edit', component: () => import('../views/project-edit/index.vue'), props: true },
      { path: 'projects/:project', name: 'project', component: () => import('../views/ProjectJobs.vue'), props: true },
      { path: 'projects/:project/:jobId', name: 'job-detail', component: () => import('../views/job/index.vue'), props: true },
      { path: 'projects/:project/:jobId/:taskId', name: 'task-detail', component: () => import('../views/job/TaskDetail.vue'), props: true },
      { path: 'targets/:name?', name: 'target', component: () => import('../views/target/index.vue'), props: true },
      { path: 'submit', name: 'submit', component: () => import('../views/submit/index.vue') },
      { path: 'utils/log-viewer', name: 'log-viewer', component: () => import('../views/utils/LogViewer.vue') },
      { path: 'settings', name: 'settings', component: () => import('../views/Settings.vue') },
      { path: 'about', name: 'about', component: () => import('../views/About.vue') },
    ],
  },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
