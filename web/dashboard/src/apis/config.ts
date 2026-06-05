import { api } from './client'
import type { ConfigResponse } from '@/types/api'

export const configApi = {
  get: () => api.get<ConfigResponse>('/config'),
}
