import { QueryClient } from '@tanstack/vue-query'

// One explicit QueryClient instance, importable OUTSIDE component setup
// (pinia stores, plain modules). Components should still prefer
// useQueryClient(); this export exists for the few non-setup callers.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      // Polling queries own their freshness; 2s staleTime just dedupes
      // multiple components mounting the same key in one navigation.
      staleTime: 2_000,
      refetchOnWindowFocus: true,
      // Background tab: stop polling (old usePolling checked document.hidden).
      refetchIntervalInBackground: false,
    },
  },
})
