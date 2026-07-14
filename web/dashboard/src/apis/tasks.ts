import { api, API_BASE, type RequestOptions } from './client'
import type { ActionResponse, LogPage, MetricPoint, TaskMetricsResponse, TaskView } from '@/types/api'

/** Default page byte budget (mirrors logfile.DefaultBudgetBytes; server cap 1MB). */
export const DEFAULT_LOG_MAX_BYTES = 256 * 1024

export interface TaskLogOptions {
  /** Byte offset into the raw log file. Ignored when `tail` is set. */
  offset?: number
  /** Page byte budget (default 256 KB, server cap 1 MB). */
  maxBytes?: number
  /** Open at the tail: size − max_bytes, aligned past the first newline. */
  tail?: boolean
  /** Ask for total_lines / start_line (archive first page). */
  countLines?: boolean
  /** LEGACY line-count param; max_bytes takes priority server-side. */
  lines?: number
}

export const tasksApi = {
  get: (taskId: string, opts?: RequestOptions) =>
    api.get<TaskView>(`/tasks/${encodeURIComponent(taskId)}`, opts),

  log: (taskId: string, opts: TaskLogOptions = {}) => {
    const params = new URLSearchParams()
    if (opts.tail) params.set('tail', '1')
    else if (opts.offset != null) params.set('offset', String(opts.offset))
    params.set('max_bytes', String(opts.maxBytes ?? DEFAULT_LOG_MAX_BYTES))
    if (opts.countLines) params.set('count_lines', '1')
    if (opts.lines != null) params.set('lines', String(opts.lines))
    const path = `/tasks/${encodeURIComponent(taskId)}/log?${params.toString()}`
    return api.get<LogPage>(path)
  },

  logStream: (taskId: string, offset?: number, maxBytes?: number) => {
    const params = new URLSearchParams()
    if (offset != null) params.set('offset', String(offset))
    params.set('max_bytes', String(maxBytes ?? DEFAULT_LOG_MAX_BYTES))
    const path = `${API_BASE}/tasks/${encodeURIComponent(taskId)}/log/stream?${params.toString()}`
    return new EventSource(path)
  },

  /** Live tail-window metric points. `after` enables incremental fetch
   *  (only samples with ts > after) — pass the previous refreshed_at. */
  metrics: async (taskId: string, after = 0): Promise<MetricPoint[]> => {
    const query = after > 0 ? `?after=${after}` : ''
    const res = await api.get<TaskMetricsResponse>(
      `/tasks/${encodeURIComponent(taskId)}/metrics${query}`)
    return res.points ?? []
  },

  kill: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/kill`),

  retry: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/retry`),
}
