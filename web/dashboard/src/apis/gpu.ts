import { api, type RequestOptions } from './client'
import type { GPUSlot } from '@/types/api'

export const gpuApi = {
  list: (opts?: RequestOptions) => api.get<GPUSlot[]>('/gpu', opts),
}
