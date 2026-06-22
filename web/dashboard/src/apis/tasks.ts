import { api, type RequestOptions } from './client'
import type { ActionResponse, LogPage, TaskView } from '@/types/api'

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
    const path = `/api/dashboard/tasks/${encodeURIComponent(taskId)}/log/stream${query ? `?${query}` : ''}`
    return new EventSource(path)
  },

  metrics: (taskId: string) =>
    api.get<any[]>(`/tasks/${encodeURIComponent(taskId)}/metrics`),

  kill: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/kill`),

  retry: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/retry`),
}
