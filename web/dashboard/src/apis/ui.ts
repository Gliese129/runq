import { api } from './client'
import type { ActionResponse } from '@/types/api'

/**
 * GET/PUT /api/v1/ui — the opaque per-user UI state blob (RQ2-1 c4).
 * The backend stores it next to config.yaml and never interprets it; every
 * key inside is frontend-owned (appearance levers here, project grouping
 * with the D ticket). PUT is a whole-document replace with no CAS —
 * read-merge-write so one feature never clobbers another's keys.
 *
 * Both verbs are silent: ui.json is a preference cache, and localStorage
 * carries the offline fallback — a down daemon must not toast.
 */
export const uiApi = {
  get: () => api.get<Record<string, unknown>>('/ui', { silent: true }),
  put: (doc: Record<string, unknown>) => api.put<ActionResponse>('/ui', doc, { silent: true }),
}
