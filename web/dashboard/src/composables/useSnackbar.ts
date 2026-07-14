import { ref } from 'vue'
import i18n from '@/plugins/i18n'

export interface SnackMessage {
  text: string
  color?: string
  action?: string
  onAction?: () => void
  /** v-snackbar timeout in ms; -1 = stays until dismissed. */
  timeout?: number
}

const queue = ref<SnackMessage[]>([])
const current = ref<SnackMessage | null>(null)
const visible = ref(false)

// Pending dismiss timer. A show() inside the 300ms dismiss window used to
// race it: show flipped visible back on, then the stale timeout nulled
// `current` — blank snackbar, message swallowed. show() now cancels the
// timer, and the timeout re-checks visible before touching state.
let dismissTimer: ReturnType<typeof setTimeout> | null = null

function show(msg: SnackMessage) {
  if (dismissTimer) {
    clearTimeout(dismissTimer)
    dismissTimer = null
  }
  if (visible.value) {
    queue.value.push(msg)
  } else {
    current.value = msg
    visible.value = true
  }
}

function dismiss() {
  visible.value = false
  dismissTimer = setTimeout(() => {
    dismissTimer = null
    if (visible.value) return // a newer show() took over this slot
    if (queue.value.length > 0) {
      current.value = queue.value.shift()!
      visible.value = true
    } else {
      current.value = null
    }
  }, 300)
}

export function useSnackbar() {
  return {
    current,
    visible,
    dismiss,
    // Timeouts scale with severity: success is glanceable (3.5s), errors
    // need reading time (8s), and an error with a Retry action must not
    // vanish before the user can act on it (-1 = manual dismiss).
    success: (text: string) => show({ text, color: 'success', timeout: 3500 }),
    error: (text: string, onRetry?: () => void) =>
      show({
        text,
        color: 'error',
        action: onRetry ? i18n.global.t('common.retry') : undefined,
        onAction: onRetry,
        timeout: onRetry ? -1 : 8000,
      }),
    info: (text: string, action?: string, onAction?: () => void) =>
      show({ text, color: 'info', action, onAction, timeout: action ? -1 : 5000 }),
    warn: (text: string) => show({ text, color: 'warning', timeout: 6000 }),
  }
}
