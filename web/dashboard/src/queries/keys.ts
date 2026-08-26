// Query key factory — the single vocabulary for the server-state cache.
// Hierarchy matters: invalidating ['job', id] hits the detail AND its
// compare sub-keys; invalidating ['jobs'] hits every list variant.
export const qk = {
  jobs: ['jobs'] as const,
  jobsArchived: ['jobs', 'archived'] as const,
  projectJobs: (project: string) => ['jobs', 'project', project] as const,
  job: (id: string) => ['job', id] as const,
  compare: (id: string, key: string, desc: boolean) => ['job', id, 'compare', key, desc] as const,
  results: (id: string) => ['job', id, 'results'] as const,
  task: (id: string) => ['task', id] as const,
  taskMetrics: (id: string) => ['task', id, 'metrics'] as const,
  gpu: ['gpu'] as const,
  health: ['health'] as const,
}
