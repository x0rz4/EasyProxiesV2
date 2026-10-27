// Browser sessions persist only in the server's HttpOnly cookie. The returned
// bearer remains in memory for clients that cannot use cookies.
let authToken: string | null = null
let authGeneration = 0
try { localStorage.removeItem('auth_token') } catch { /* storage can be disabled */ }

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export function setToken(token: string | null) {
  authToken = token
  authGeneration++
}

export function clearToken() { setToken(null) }

export async function readErrorMessage(res: Response): Promise<string> {
  try {
    const body: unknown = await res.json()
    if (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string') return body.error
  } catch { /* non-JSON proxy errors retain their HTTP status */ }
  return `HTTP ${res.status}`
}

export async function authFetch(path: string, options: RequestInit = {}): Promise<Response> {
  const generation = authGeneration
  const headers = new Headers(options.headers)
  if (authToken) headers.set('Authorization', `Bearer ${authToken}`)
  const response = await fetch(path, { ...options, headers, credentials: 'same-origin', cache: 'no-store' })
  if (response.status === 401) {
    await response.body?.cancel()
    // A response from the old session must not log out a newly signed-in user.
    if (generation === authGeneration) {
      clearToken()
      window.dispatchEvent(new CustomEvent('auth:unauthorized'))
    }
    throw new ApiError('未授权，请重新登录', 401)
  }
  return response
}

export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (typeof options.body === 'string') headers.set('Content-Type', 'application/json')
  const response = await authFetch(path, { ...options, headers })
  if (!response.ok) throw new ApiError(await readErrorMessage(response), response.status)
  const text = await response.text()
  return text ? JSON.parse(text) as T : {} as T
}
