import axios from 'axios'
import { useSnackbar } from '@/composables/useSnackbar'
import { onApiSuccess, onApiError } from '@/composables/useConnection'

const http = axios.create({
  baseURL: '/api/dashboard',
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
})

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
  silent?: boolean
}

async function request<T>(method: string, path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
  try {
    const res = await http.request<T>({
      method,
      url: path,
      data: body,
    })
    onApiSuccess()
    return res.data
  } catch (e: any) {
    if (axios.isAxiosError(e)) {
      const status = e.response?.status || 0
      const msg = e.response?.data?.error || e.message || 'Network error'
      onApiError(msg)
      if (!opts?.silent && status !== 404) {
        snack.error(msg)
      }
      throw new ApiError(msg, status, path)
    }
    const msg = e instanceof Error ? e.message : 'Unknown error'
    onApiError(msg)
    if (!opts?.silent) snack.error(`Connection failed: ${msg}`)
    throw new ApiError(msg, 0, path)
  }
}

export const api = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, body, opts),
  put: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, body, opts),
  del: <T>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, undefined, opts),
}
