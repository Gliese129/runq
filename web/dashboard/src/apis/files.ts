import { api, type RequestOptions } from './client'
import type { FSEntry, ParseResult } from '@/types/api'

export const filesApi = {
  list: (path: string) => {
    const params = new URLSearchParams({ path })
    return api.get<FSEntry[]>(`/fs/list?${params}`)
  },

  parseScript: (path: string, opts?: RequestOptions) =>
    api.post<ParseResult>('/fs/parse-script', { path }, opts),
}
