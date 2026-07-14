import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { gpuApi } from '@/apis/gpu'
import { useConfigStore } from '@/stores/config'
import { qk } from './keys'

export function useGpuQuery() {
  const config = useConfigStore()
  const query = useQuery({
    queryKey: qk.gpu,
    queryFn: ({ signal }) => gpuApi.list({ silent: true, signal }),
    // caps-gated: no GPU concept on this target → never fetch (render-or-
    // remove, never disable)
    enabled: computed(() => config.caps.gpu_map),
    refetchInterval: 5_000,
  })
  const freeCount = computed(() => (query.data.value ?? []).filter(g => !g.task_id).length)
  const totalCount = computed(() => (query.data.value ?? []).length)
  return { ...query, freeCount, totalCount }
}
