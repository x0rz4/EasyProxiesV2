import type {
  AuthResponse,
  NodesResponse,
  DebugResponse,
  SettingsData,
  SettingsUpdateResponse,
  ConfigNodesResponse,
  ConfigNodePayload,
  ConfigNodeMutationResponse,
  SubscriptionStatus,
  Subscription,
  SubscriptionPayload,
  SubscriptionsResponse,
  SubscriptionNodesResponse,
  SubscriptionActionResponse,
  ProbeSSEEvent,
  UnlockResult,
  UnlockResultsResponse,
  UnlockSSEEvent,
  TrafficStreamEvent,
  DebugLogEvent,
  GeoipStatus,
  GroupPoolsResponse,
  GroupPoolPayload,
  GroupPoolMutationResponse,
} from '../types'

// ---- Token management ----

let authToken: string | null = localStorage.getItem('auth_token')

export function getToken(): string | null {
  return authToken
}

export function setToken(token: string | null) {
  authToken = token
  if (token) {
    localStorage.setItem('auth_token', token)
  } else {
    localStorage.removeItem('auth_token')
  }
}

export function clearToken() {
  setToken(null)
}

// ---- Base request helper ----

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    ...(options.headers as Record<string, string> || {}),
  }

  // Add auth header if we have a token
  if (authToken) {
    headers['Authorization'] = `Bearer ${authToken}`
  }

  // Set JSON content type for non-GET requests with body
  if (options.body && typeof options.body === 'string') {
    headers['Content-Type'] = 'application/json'
  }

  const res = await fetch(path, {
    ...options,
    headers,
    credentials: 'include', // send cookies
  })

  if (res.status === 401) {
    clearToken()
    // Dispatch a custom event so App can react
    window.dispatchEvent(new CustomEvent('auth:unauthorized'))
    throw new ApiError('未授权，请重新登录', 401)
  }

  if (!res.ok) {
    let msg = `HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body.error) msg = body.error
    } catch { /* ignore parse errors */ }
    throw new ApiError(msg, res.status)
  }

  // Handle empty responses
  const text = await res.text()
  if (!text) return {} as T
  return JSON.parse(text) as T
}

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

// ---- Auth API ----

/** Check if password is required & login */
export async function checkAuth(): Promise<AuthResponse> {
  // Use GET-like behavior: /api/auth without POST returns password status
  const res = await fetch('/api/auth', { credentials: 'include' })
  return res.json()
}

export async function login(password: string): Promise<AuthResponse> {
  const res = await fetch('/api/auth', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
    credentials: 'include',
  })

  if (!res.ok) {
    const body = await res.json()
    throw new ApiError(body.error || '登录失败', res.status)
  }

  const data: AuthResponse = await res.json()
  if (data.token) {
    setToken(data.token)
  }
  return data
}

export function logout() {
  clearToken()
}

// ---- Nodes API ----

export async function fetchNodes(): Promise<NodesResponse> {
  return request<NodesResponse>('/api/nodes')
}

// ---- Group pools API ----

export async function listGroupPools(): Promise<GroupPoolsResponse> {
  return request<GroupPoolsResponse>('/api/groups')
}

export async function createGroupPool(payload: GroupPoolPayload): Promise<GroupPoolMutationResponse> {
  return request('/api/groups', { method: 'POST', body: JSON.stringify(payload) })
}

export async function updateGroupPool(id: number, payload: GroupPoolPayload): Promise<GroupPoolMutationResponse> {
  return request(`/api/groups/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteGroupPool(id: number) {
  return request(`/api/groups/${id}`, { method: 'DELETE' })
}

export async function restoreGroupMember(groupId: number, nodeId: number) {
  return request(`/api/groups/${groupId}/members/${nodeId}/restore`, { method: 'POST' })
}

export async function activateGroupMember(groupId: number, nodeId: number) {
  return request(`/api/groups/${groupId}/members/${nodeId}/activate`, { method: 'POST' })
}

export async function removeGroupMember(groupId: number, nodeId: number) {
  return request(`/api/groups/${groupId}/members/${nodeId}`, { method: 'DELETE' })
}

export async function resetGroupSubscriptionToken(groupId: number): Promise<{ message: string; token: string }> {
  return request(`/api/groups/${groupId}/subscription/reset-token`, { method: 'POST' })
}

export async function probeNode(tag: string): Promise<{ message: string; latency_ms: number }> {
  return request(`/api/nodes/${encodeURIComponent(tag)}/probe`, { method: 'POST' })
}

export async function releaseNode(tag: string): Promise<{ message: string }> {
  return request(`/api/nodes/${encodeURIComponent(tag)}/release`, { method: 'POST' })
}

/** Unlock detection for a single node. */
export async function unlockNode(tag: string): Promise<UnlockResult> {
  const data = await request<{ error?: string } & UnlockResult>(
    `/api/nodes/${encodeURIComponent(tag)}/unlock`,
    { method: 'POST' }
  )
  if ((data as { error?: string }).error) {
    throw new ApiError((data as { error: string }).error, 400)
  }
  return data
}

/** Fetch the last persisted unlock detection result for every node. */
export async function fetchUnlockResults(): Promise<UnlockResultsResponse> {
  return request<UnlockResultsResponse>('/api/nodes/unlock-results')
}

/** Probe all nodes with SSE progress updates */
export function probeAllNodes(
  onEvent: (event: ProbeSSEEvent) => void,
  onError?: (error: Error) => void
): AbortController {
  const controller = new AbortController()

  const doFetch = async () => {
    try {
      const headers: Record<string, string> = {}
      if (authToken) {
        headers['Authorization'] = `Bearer ${authToken}`
      }

      const res = await fetch('/api/nodes/probe-all', {
        method: 'POST',
        headers,
        credentials: 'include',
        signal: controller.signal,
      })

      if (!res.ok) {
        throw new ApiError(`探测失败: HTTP ${res.status}`, res.status)
      }

      const reader = res.body?.getReader()
      if (!reader) throw new Error('No response body')

      const decoder = new TextDecoder()
      let buffer = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) break

        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() || ''

        for (const line of lines) {
          const trimmed = line.trim()
          if (trimmed.startsWith('data: ')) {
            try {
              const data = JSON.parse(trimmed.slice(6)) as ProbeSSEEvent
              onEvent(data)
            } catch { /* skip malformed events */ }
          }
        }
      }
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        onError?.(err as Error)
      }
    }
  }

  doFetch()
  return controller
}

/** Unlock detection for all nodes with SSE progress updates. */
export function unlockAllNodes(
  onEvent: (event: UnlockSSEEvent) => void,
  onError?: (error: Error) => void
): AbortController {
  const controller = new AbortController()

  const doFetch = async () => {
    try {
      const headers: Record<string, string> = {}
      if (authToken) {
        headers['Authorization'] = `Bearer ${authToken}`
      }

      const res = await fetch('/api/nodes/unlock-all', {
        method: 'POST',
        headers,
        credentials: 'include',
        signal: controller.signal,
      })

      if (!res.ok) {
        throw new ApiError(`解锁检测失败: HTTP ${res.status}`, res.status)
      }

      const reader = res.body?.getReader()
      if (!reader) throw new Error('No response body')

      const decoder = new TextDecoder()
      let buffer = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) break

        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() || ''

        for (const line of lines) {
          const trimmed = line.trim()
          if (trimmed.startsWith('data: ')) {
            try {
              const data = JSON.parse(trimmed.slice(6)) as UnlockSSEEvent
              onEvent(data)
            } catch { /* skip malformed events */ }
          }
        }
      }
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        onError?.(err as Error)
      }
    }
  }

  doFetch()
  return controller
}

// ---- Traffic Stream API ----

/** Subscribe real-time traffic speeds via SSE */
export function streamTraffic(
  onEvent: (event: TrafficStreamEvent) => void,
  onError?: (error: Error) => void
): AbortController {
  const controller = new AbortController()

  const doFetch = async () => {
    try {
      const headers: Record<string, string> = {}
      if (authToken) {
        headers['Authorization'] = `Bearer ${authToken}`
      }

      const res = await fetch('/api/nodes/traffic/stream', {
        method: 'GET',
        headers,
        credentials: 'include',
        signal: controller.signal,
      })

      if (!res.ok) {
        throw new ApiError(`流量流订阅失败: HTTP ${res.status}`, res.status)
      }

      const reader = res.body?.getReader()
      if (!reader) throw new Error('No response body')

      const decoder = new TextDecoder()
      let buffer = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) break

        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() || ''

        for (const line of lines) {
          const trimmed = line.trim()
          if (trimmed.startsWith('data: ')) {
            try {
              const data = JSON.parse(trimmed.slice(6)) as TrafficStreamEvent
              if (data.type === 'traffic') {
                onEvent(data)
              }
            } catch { /* skip malformed events */ }
          }
        }
      }
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        onError?.(err as Error)
      }
    }
  }

  doFetch()
  return controller
}

// ---- Debug API ----

export async function fetchDebug(): Promise<DebugResponse> {
  return request<DebugResponse>('/api/debug')
}

export function streamDebugLogs(onEvent: (event: DebugLogEvent) => void, onStatus: (connected: boolean) => void): AbortController {
  const controller = new AbortController()
  const connect = async () => {
    while (!controller.signal.aborted) {
      try {
        const headers: Record<string, string> = {}
        if (authToken) headers.Authorization = `Bearer ${authToken}`
        const response = await fetch('/api/debug/stream', { headers, credentials: 'include', signal: controller.signal })
        if (response.status === 401) {
          clearToken()
          window.dispatchEvent(new CustomEvent('auth:unauthorized'))
          return
        }
        if (!response.ok || !response.body) throw new ApiError(`HTTP ${response.status}`, response.status)
        onStatus(true)
        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''
        while (!controller.signal.aborted) {
          const { done, value } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })
          const messages = buffer.split('\n\n')
          buffer = messages.pop() ?? ''
          for (const message of messages) {
            const line = message.split('\n').find((item) => item.startsWith('data: '))
            if (line) onEvent(JSON.parse(line.slice(6)) as DebugLogEvent)
          }
        }
      } catch (error) {
        if ((error as Error).name === 'AbortError') return
      } finally {
        onStatus(false)
      }
      await new Promise((resolve) => setTimeout(resolve, 2000))
    }
  }
  void connect()
  return controller
}

// ---- Settings API ----

export async function fetchSettings(): Promise<SettingsData> {
  return request<SettingsData>('/api/settings')
}

export async function updateSettings(settings: SettingsData): Promise<SettingsUpdateResponse> {
  return request<SettingsUpdateResponse>('/api/settings', {
    method: 'PUT',
    body: JSON.stringify(settings),
  })
}

// ---- Config Nodes CRUD API ----

export async function fetchConfigNodes(subscriptionId?: number): Promise<ConfigNodesResponse> {
  const query = subscriptionId === undefined ? '' : `?subscription_id=${encodeURIComponent(subscriptionId)}`
  return request<ConfigNodesResponse>(`/api/nodes/config${query}`)
}

export async function createConfigNode(payload: ConfigNodePayload): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>('/api/nodes/config', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function updateConfigNode(name: string, payload: ConfigNodePayload): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

export async function deleteConfigNode(name: string): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/${encodeURIComponent(name)}`, {
    method: 'DELETE',
  })
}

export async function toggleConfigNode(name: string, enabled: boolean): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

export async function batchToggleConfigNodes(names: string[], enabled: boolean): Promise<{ message: string; success: number; total: number; errors?: string[] }> {
  return request('/api/nodes/config/batch-toggle', {
    method: 'POST',
    body: JSON.stringify({ names, enabled }),
  })
}

export async function batchDeleteConfigNodes(names: string[]): Promise<{ message: string; success: number; total: number; errors?: string[] }> {
  return request('/api/nodes/config/batch-delete', {
    method: 'POST',
    body: JSON.stringify({ names }),
  })
}

// ---- Reload API ----

export async function triggerReload(): Promise<{ message: string }> {
  return request('/api/reload', { method: 'POST' })
}

// ---- GeoIP database management API ----

export async function fetchGeoipStatus(): Promise<GeoipStatus> {
  return request<GeoipStatus>('/api/geoip/status')
}

export async function downloadGeoipDatabase(): Promise<GeoipStatus> {
  return request<GeoipStatus>('/api/geoip/download', { method: 'POST' })
}

export async function updateGeoipDatabase(): Promise<GeoipStatus> {
  return request<GeoipStatus>('/api/geoip/update', { method: 'POST' })
}

// ---- Subscription API ----

export async function fetchSubscriptionStatus(): Promise<SubscriptionStatus> {
  return request<SubscriptionStatus>('/api/subscription/status')
}

export async function refreshSubscription(): Promise<{ message: string; node_count: number }> {
  return request('/api/subscription/refresh', { method: 'POST' })
}

export async function listSubscriptions(): Promise<SubscriptionsResponse> {
  return request<SubscriptionsResponse>('/api/subscriptions')
}

export async function createSubscription(payload: SubscriptionPayload): Promise<Subscription> {
  return request<Subscription>('/api/subscriptions', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function updateSubscription(id: number, payload: SubscriptionPayload): Promise<Subscription> {
  return request<Subscription>(`/api/subscriptions/${id}`, {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

export async function deleteSubscription(id: number): Promise<SubscriptionActionResponse> {
  return request<SubscriptionActionResponse>(`/api/subscriptions/${id}`, { method: 'DELETE' })
}

export async function toggleSubscription(id: number, enabled: boolean): Promise<SubscriptionActionResponse> {
  return request<SubscriptionActionResponse>(`/api/subscriptions/${id}/enabled`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

export async function activateSubscription(id: number): Promise<SubscriptionActionResponse> {
  return request<SubscriptionActionResponse>(`/api/subscriptions/${id}/activate`, { method: 'POST' })
}

export async function refreshOneSubscription(id: number): Promise<SubscriptionActionResponse> {
  return request<SubscriptionActionResponse>(`/api/subscriptions/${id}/refresh`, { method: 'POST' })
}

export async function listSubscriptionNodes(id: number): Promise<SubscriptionNodesResponse> {
  return request<SubscriptionNodesResponse>(`/api/subscriptions/${id}/nodes`)
}

// ---- Export API ----

export async function exportProxies(): Promise<string> {
  const headers: Record<string, string> = {}
  if (authToken) {
    headers['Authorization'] = `Bearer ${authToken}`
  }
  const res = await fetch('/api/export', {
    headers,
    credentials: 'include',
  })
  if (!res.ok) throw new ApiError('导出失败', res.status)
  return res.text()
}

// ---- Import API ----

export async function importNodes(content: string): Promise<{ message: string; imported: number; errors?: string[] }> {
  return request('/api/import', {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}
