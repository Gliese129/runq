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
    /** current config generation on generation_conflict (RQ-75) */
    public currentGeneration?: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

/** true when err is a 409 generation_conflict (RQ-75 CAS write lost the race) */
export function isGenerationConflict(err: unknown): err is ApiError {
  return err instanceof ApiError && err.code === 'generation_conflict'
}

export interface RequestOptions {
  silent?: boolean
  timeoutMs?: number
  /** abort in-flight request (TanStack Query passes this per queryFn) */
  signal?: AbortSignal
  /** config generation to send as If-Match (RQ-75 CAS writes) */
  ifMatch?: string
}

async function request<T>(method: string, path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
  try {
    const res = await http.request<T>({
      method,
      url: path,
      data: body,
      timeout: opts?.timeoutMs,
      signal: opts?.signal,
      headers: opts?.ifMatch ? { 'If-Match': opts.ifMatch } : undefined,
    })
    onApiSuccess()
    return res.data
  } catch (e: any) {
    if (axios.isAxiosError(e)) {
      const status = e.response?.status || 0
      const msg = e.response?.data?.error || e.message || 'Network error'
      const code = e.response?.data?.code
      const currentGen = e.response?.data?.current_generation
      onApiError(msg)
      // Daemon-down has its own persistent banner + sidebar state; a raw
      // socket snackbar on top of that just makes the three surfaces fight.
      // generation_conflict gets a dedicated dialog — no snackbar on top.
      if (!opts?.silent && status !== 404 && code !== 'generation_conflict' && !isDaemonDownError(msg)) {
        snack.error(msg)
      }
      throw new ApiError(msg, status, path, code, currentGen)
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
