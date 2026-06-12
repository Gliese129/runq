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
  interval: number | (() => number),
  active?: Ref<boolean>,
) {
  let timer: ReturnType<typeof setTimeout> | null = null
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

  // setTimeout chain (not setInterval): the interval is re-evaluated every
  // cycle, so a lazy `() => number` that depends on async-loaded state
  // (e.g. capabilities arriving after mount) takes effect on the next tick
  // instead of being frozen at start().
  function schedule() {
    timer = setTimeout(() => {
      if (!document.hidden && (!active || active.value)) {
        tick(true) // subsequent calls are silent
      }
      schedule()
    }, typeof interval === 'function' ? interval() : interval)
  }

  function start() {
    stop()
    tick(false) // first call is non-silent
    schedule()
  }

  function stop() {
    if (timer) {
      clearTimeout(timer)
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
