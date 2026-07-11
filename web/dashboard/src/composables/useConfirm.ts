import { ref } from 'vue'

export interface ConfirmOptions {
  title: string
  body?: string
  confirmText?: string
  cancelText?: string
  /** renders the confirm button in error color for destructive actions */
  danger?: boolean
}

// Module-level singleton — one dialog instance mounted in App.vue serves
// the whole app (same pattern as useSnackbar).
const visible = ref(false)
const opts = ref<ConfirmOptions>({ title: '' })
let resolver: ((v: boolean) => void) | null = null

/**
 * Promise-based confirmation. Usage:
 *   if (!await confirm({ title, body, danger: true })) return
 */
function confirm(options: ConfirmOptions): Promise<boolean> {
  // A second confirm while one is open settles the first as cancelled.
  resolver?.(false)
  opts.value = options
  visible.value = true
  return new Promise<boolean>((resolve) => {
    resolver = resolve
  })
}

function settle(v: boolean) {
  visible.value = false
  resolver?.(v)
  resolver = null
}

export function useConfirm() {
  return { visible, opts, confirm, settle }
}
