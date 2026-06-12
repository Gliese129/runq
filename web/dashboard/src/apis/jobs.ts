import { api, type RequestOptions } from './client'
import type { CompareRow, JobConfigPayload, JobDetail, JobSubmitResponse, JobSummary, MessageResponse } from '@/types/api'

export interface SubmitJobOptions extends RequestOptions {
  preflightEnabled?: boolean
  forceSkipPreflight?: unknown
}

export function submitJobPath(preflightEnabled = true, forceSkipPreflight: unknown = false): string {
  const skipPreflight = forceSkipPreflight === true || !preflightEnabled
  return skipPreflight ? '/jobs?no_preflight=1' : '/jobs'
}

export const jobsApi = {
  list: (opts?: RequestOptions) => api.get<JobSummary[]>('/jobs', opts),

  /** Project-scoped list — unlike the global list, it skips the archived-
   *  project cascade, so an archived project's page still shows its jobs. */
  listByProject: (project: string, opts?: RequestOptions) =>
    api.get<JobSummary[]>(`/jobs?project=${encodeURIComponent(project)}`, opts),

  get: (jobId: string, opts?: RequestOptions) =>
    api.get<JobDetail>(`/jobs/${encodeURIComponent(jobId)}`, opts),

  compare: (jobId: string, key: string, desc: boolean) => {
    const params = new URLSearchParams({
      key,
      order: desc ? 'desc' : 'asc',
    })
    return api.get<CompareRow[]>(`/jobs/${encodeURIComponent(jobId)}/compare?${params}`)
  },

  dryRun: (cfg: JobConfigPayload) =>
    api.post<Record<string, any>[]>('/jobs/dry-run', cfg),

  /** GUI face of `--dry-run`: rendered run.sh + submit command, zero side effects. */
  previewSubmit: (cfg: JobConfigPayload, skipPreflight: boolean) =>
    api.post<{ preview: string }>('/jobs/preview', { config: cfg, skip_preflight: skipPreflight }, { silent: true }),

  submit: (cfg: JobConfigPayload, opts: SubmitJobOptions = {}) =>
    api.post<JobSubmitResponse>(
      submitJobPath(opts.preflightEnabled, opts.forceSkipPreflight),
      cfg,
      { silent: opts.silent, timeoutMs: opts.timeoutMs },
    ),

  listArchived: () => api.get<JobSummary[]>('/jobs/archived'),

  archive: (jobId: string) =>
    api.post<MessageResponse>(`/jobs/${encodeURIComponent(jobId)}/archive`, {}),

  unarchive: (jobId: string) =>
    api.post<MessageResponse>(`/jobs/${encodeURIComponent(jobId)}/unarchive`, {}),

  kill: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/kill`),

  pause: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/pause`),

  resume: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/resume`),

  /** Force a reconcile from external sources (poll-model backends only). */
  refresh: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/refresh`),

  /** Preview note resolution ({{version}} scan etc.) — submit's code path. */
  resolveNote: (cfg: JobConfigPayload) =>
    api.post<{ resolved: string }>('/jobs/resolve-note', cfg, { silent: true }),
}
