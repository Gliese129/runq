import { api, type RequestOptions } from './client'
import { useConfigStore } from '@/stores/config'
import type { GPUSlot } from '@/types/api'

export const gpuApi = {
  /** GPU slots on the given target (defaults to the current target). */
  list: (opts?: RequestOptions, target?: string) => {
    const t = target ?? useConfigStore().currentTarget
    return api.getList<GPUSlot>(`/targets/${encodeURIComponent(t)}/gpus`, opts)
  },
}
