import { ref } from 'vue'

export interface SnackMessage {
  text: string
  color?: string
  action?: string
  onAction?: () => void
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
    success: (text: string) => show({ text, color: 'success' }),
    error: (text: string, onRetry?: () => void) =>
      show({ text, color: 'error', action: onRetry ? 'Retry' : undefined, onAction: onRetry }),
    info: (text: string, action?: string, onAction?: () => void) =>
      show({ text, color: 'info', action, onAction }),
    warn: (text: string) => show({ text, color: 'warning' }),
  }
}
