import { api } from './client'
import type { ActionResponse, ConfigResponse, HealthResponse, RefreshReceipt } from '@/types/api'

/**
 * Management view of one compute target (mirrors Go config.TargetConfig).
 * The GUI form is schema-driven: the placeholder vocabulary ships from the
 * backend (single source of truth), so unknown fields must survive a
 * read-modify-write round trip — hence the index signature.
 */
export interface TargetConfig {
  name: string
  gpus?: number[]
  scheduler?: string
  workspace?: string
  ssh?: Record<string, unknown>
  max_inflight?: number
  remote_cli?: boolean
  trust_empty_list?: boolean
  [key: string]: unknown
}

export interface TargetsListResponse {
  items: TargetConfig[]
  /** field → allowed {{placeholders}} — schema ships from the backend */
  placeholders: Record<string, string[]>
  path: string
}

export interface TargetPresetsResponse {
  /** canonical preset order (maps don't preserve it) */
  names: string[]
  presets: Record<string, TargetConfig>
}

export interface HPCCheckResult {
  name: string
  status: 'ok' | 'fail' | 'skip'
  detail: string
}

export const configApi = {
  /** Bootstrap summary: paths, default target, per-target capabilities. */
  get: () => api.get<ConfigResponse>('/config'),

  /** PUT /config — global keys (D5: mode is gone). Takes effect on next start. */
  putGlobal: (dataPath: string, defaultTarget = '') =>
    api.put<ActionResponse>('/config', { data_path: dataPath, default_target: defaultTarget }),

  health: () => api.get<HealthResponse>('/health', { silent: true }),

  // ── Targets management (spec §5.2, D10 — /hpc-config* is retired) ──

  listTargets: () => api.get<TargetsListResponse>('/targets'),

  targetPresets: () => api.get<TargetPresetsResponse>('/targets/presets'),

  putTarget: (name: string, cfg: TargetConfig) =>
    api.put<ActionResponse>(`/targets/${encodeURIComponent(name)}`, cfg),

  deleteTarget: (name: string) =>
    api.del<ActionResponse>(`/targets/${encodeURIComponent(name)}`),

  checkTarget: (name: string, cfg: TargetConfig) =>
    api.post<{ results: HPCCheckResult[] }>(`/targets/${encodeURIComponent(name)}/check`, cfg),

  connectTarget: (name: string) =>
    api.post<ActionResponse>(`/targets/${encodeURIComponent(name)}/connect`),

  disconnectTarget: (name: string) =>
    api.post<ActionResponse>(`/targets/${encodeURIComponent(name)}/disconnect`),

  refreshTarget: (name: string) =>
    api.post<RefreshReceipt>(`/targets/${encodeURIComponent(name)}/refresh`),
}
