import { api, type RequestOptions } from './client'
import { useConfigStore } from '@/stores/config'
import type { FSEntry, ParseResult } from '@/types/api'

// v1: filesystem endpoints are target-scoped (/targets/{name}/fs/*) —
// the browsed filesystem is the one the job would run on. Callers may pass
// an explicit target; default is the current (bootstrap default) target.
function targetOf(target?: string): string {
  return encodeURIComponent(target ?? useConfigStore().currentTarget)
}

export const filesApi = {
  list: (path: string, target?: string) => {
    const params = new URLSearchParams({ path })
    return api.getList<FSEntry>(`/targets/${targetOf(target)}/fs/list?${params}`)
  },

  parseScript: (path: string, opts?: RequestOptions, target?: string) =>
    api.post<ParseResult>(`/targets/${targetOf(target)}/fs/parse-script`, { path }, opts),

  /**
   * Resolve a path pattern on the TARGET (RQ2-3). Owning-side walk: the
   * browser would otherwise pay one round trip per directory, which over
   * SFTP is the difference between one call and hundreds.
   * Zero matches is a normal empty result, not an error.
   */
  glob: (root: string, pattern: string, opts?: { limit?: number; target?: string }) => {
    const params = new URLSearchParams({ path: root, pattern })
    if (opts?.limit) params.set('limit', String(opts.limit))
    return api.get<{ items: FSEntry[]; truncated: boolean }>(
      `/targets/${targetOf(opts?.target)}/fs/glob?${params}`,
    )
  },

  /** Read a text file from the TARGET's filesystem (size-capped). */
  read: (path: string, target?: string) => {
    const params = new URLSearchParams({ path })
    return api.get<{ content: string }>(`/targets/${targetOf(target)}/fs/read?${params}`)
  },
}
