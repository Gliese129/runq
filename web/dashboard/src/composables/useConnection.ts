import { ref, computed } from 'vue'

const consecutiveErrors = ref(0)
const lastError = ref('')
const lastSuccessAt = ref(Date.now())

// A raw unix-socket failure only happens in daemon mode (the HPC/template
// backend reads the store directly, no socket). Treat it as a definitive
// "daemon not running" signal rather than a transient blip: surface it
// immediately, without the 2-strike debounce used for ordinary errors.
const DAEMON_DOWN_SIGNATURES = [
  'connection refused',
  'econnrefused',
  'no such file or directory',
  'dial unix',
  'socket',
]

export function isDaemonDownError(msg: string): boolean {
  const m = msg.toLowerCase()
  return DAEMON_DOWN_SIGNATURES.some((s) => m.includes(s))
}

export const daemonDown = computed(
  () => consecutiveErrors.value > 0 && isDaemonDownError(lastError.value),
)

// Ordinary errors tolerate a single blip (2-strike); a daemon-down signal does not.
export const connected = computed(
  () => !daemonDown.value && consecutiveErrors.value < 2,
)

// Single source of truth for the i18n key the sidebar / banner should show,
// so the dot, the banner, and the snackbar can never disagree.
export const statusKey = computed(() => {
  if (daemonDown.value) return 'status.daemon_down'
  return connected.value ? 'status.connected' : 'status.disconnected'
})

// Detail line under the status. For daemon-down we hide the raw socket error
// (noise) and point at the recovery command instead.
export const detailKey = computed(() =>
  daemonDown.value ? 'status.daemon_down_hint' : '',
)

export function onApiSuccess() {
  consecutiveErrors.value = 0
  lastError.value = ''
  lastSuccessAt.value = Date.now()
}

export function onApiError(msg: string) {
  consecutiveErrors.value++
  lastError.value = msg
}

export function useConnection() {
  return {
    connected,
    daemonDown,
    statusKey,
    detailKey,
    consecutiveErrors,
    lastError,
    lastSuccessAt,
  }
}
