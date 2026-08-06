import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { configApi } from '@/apis/config'
import { daemonDown, connected } from '@/composables/useConnection'
import { qk } from './keys'

// /health poll (RQ-74): feeds the status bar (targets online, forward
// states, daemon version/uptime). Doubles as the RECONNECT PROBE: when the
// connection is down the poll tightens to 3s, and its first success flips
// useConnection back to connected (client.ts records every success), which
// is what ends "reconnecting" mode — no separate ping loop.
export function useHealthQuery() {
  const query = useQuery({
    queryKey: qk.health,
    queryFn: ({ signal }) => configApi.health({ silent: true, signal }),
    refetchInterval: computed(() => (daemonDown.value || !connected.value ? 3_000 : 15_000)),
    // Keep probing while the tab is visible even after failures; staleTime 0
    // so a manual invalidate always refetches.
    retry: false,
  })
  return query
}
