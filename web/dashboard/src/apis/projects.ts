import { api } from './client'
import type { MessageResponse, ProjectConfig, ProjectPayload, ProjectSummary } from '@/types/api'

export const projectsApi = {
  list: () => api.get<ProjectSummary[]>('/projects'),

  match: (dir: string) => {
    const params = new URLSearchParams({ dir })
    return api.get<ProjectSummary[]>(`/projects/match?${params}`)
  },

  get: (name: string) =>
    api.get<ProjectConfig>(`/projects/${encodeURIComponent(name)}`),

  create: (payload: ProjectPayload) =>
    api.post<MessageResponse>('/projects', payload),

  update: (name: string, payload: ProjectPayload) =>
    api.put<MessageResponse>(`/projects/${encodeURIComponent(name)}`, payload),

  rename: (name: string, newName: string) =>
    api.post<MessageResponse>(`/projects/${encodeURIComponent(name)}/rename`, { new_name: newName }),
}
