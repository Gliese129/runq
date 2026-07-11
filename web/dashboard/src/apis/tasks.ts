import { api, API_BASE, type RequestOptions } from './client'
import type { ActionResponse, LogPage, MetricPoint, TaskMetricsResponse, TaskView } from '@/types/api'

export interface TaskLogOptions {
  /** Byte offset into the raw log file (default 0). */
  offset?: number
  /** Number of lines to return (default 200, max 5000). */
  lines?: number
}

export const tasksApi = {
  get: (taskId: string, opts?: RequestOptions) =>
    api.get<TaskView>(`/tasks/${encodeURIComponent(taskId)}`, opts),

  log: (taskId: string, opts: TaskLogOptions = {}) => {
    const params = new URLSearchParams()
    if (opts.offset != null) params.set('offset', String(opts.offset))
    if (opts.lines != null) params.set('lines', String(opts.lines))
    const query = params.toString()
    const path = `/tasks/${encodeURIComponent(taskId)}/log${query ? `?${query}` : ''}`
    return api.get<LogPage>(path)
  },

  logStream: (taskId: string, offset?: number) => {
    const params = new URLSearchParams()
    if (offset != null) params.set('offset', String(offset))
    const query = params.toString()
    const path = `${API_BASE}/tasks/${encodeURIComponent(taskId)}/log/stream${query ? `?${query}` : ''}`
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
