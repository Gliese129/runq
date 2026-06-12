import { api } from './client'
import type { ConfigResponse } from '@/types/api'

export interface HPCConfig {
  submit_template: string
  submit_id_regex: string
  status_template?: string
  status_parser?: string[]
  kill_template: string
}

export interface HPCConfigResponse {
  exists: boolean
  config: HPCConfig
  /** field → allowed {{placeholders}} — schema ships from the backend */
  placeholders: Record<string, string[]>
  path: string
}

export interface HPCCheckResult {
  Name: string
  Status: 'ok' | 'fail' | 'skip'
  Detail: string
}

export const configApi = {
  get: () => api.get<ConfigResponse>('/config'),

  getHPC: () => api.get<HPCConfigResponse>('/hpc-config'),
  getHPCPresets: () =>
    api.get<{ names: string[]; presets: Record<string, HPCConfig> }>('/hpc-config/presets'),
  putHPC: (cfg: HPCConfig) => api.put('/hpc-config', cfg),
  checkHPC: (cfg: HPCConfig) =>
    api.post<{ results: HPCCheckResult[] }>('/hpc-config/check', cfg),

  putGlobal: (mode: string, dataPath: string) =>
    api.put('/global-config', { mode, data_path: dataPath }),
}
