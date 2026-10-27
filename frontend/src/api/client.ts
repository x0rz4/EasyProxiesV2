import type {
  GroupHealEvent,
  AuthResponse,
  NodesResponse,
  DebugResponse,
  SettingsData,
  SettingsUpdateResponse,
  SubscriptionRefreshSettings,
  APIKey,
  APIKeysResponse,
  NodeMaintenanceResponse,
  ConfigNodesResponse,
  ConfigNodePayload,
  ConfigNodeMutationResponse,
  SubscriptionStatus,
  Subscription,
  SubscriptionPayload,
  SubscriptionsResponse,
  SubscriptionActionResponse,
  SubscriptionRefreshResponse,
  ProbeSSEEvent,
  ProbeOperationsSettings,
  ProbeOperationsStatus,
  UnlockResult,
  UnlockResultsResponse,
  TrafficStreamEvent,
  DebugLogEvent,
  GeoipStatus,
  GroupPoolsResponse,
  GroupPoolPayload,
  GroupPoolMutationResponse,
  NodeCheckSettings,
  NodeCheckStages,
  NodeCheckTask,
  NodeCheckEvent,
  NodeCheckResultItem,
  NodeTagAssignment,
  Tag,
  TagCondition,
  TagMutexGroup,
  TagPayload,
  TagPreviewResponse,
  TagSchema,
  TagsResponse,
} from '../types'

import { ApiError, authFetch, clearToken, readErrorMessage, request, setToken } from './transport'
export { ApiError } from './transport'
import { readEvents, parseStreamJSON, waitForReconnect } from './sse'

// ---- Auth API ----

/** Check if password is required & login */
export async function checkAuth(signal?: AbortSignal): Promise<AuthResponse> {
  return request('/api/auth', { signal })
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

export async function logout() {
  await request('/api/auth/logout', { method: 'POST' })
  clearToken()
}

// ---- Nodes API ----

function nodeIDsQuery(ids?: number[]): string {
  return ids === undefined ? '' : `?node_ids=${encodeURIComponent(ids.join(','))}`
}

export async function fetchNodes(ids?: number[], signal?: AbortSignal): Promise<NodesResponse> {
  return request<NodesResponse>(`/api/nodes${nodeIDsQuery(ids)}`, { signal })
}

/** Explicit bulk actions resolve stable IDs in bounded detail requests. */
export async function fetchNodesByIDs(ids: number[]) {
  const nodes: NodesResponse['nodes'] = []
  for (let offset = 0; offset < ids.length; offset += 200) {
    nodes.push(...(await fetchNodes(ids.slice(offset, offset + 200))).nodes)
  }
  return nodes
}

export async function fetchProbeSettings(signal?: AbortSignal): Promise<ProbeOperationsSettings> {
  return request<ProbeOperationsSettings>('/api/operations/probe-settings', { signal })
}

export async function fetchNodeCheckSettings(): Promise<NodeCheckSettings> {
  return request<NodeCheckSettings>('/api/operations/node-check-settings')
}

export async function updateNodeCheckSettings(settings: NodeCheckSettings): Promise<NodeCheckSettings> {
  return request<NodeCheckSettings>('/api/operations/node-check-settings', { method: 'PUT', body: JSON.stringify(settings) })
}

export async function fetchNodeCheckResults(ids?: number[], signal?: AbortSignal): Promise<{ results: NodeCheckResultItem[] }> {
  return request(`/api/node-check/results${nodeIDsQuery(ids)}`, { signal })
}

export async function fetchNodeCheckTasks(): Promise<{ tasks: NodeCheckTask[] }> {
  return request('/api/node-check/tasks')
}

export async function createNodeCheckTask(nodeIds: number[], stages: NodeCheckStages, settings?: NodeCheckSettings): Promise<NodeCheckTask> {
  const response = await request<{ task: NodeCheckTask }>('/api/node-check/tasks', { method: 'POST', body: JSON.stringify({ node_ids: nodeIds, stages, settings }) })
  return response.task
}

export async function cancelNodeCheckTask(taskId: string): Promise<void> {
  await request(`/api/node-check/tasks/${encodeURIComponent(taskId)}`, { method: 'DELETE' })
}

export function streamNodeCheckTask(taskId: string, onEvent: (event: NodeCheckEvent) => void, onError?: (error: Error) => void): AbortController {
  const controller = new AbortController()
  void (async () => {
    while (!controller.signal.aborted) {
      try {
        for await (const frame of readEvents('/api/node-check/tasks/' + encodeURIComponent(taskId) + '/events', controller)) {
          const event = parseStreamJSON<NodeCheckEvent>(frame.data)
          if (!event) continue
          onEvent(event)
          if (event.type === 'done') return
          if (event.task && ['completed', 'failed', 'cancelled', 'interrupted'].includes(event.task.status)) {
            // A reconnect can start with an already-terminal task snapshot.
            // Finish the UI exactly once before releasing its stream reader.
            onEvent({ ...event, type: 'done' })
            return
          }
        }
      } catch (error) {
        if (controller.signal.aborted) return
        if (error instanceof ApiError) { onError?.(error); return }
      }
      // GET resumes the server-side task through a fresh aggregate snapshot.
      await waitForReconnect(controller.signal, 1000)
    }
  })()
  return controller
}

export async function updateProbeSettings(settings: ProbeOperationsSettings): Promise<ProbeOperationsSettings> {
  return request<ProbeOperationsSettings>('/api/operations/probe-settings', {
    method: 'PUT',
    body: JSON.stringify(settings),
  })
}

export async function fetchProbeStatus(): Promise<ProbeOperationsStatus> {
  return request<ProbeOperationsStatus>('/api/operations/probe-status')
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

// ---- Node tags API ----

export async function fetchTags(): Promise<TagsResponse> {
  return request<TagsResponse>('/api/tags')
}

export async function createTag(payload: TagPayload): Promise<{ tag: Tag }> {
  return request<{ tag: Tag }>('/api/tags', { method: 'POST', body: JSON.stringify(payload) })
}

export async function updateTag(id: number, payload: TagPayload): Promise<{ tag: Tag }> {
  return request<{ tag: Tag }>(`/api/tags/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteTag(id: number, force = false): Promise<{ ok: boolean }> {
  return request<{ ok: boolean }>(`/api/tags/${id}${force ? '?force=1' : ''}`, { method: 'DELETE' })
}

export async function setTagAuto(id: number, autoEnabled: boolean): Promise<{ tag: Tag }> {
  return request<{ tag: Tag }>(`/api/tags/${id}/auto`, {
    method: 'PATCH', body: JSON.stringify({ auto_enabled: autoEnabled }),
  })
}

export async function fetchTagSchema(): Promise<TagSchema> {
  return request<TagSchema>('/api/tags/schema')
}

export async function previewTagRule(body: {
  rule: TagCondition
  tag_id?: number
  mutex_group_id?: number
  priority?: number
  node_ids?: number[]
  limit?: number
}, signal?: AbortSignal): Promise<TagPreviewResponse> {
  return request<TagPreviewResponse>('/api/tags/preview', {
    method: 'POST', body: JSON.stringify(body), signal,
  })
}

export async function recomputeTags(nodeIds?: number[]): Promise<{ changed_node_ids: number[] }> {
  return request<{ changed_node_ids: number[] }>('/api/tags/recompute', {
    method: 'POST', body: JSON.stringify(nodeIds ? { node_ids: nodeIds } : {}),
  })
}

export async function seedTagTemplates(): Promise<{ created: string[]; skipped: string[]; conflicts: string[] }> {
  return request('/api/tags/templates', { method: 'POST' })
}

export async function fetchTagAssignments(nodeIds?: number[], signal?: AbortSignal): Promise<{ assignments: NodeTagAssignment[] }> {
  // The legacy tag endpoint treats an empty query as all nodes. Resolve an
  // explicit empty client selection locally rather than widening its scope.
  if (nodeIds?.length === 0) return { assignments: [] }
  return request(`/api/tags/assignments${nodeIDsQuery(nodeIds)}`, { signal })
}

export async function setNodeManualTags(nodeId: number, tagIds: number[]): Promise<{ assignment: NodeTagAssignment }> {
  return request<{ assignment: NodeTagAssignment }>(`/api/tags/nodes/${nodeId}`, {
    method: 'PUT', body: JSON.stringify({ tag_ids: tagIds }),
  })
}

export async function fetchTagMutexGroups(): Promise<{ mutex_groups: TagMutexGroup[] }> {
  return request('/api/tags/mutex-groups')
}

export async function createTagMutexGroup(payload: { name: string; description?: string }): Promise<{ mutex_group: TagMutexGroup }> {
  return request('/api/tags/mutex-groups', { method: 'POST', body: JSON.stringify(payload) })
}

export async function updateTagMutexGroup(id: number, payload: { name?: string; description?: string }): Promise<{ mutex_group: TagMutexGroup }> {
  return request(`/api/tags/mutex-groups/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteTagMutexGroup(id: number): Promise<{ ok: boolean }> {
  return request(`/api/tags/mutex-groups/${id}`, { method: 'DELETE' })
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
export async function fetchUnlockResults(ids?: number[], signal?: AbortSignal): Promise<UnlockResultsResponse> {
  return request<UnlockResultsResponse>(`/api/nodes/unlock-results${nodeIDsQuery(ids)}`, { signal })
}

/** Probe all nodes with SSE progress updates */
export function probeAllNodes(onEvent: (event: ProbeSSEEvent) => void, onError?: (error: Error) => void): AbortController {
  const controller = new AbortController()
  void (async () => {
    try {
      for await (const frame of readEvents('/api/nodes/probe-all', controller, 'POST')) {
        const event = parseStreamJSON<ProbeSSEEvent>(frame.data)
        if (!event) continue
        onEvent(event)
        if (event.type === 'complete') return
      }
      if (!controller.signal.aborted) throw new Error('探测连接提前断开，请刷新运行状态')
    } catch (error) {
      if (!controller.signal.aborted) onError?.(error as Error)
    }
  })()
  // A mutating probe-all POST must never be replayed automatically.
  return controller
}

// ---- Traffic Stream API ----

/** Subscribe real-time traffic speeds via SSE */
export function streamTraffic(onEvent: (event: TrafficStreamEvent) => void, onError?: (error: Error) => void): AbortController {
  const controller = new AbortController()
  void (async () => {
    try {
      for await (const frame of readEvents('/api/nodes/traffic/stream', controller)) {
        const event = parseStreamJSON<TrafficStreamEvent>(frame.data)
        if (event?.type === 'traffic') onEvent(event)
      }
      if (!controller.signal.aborted) throw new Error('流量连接已断开')
    } catch (error) {
      if (!controller.signal.aborted) onError?.(error as Error)
    }
  })()
  return controller
}

// ---- Debug API ----

export function streamSpeedtest(tag: string, onSpeed: (mbps: number, done: boolean) => void, onError: (error: Error) => void): AbortController {
  const controller = new AbortController()
  void (async () => {
    try {
      for await (const frame of readEvents(`/api/nodes/${encodeURIComponent(tag)}/speedtest`, controller)) {
        if (frame.type === 'error') {
          const payload = parseStreamJSON<{ error?: string }>(frame.data)
          throw new Error(payload?.error || frame.data || '测速失败')
        }
        const payload = parseStreamJSON<{ mbps?: number }>(frame.data)
        if (typeof payload?.mbps !== 'number' || !Number.isFinite(payload.mbps)) continue
        onSpeed(payload.mbps, frame.type === 'done')
        if (frame.type === 'done') return
      }
      if (!controller.signal.aborted) throw new Error('测速连接提前断开')
    } catch (error) { if (!controller.signal.aborted) onError(error as Error) }
  })()
  return controller
}

export async function fetchDebug(signal?: AbortSignal): Promise<DebugResponse> {
  return request<DebugResponse>('/api/debug', { signal })
}

export function streamDebugLogs(onEvent: (event: DebugLogEvent) => void, onStatus: (connected: boolean) => void): AbortController {
  const controller = new AbortController()
  void (async () => {
    while (!controller.signal.aborted) {
      try {
        for await (const frame of readEvents('/api/debug/stream', controller, 'GET', () => onStatus(true))) {
          const event = parseStreamJSON<DebugLogEvent>(frame.data)
          if (event) onEvent(event)
        }
      } catch (error) {
        if (controller.signal.aborted || (error instanceof ApiError && error.status === 401)) return
      } finally { onStatus(false) }
      await waitForReconnect(controller.signal, 2000)
    }
  })()
  return controller
}

// ---- Settings API ----

export async function fetchSettings(signal?: AbortSignal): Promise<SettingsData> {
  return request<SettingsData>('/api/settings', { signal })
}

export async function updateSettings(settings: SettingsData): Promise<SettingsUpdateResponse> {
  return request<SettingsUpdateResponse>('/api/settings', {
    method: 'PUT',
    body: JSON.stringify(settings),
  })
}

// ---- Config Nodes CRUD API ----

export async function fetchConfigNodes(subscriptionId?: number, ids?: number[], signal?: AbortSignal): Promise<ConfigNodesResponse> {
  const query = new URLSearchParams()
  if (subscriptionId !== undefined) query.set('subscription_id', String(subscriptionId))
  if (ids !== undefined) query.set('node_ids', ids.join(','))
  return request<ConfigNodesResponse>(`/api/nodes/config?${query}`, { signal })
}

export async function createConfigNode(payload: ConfigNodePayload): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>('/api/nodes/config', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

// Node names are not unique in the store (two subscriptions may ship the same
// display name), so mutations address the stable node ID.
export async function updateConfigNode(id: number, payload: ConfigNodePayload): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/id/${id}`, {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

export async function deleteConfigNode(id: number): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/id/${id}`, {
    method: 'DELETE',
  })
}

export async function toggleConfigNode(id: number, enabled: boolean): Promise<ConfigNodeMutationResponse> {
  return request<ConfigNodeMutationResponse>(`/api/nodes/config/id/${id}`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

export async function batchToggleConfigNodes(ids: number[], enabled: boolean): Promise<{ message: string; success: number; total: number; errors?: string[] }> {
  return request('/api/nodes/config/batch-toggle', {
    method: 'POST',
    body: JSON.stringify({ ids, enabled }),
  })
}

export async function batchDeleteConfigNodes(ids: number[]): Promise<{ message: string; success: number; total: number; errors?: string[] }> {
  return request('/api/nodes/config/batch-delete', {
    method: 'POST',
    body: JSON.stringify({ ids }),
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

// ---- Subscription auto-refresh settings (subscription management page) ----

export async function fetchSubscriptionRefreshSettings(): Promise<SubscriptionRefreshSettings> {
  return request<SubscriptionRefreshSettings>('/api/subscription/refresh-settings')
}

export async function updateSubscriptionRefreshSettings(payload: SubscriptionRefreshSettings): Promise<SettingsUpdateResponse> {
  return request<SettingsUpdateResponse>('/api/subscription/refresh-settings', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// ---- Session-free API credentials ----

export async function listAPIKeys(): Promise<APIKeysResponse> {
  return request<APIKeysResponse>('/api/api-keys')
}

export async function createAPIKey(name: string): Promise<APIKey> {
  return request<APIKey>('/api/api-keys', {
    method: 'POST',
    body: JSON.stringify({ name }),
  })
}

export async function deleteAPIKey(id: number): Promise<{ ok: boolean; message: string }> {
  return request(`/api/api-keys/${id}`, { method: 'DELETE' })
}

// ---- Node maintenance API ----

/** Lists every managed node with its online/offline state and offline streak. */
export async function fetchNodeMaintenance(): Promise<NodeMaintenanceResponse> {
  return request<NodeMaintenanceResponse>('/api/nodes/maintenance')
}

export async function refreshSubscription(): Promise<SubscriptionRefreshResponse> {
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

// ---- Export API ----

export interface ExportResult {
  /** 导出的纯文本内容，每行一个代理 URI */
  text: string
  /** 后端返回的实际导出条数 */
  count: number
}

export async function exportProxies(): Promise<ExportResult> {
  const res = await authFetch('/api/export')
  if (!res.ok) throw new ApiError(await readErrorMessage(res), res.status)
  const text = await res.text()
  const rawCount = res.headers.get('X-Export-Count')
  const parsed = rawCount === null ? Number.NaN : Number(rawCount)
  const count = Number.isFinite(parsed) ? parsed : text.split('\n').filter(line => line.trim()).length
  return { text, count }
}

// ---- Import API ----

/** 单条导入问题，line 为 0 表示无法定位到具体行。 */
export interface ImportIssue {
  line?: number
  kind: string
  message: string
  uri?: string
}

export interface ImportNodesResult {
  reloaded?: boolean
  reload_error?: string
  message: string
  imported: number
  parsed: number
  created: number
  duplicates_skipped: number
  invalid: number
  failed?: number
  would_create?: number
  dry_run?: boolean
  duplicate_groups?: Array<{ existing_node: string; incoming_node: string }>
  endpoint_collisions?: Array<{ endpoint: string; existing_nodes: string[]; incoming_node: string }>
  errors?: string[]
  issues?: ImportIssue[]
}

export async function importNodes(content: string, options?: { dryRun?: boolean }): Promise<ImportNodesResult> {
  return request('/api/import', {
    method: 'POST',
    body: JSON.stringify({ content, dry_run: options?.dryRun ?? false }),
  })
}
import type { GroupPreview, NodeFacets, NodeFilterSpec, NodeViewPage, NodeViewResult, SavedNodeFilter } from '../types/nodeView'

export function queryNodeView(spec: NodeFilterSpec, page: NodeViewPage, signal?: AbortSignal) {
  return request<NodeViewResult>('/api/nodes/query', { method: 'POST', body: JSON.stringify({ spec, page }), signal })
}
export function fetchNodeFacets(signal?: AbortSignal, runtimeOnly = false, healthyOnly = false) {
  const params = new URLSearchParams()
  if (runtimeOnly) params.set('runtime_only', 'true')
  if (healthyOnly) params.set('healthy_only', 'true')
  return request<NodeFacets>(`/api/nodes/facets${params.size ? `?${params}` : ''}`, { signal })
}
export function listSavedNodeFilters() {
  return request<{ filters: SavedNodeFilter[] }>('/api/node-filters')
}
export function saveNodeFilter(name: string, spec: NodeFilterSpec, previous?: SavedNodeFilter) {
  return request<SavedNodeFilter>(previous ? `/api/node-filters/${previous.id}` : '/api/node-filters', {
    method: previous ? 'PUT' : 'POST', body: JSON.stringify({ name, spec, revision: previous?.revision || 0 }),
  })
}
export function deleteNodeFilter(id: number) {
  return request<{ success: boolean }>(`/api/node-filters/${id}`, { method: 'DELETE' })
}
export function previewGroup(id: number | undefined, payload: GroupPoolPayload, signal?: AbortSignal) {
  return request<GroupPreview>(id ? `/api/groups/${id}/preview` : '/api/groups/preview', {
    method: 'POST', body: JSON.stringify(payload), signal,
  })
}

export function queryNodeIDs(spec: NodeFilterSpec) {
  return request<{ ids: number[]; snapshot_revision: string }>('/api/nodes/query-ids', { method: 'POST', body: JSON.stringify({ spec }) })
}

export async function fetchGroupHealEvents(groupID: number, signal?: AbortSignal): Promise<{ events: GroupHealEvent[] }> {
  return request(`/api/groups/${groupID}/healing-events?limit=50`, { signal })
}
