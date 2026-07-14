import axios from 'axios'
import { useSnackbar } from '@/composables/useSnackbar'
import { onApiSuccess, onApiError, isDaemonDownError } from '@/composables/useConnection'
import type { ListEnvelope } from '@/types/api'

// v1 spec-first API — protocol-shape compatibility is owned by path
// versioning (legacy /api/dashboard/* is gone on the backend).
export const API_BASE = '/api/v1'

const http = axios.create({
  baseURL: API_BASE,
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
})

const snack = useSnackbar()

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public endpoint: string,
    /** machine-readable backend error code (v1 error envelope), if any */
    public code?: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export interface RequestOptions {
  silent?: boolean
  timeoutMs?: number
  /** abort in-flight request (TanStack Query passes this per queryFn) */
  signal?: AbortSignal
}

async function request<T>(method: string, path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
  try {
    const res = await http.request<T>({
      method,
      url: path,
      data: body,
      timeout: opts?.timeoutMs,
      signal: opts?.signal,
    })
    onApiSuccess()
    return res.data
  } catch (e: any) {
    if (axios.isAxiosError(e)) {
      const status = e.response?.status || 0
      const msg = e.response?.data?.error || e.message || 'Network error'
      const code = e.response?.data?.code
      onApiError(msg)
      // Daemon-down has its own persistent banner + sidebar state; a raw
      // socket snackbar on top of that just makes the three surfaces fight.
      if (!opts?.silent && status !== 404 && !isDaemonDownError(msg)) {
        snack.error(msg)
      }
      throw new ApiError(msg, status, path, code)
    }
    const msg = e instanceof Error ? e.message : 'Unknown error'
    onApiError(msg)
    if (!opts?.silent && !isDaemonDownError(msg)) snack.error(`Connection failed: ${msg}`)
    throw new ApiError(msg, 0, path)
  }
}

export const api = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, body, opts),
  put: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, body, opts),
  del: <T>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, undefined, opts),

  /** GET a v1 collection endpoint and unwrap its ListEnvelope to items. */
  getList: async <T>(path: string, opts?: RequestOptions): Promise<T[]> =>
    (await request<ListEnvelope<T>>('GET', path, undefined, opts)).items ?? [],

  /** GET a v1 collection endpoint keeping envelope metadata (stale etc.). */
  getEnvelope: <T>(path: string, opts?: RequestOptions): Promise<ListEnvelope<T>> =>
    request<ListEnvelope<T>>('GET', path, undefined, opts),
}
