import { api, type RequestOptions } from './client'
import type { FSEntry, ParseResult } from '@/types/api'

export const filesApi = {
  list: (path: string) => {
    const params = new URLSearchParams({ path })
    return api.get<FSEntry[]>(`/fs/list?${params}`)
  },

  parseScript: (path: string, opts?: RequestOptions) =>
    api.post<ParseResult>('/fs/parse-script', { path }, opts),

  /** Read a text file from the SERVER's filesystem (size-capped). */
  read: (path: string) => {
    const params = new URLSearchParams({ path })
    return api.get<{ content: string }>(`/fs/read?${params}`)
  },
}
