import { onMounted, onUnmounted, ref, type Ref, watch } from 'vue'

/**
 * Unified polling composable.
 * - Calls fn(false) immediately (non-silent first load)
 * - Then fn(true) every `interval` ms (silent — won't toast)
 * - Pauses when browser tab is hidden
 * - Debounces: skips tick if previous call is still in-flight
 * - Optionally stops when `active` ref becomes false
 */
export function usePolling(
  fn: (silent: boolean) => void | Promise<void>,
  interval: number,
  active?: Ref<boolean>,
) {
  let timer: ReturnType<typeof setInterval> | null = null
  const inflight = ref(false)

  async function tick(silent: boolean) {
    if (inflight.value) return // debounce: skip if previous still running
    inflight.value = true
    try {
      await fn(silent)
    } finally {
      inflight.value = false
    }
  }

  function start() {
    stop()
    tick(false) // first call is non-silent
    timer = setInterval(() => {
      if (document.hidden) return
      if (active && !active.value) return
      tick(true) // subsequent calls are silent
    }, interval)
  }

  function stop() {
    if (timer) {
      clearInterval(timer)
      timer = null
    }
  }

  onMounted(start)
  onUnmounted(stop)

  if (active) {
    watch(active, (val) => {
      if (val) start()
      else stop()
    })
  }
}
