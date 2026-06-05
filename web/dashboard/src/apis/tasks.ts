import { api, type RequestOptions } from './client'
import type { ActionResponse, TaskLogResponse, TaskView } from '@/types/api'

export interface TaskLogOptions {
  tail?: number
  offset?: number
}

export const tasksApi = {
  get: (taskId: string, opts?: RequestOptions) =>
    api.get<TaskView>(`/tasks/${encodeURIComponent(taskId)}`, opts),

  log: (taskId: string, opts: TaskLogOptions = {}) => {
    const params = new URLSearchParams()
    if (opts.tail != null) params.set('tail', String(opts.tail))
    if (opts.offset != null) params.set('offset', String(opts.offset))
    const query = params.toString()
    const path = `/tasks/${encodeURIComponent(taskId)}/log${query ? `?${query}` : ''}`
    return api.get<TaskLogResponse>(path)
  },

  metrics: (taskId: string) =>
    api.get<any[]>(`/tasks/${encodeURIComponent(taskId)}/metrics`),

  kill: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/kill`),

  retry: (taskId: string) =>
    api.post<ActionResponse>(`/tasks/${encodeURIComponent(taskId)}/retry`),
}
