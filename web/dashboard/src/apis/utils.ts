import { api } from './client'
import type { LogPage } from '@/types/api'

export interface UtilsLogSession {
  id: string
  total_bytes: number
}

export interface UtilsLogReadOptions {
  offset?: number
  lines?: number
}

export const utilsApi = {
  /** Upload raw log content, get a session ID back. Goes through the
   *  shared client — the raw-axios version bypassed its error
   *  normalization, so a failed upload surfaced nothing (RQ2-4 ⑦). */
  uploadLog: (body: string | Blob): Promise<UtilsLogSession> =>
    api.post<UtilsLogSession>('/log-sessions', body, {
      contentType: 'application/octet-stream',
      timeoutMs: 60000,
    }),

  /** Read a page of ANSI-stripped lines (same shape as task log). */
  readLog: (id: string, opts: UtilsLogReadOptions = {}) => {
    const params = new URLSearchParams()
    if (opts.offset != null) params.set('offset', String(opts.offset))
    if (opts.lines != null) params.set('lines', String(opts.lines))
    const query = params.toString()
    return api.get<LogPage>(`/log-sessions/${encodeURIComponent(id)}${query ? `?${query}` : ''}`)
  },

  /** Delete an uploaded log session. */
  deleteLog: (id: string) =>
    api.del(`/log-sessions/${encodeURIComponent(id)}`),
}
