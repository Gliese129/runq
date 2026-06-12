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

function show(msg: SnackMessage) {
  if (visible.value) {
    queue.value.push(msg)
  } else {
    current.value = msg
    visible.value = true
  }
}

function dismiss() {
  visible.value = false
  setTimeout(() => {
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
