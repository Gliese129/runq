// Thin fetch wrapper. Always uses relative paths — vite proxy handles dev,
// same-origin handles production + SSH tunnel.

import { useSnackbar } from '@/composables/useSnackbar'
import { onApiSuccess, onApiError, connected } from '@/composables/useConnection'

const snack = useSnackbar()

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public endpoint: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

interface RequestOptions {
  /** Suppress toast on error (used by polling) */
  silent?: boolean
}

async function request<T>(method: string, path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
  let res: Response
  try {
    res = await fetch(`/api/dashboard${path}`, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    })
  } catch (e) {
    const msg = e instanceof Error ? e.message : 'Network error'
    onApiError(msg)
    if (!opts?.silent) {
      snack.error(`Connection failed: ${msg}`)
    }
    throw new ApiError(msg, 0, path)
  }

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    const msg = err.error || res.statusText
    onApiError(msg)
    if (!opts?.silent && res.status !== 404) {
      snack.error(msg)
    }
    throw new ApiError(msg, res.status, path)
  }

  onApiSuccess()
  return res.json()
}

export const api = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, body, opts),
  del: <T>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, undefined, opts),
}
