import { ref, computed } from 'vue'

const consecutiveErrors = ref(0)
const lastError = ref('')
const lastSuccessAt = ref(Date.now())

export const connected = computed(() => consecutiveErrors.value < 2)

export function onApiSuccess() {
  consecutiveErrors.value = 0
  lastSuccessAt.value = Date.now()
}

export function onApiError(msg: string) {
  consecutiveErrors.value++
  lastError.value = msg
}

export function useConnection() {
  return {
    connected,
    consecutiveErrors,
    lastError,
    lastSuccessAt,
  }
}
