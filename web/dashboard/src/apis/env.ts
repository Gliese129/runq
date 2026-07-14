import { api } from './client'
import { useConfigStore } from '@/stores/config'

export const envApi = {
  /** Conda envs on the given target (defaults to the current target). */
  listCondaEnvs: (target?: string) => {
    const t = target ?? useConfigStore().currentTarget
    return api.getList<string>(`/targets/${encodeURIComponent(t)}/python-envs`)
  },
}
