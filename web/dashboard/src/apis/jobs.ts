import { api, type RequestOptions } from './client'
import type { CompareRow, JobConfigPayload, JobDetail, JobSubmitResponse, JobSummary } from '@/types/api'

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

  submit: (cfg: JobConfigPayload, opts: SubmitJobOptions = {}) =>
    api.post<JobSubmitResponse>(
      submitJobPath(opts.preflightEnabled, opts.forceSkipPreflight),
      cfg,
      { silent: opts.silent, timeoutMs: opts.timeoutMs },
    ),

  kill: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/kill`),

  pause: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/pause`),

  resume: (jobId: string) =>
    api.post(`/jobs/${encodeURIComponent(jobId)}/resume`),
}
