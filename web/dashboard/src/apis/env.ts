import { api } from './client'

export const envApi = {
  listCondaEnvs: () => api.get<string[]>('/conda/envs'),
}
