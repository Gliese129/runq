import { api } from './client'
import type { RequestOptions } from './client'
import type { LogPage } from '@/types/api'

/** One runq self-log file present on the daemon machine (RQ-74). */
export interface DaemonLogFile {
  name: string
  size: number
  mtime: number
}

/** runq self-logs (RQ-74): read the daemon's own log from the browser. */
export const daemonLogsApi = {
  list: (opts?: RequestOptions) =>
    api.get<{ files: DaemonLogFile[] }>('/daemon/logs', { silent: true, ...opts }),

  page: (
    name: string,
    params: { offset?: number; tail?: boolean; max_bytes?: number },
    opts?: RequestOptions,
  ) => {
    const q = new URLSearchParams()
    if (params.tail) q.set('tail', '1')
    if (params.offset !== undefined) q.set('offset', String(params.offset))
    if (params.max_bytes) q.set('max_bytes', String(params.max_bytes))
    return api.get<LogPage>(`/daemon/logs/${encodeURIComponent(name)}?${q.toString()}`, {
      silent: true,
      ...opts,
    })
  },
}
