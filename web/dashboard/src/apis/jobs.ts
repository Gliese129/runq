import { api, type RequestOptions } from './client'
import type {
  CompareRow, JobActivityResponse, JobConfigPayload, JobDetail, JobLogSearchResponse,
  JobPlanResponse, JobPreviewResponse, JobResultsResponse, JobSubmitResponse, JobSummary,
  MessageResponse, RefreshReceipt,
} from '@/types/api'

export interface SubmitJobOptions extends RequestOptions {
  preflightEnabled?: boolean
  forceSkipPreflight?: unknown
  target?: string
}

/** v1 submit-family body (spec §5.4, D12): options in the body, never in query. */
function submitBody(cfg: JobConfigPayload, skipPreflight = false, target = '') {
  return { config: cfg, target, skip_preflight: skipPreflight }
}

export const jobsApi = {
  list: (opts?: RequestOptions) => api.getList<JobSummary>('/jobs', opts),

  /** Project-scoped list — unlike the global list, it skips the archived-
   *  project cascade, so an archived project's page still shows its jobs. */
  listByProject: (project: string, opts?: RequestOptions) =>
    api.getList<JobSummary>(`/jobs?project=${encodeURIComponent(project)}`, opts),

  listArchived: () => api.getList<JobSummary>('/jobs?archived=true'),

  get: (jobId: string, opts?: RequestOptions) =>
    api.get<JobDetail>(`/jobs/${encodeURIComponent(jobId)}`, opts),

  /** GET /jobs/{id}/metrics with ?key= — ranked rows for the compare table. */
  compare: async (jobId: string, key: string, desc: boolean) => {
    const params = new URLSearchParams({ key, order: desc ? 'desc' : 'asc' })
    const res = await api.get<{ rows: CompareRow[] }>(
      `/jobs/${encodeURIComponent(jobId)}/metrics?${params}`)
    return res.rows ?? []
  },

  /** GET /jobs/{id}/metrics without ?key= — metric key discovery. */
  metricKeys: async (jobId: string) => {
    const res = await api.get<{ keys: string[] }>(`/jobs/${encodeURIComponent(jobId)}/metrics`)
    return res.keys ?? []
  },

  /** POST /jobs/plan — merged dry-run + resolve-note, one wizard call. */
  plan: (cfg: JobConfigPayload, target = '') =>
    api.post<JobPlanResponse>('/jobs/plan', submitBody(cfg, false, target), { silent: true }),

  /** GUI face of `--dry-run`: rendered run.sh + submit command, zero side effects. */
  previewSubmit: (cfg: JobConfigPayload, skipPreflight: boolean, target = '') =>
    api.post<JobPreviewResponse>('/jobs/preview', submitBody(cfg, skipPreflight, target), { silent: true }),

  submit: (cfg: JobConfigPayload, opts: SubmitJobOptions = {}) => {
    const skip = opts.forceSkipPreflight === true || opts.preflightEnabled === false
    return api.post<JobSubmitResponse>(
      '/jobs',
      submitBody(cfg, skip, opts.target ?? ''),
      { silent: opts.silent, timeoutMs: opts.timeoutMs },
    )
  },

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

  /** Force a reconcile from external sources (poll-model backends only).
   *  D22: the receipt always says whether the refresh actually happened. */
  refresh: (jobId: string) =>
    api.post<RefreshReceipt>(`/jobs/${encodeURIComponent(jobId)}/refresh`),

  /** RQ2-1 §A: columnar results wire — full runq.record ingest. */
  results: (jobId: string, opts?: RequestOptions) =>
    api.get<JobResultsResponse>(`/jobs/${encodeURIComponent(jobId)}/results`, opts),

  /** P6: fetch activity.tsv data for all tasks in a job (501 until implemented). */
  activity: (jobId: string) =>
    api.get<JobActivityResponse>(`/jobs/${encodeURIComponent(jobId)}/activity`),

  /** P6: search across all task logs in a job. */
  logSearch: (jobId: string, q: string, offset = 0, limit = 100) => {
    const params = new URLSearchParams({ q, offset: String(offset), limit: String(limit) })
    return api.get<JobLogSearchResponse>(`/jobs/${encodeURIComponent(jobId)}/log/search?${params}`)
  },
}
