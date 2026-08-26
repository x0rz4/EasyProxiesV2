// ---- Node & Snapshot types (maps to monitor.Snapshot) ----

export interface NodeInfo {
  node_id: number
  tag: string
  name: string
  uri: string
  mode: string
  listen_address?: string
  port?: number
  region?: string
  country?: string
  tags?: string[]
}

export interface TimelineEvent {
  time: string
  success: boolean
  latency_ms: number
  error?: string
  destination?: string
}

export interface NodeSnapshot extends NodeInfo {
  failure_count: number
  success_count: number
  blacklisted: boolean
  blacklisted_until: string
  active_connections: number
  last_error?: string
  last_failure?: string
  last_success?: string
  last_probe_latency?: number
  last_latency_ms: number
  available: boolean
  initial_check_done: boolean
  total_upload: number
  total_download: number
  timeline?: TimelineEvent[]
}

// ---- API Response types ----

export interface NodesResponse {
  nodes: NodeSnapshot[]
  total_nodes: number
  total_upload: number
  total_download: number
  upload_speed?: number
  download_speed?: number
  traffic_sampled?: string
  region_stats: Record<string, number>
  region_healthy: Record<string, number>
}

// ---- Group pool types ----

export interface GroupNodeOption {
  id: number
  name: string
  uri: string
  region?: string
  country?: string
  enabled: boolean
  tag?: string
  status: 'normal' | 'unavailable' | 'blacklisted' | 'pending' | 'disabled'
  latency_ms: number
  available: boolean
  initial_check_done: boolean
  selectable: boolean
}

export type GroupMemberStatus = 'ALIVE' | 'SUSPECT' | 'EVICTED'

export interface GroupMember {
  node_id: number
  tag: string
  name: string
  region?: string
  country?: string
  status: GroupMemberStatus
  failure_count: number
  last_error?: string
  evicted_at?: string
  latency_ms: number
  available: boolean
  is_active: boolean
}

export interface GroupPool {
  id: number
  name: string
  bind_address: string
  bind_port: number
  protocol: string
  username?: string
  password?: string
  dispatch_mode: 'fixed' | 'lowest_latency' | 'random'
  regions: string[]
  explicit_node_ids: number[]
  excluded_node_ids: number[]
  failure_window_seconds: number
  failure_threshold: number
  health_check_seconds: number
  current_active_node_id?: number
  enabled: boolean
  runtime_status: 'starting' | 'ready' | 'reconfiguring' | 'stopped' | 'error'
  runtime_error?: string
  subscription_enabled: boolean
  subscription_token?: string
  subscription_mode: 'members' | 'entry'
  external_host?: string
  current_active_tag?: string
  members: GroupMember[]
  member_count: number
  alive_count: number
  evicted_count: number
  created_at: string
  updated_at: string
}

export interface GroupPoolsResponse {
  groups: GroupPool[]
  nodes: GroupNodeOption[]
  port_range: { start: number; end: number }
}

export interface GroupPoolPayload {
  name: string
  bind_address: string
  bind_port: number
  protocol: string
  username: string
  password: string
  dispatch_mode: 'fixed' | 'lowest_latency' | 'random'
  regions: string[]
  explicit_node_ids: number[]
  excluded_node_ids: number[]
  failure_window_seconds: number
  failure_threshold: number
  health_check_seconds: number
  enabled: boolean
  subscription_enabled: boolean
  subscription_mode: 'members' | 'entry'
  external_host: string
}

export interface GroupPoolMutationResponse {
  group: GroupPool
  reloaded: boolean
  reload_error?: string
  rolled_back?: boolean
  removed_unavailable_node_ids: number[]
}

export interface DebugNode {
  tag: string
  name: string
  mode: string
  port: number
  failure_count: number
  success_count: number
  active_connections: number
  last_latency_ms: number
  last_success: string
  last_failure: string
  last_error: string
  blacklisted: boolean
  total_upload: number
  total_download: number
  timeline: TimelineEvent[]
}

export interface DebugResponse {
  nodes: DebugNode[]
  total_calls: number
  total_success: number
  success_rate: number
}

export interface SettingsData {
  // Global
  log_level: string
  external_ip: string
  skip_cert_verify: boolean

  // Listener
  listener_enabled: boolean
  listener_address: string
  listener_port: number
  listener_protocol: string
  listener_username: string
  listener_password: string

  // Multi-port
  multi_port_enabled: boolean
  multi_port_address: string
  multi_port_base_port: number
  multi_port_protocol: string
  multi_port_username: string
  multi_port_password: string

  // Pool
  pool_mode: string
  pool_failure_threshold: number
  pool_blacklist_duration: string

  // Management
  management_enabled: boolean
  management_listen: string
  management_probe_target: string
  management_password: string
  management_health_check_interval: string

  // Subscription refresh
  sub_refresh_enabled: boolean
  sub_refresh_interval: string
  sub_refresh_timeout: string
  sub_refresh_health_check_timeout: string
  sub_refresh_drain_timeout: string
  sub_refresh_min_available_nodes: number

  // GeoIP
  geoip_enabled: boolean
  geoip_database_path: string
  geoip_auto_update_enabled: boolean
  geoip_auto_update_interval: string

  // Subscriptions
  subscriptions: string[]
}

export interface SettingsUpdateResponse {
  message: string
  saved: boolean
  need_reload: boolean
  need_restart: boolean
  reloaded: boolean
  reload_error?: string
  applied: string[]
  pending: string[]
}

export interface DebugLogEvent {
  node_tag: string
  node_name: string
  event: TimelineEvent
}

// ---- Auth types ----

export interface AuthResponse {
  message: string
  token?: string
  no_password?: boolean
}

export interface ErrorResponse {
  error: string
}

// ---- Config Node CRUD types ----

export interface ConfigNodePayload {
  name: string
  uri: string
  port: number
  username: string
  password: string
}

export interface ConfigNodeConfig {
  name: string
  uri: string
  port: number
  username: string
  password: string
  source?: string
  disabled?: boolean
  subscription_ids: number[]
}

export interface ConfigNodesResponse {
  nodes: ConfigNodeConfig[]
}

export interface ConfigNodeMutationResponse {
  node?: ConfigNodeConfig
  message: string
  reloaded?: boolean
  reload_error?: string
}

// ---- Subscription types ----

export type SubscriptionFormat = 'auto' | 'clash' | 'base64' | 'sing-box'

export interface Subscription {
  id: number
  name: string
  url: string
  format: SubscriptionFormat
  user_agent: string
  enabled: boolean
  refresh_interval_seconds: number
  refresh_timeout_seconds: number
  sort_order: number
  last_attempt: string
  last_success: string
  last_error: string
  node_count: number
  etag: string
  last_modified: string
  created_at: string
  updated_at: string
}

export interface SubscriptionPayload {
  name: string
  url: string
  format: SubscriptionFormat
  user_agent: string
  enabled: boolean
  refresh_interval_seconds: number
  refresh_timeout_seconds: number
  sort_order: number
}

export interface SubscriptionNodeData {
  id: number
  uri: string
  name: string
  source: string
  port: number
  username?: string
  password?: string
  region?: string
  country?: string
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface SubscriptionNode {
  subscription_id: number
  position: number
  node: SubscriptionNodeData
}

export interface SubscriptionsResponse {
  subscriptions: Subscription[]
}

export interface SubscriptionNodesResponse {
  nodes: SubscriptionNode[]
}

export interface SubscriptionActionResponse {
  ok: boolean
  parsed?: number
  created?: number
  updated?: number
  duplicates_skipped?: number
  invalid?: number
}

export interface SubscriptionRefreshResponse {
  message: string
  node_count: number
  parsed: number
  created: number
  updated: number
  duplicates_skipped: number
  invalid: number
}

// ---- GeoIP database management types ----

export interface GeoipDatabaseInfo {
  database_path: string
  exists: boolean
  size_bytes: number
  modified_at: string
  download_url: string
}

export interface GeoipStatus {
  enabled: boolean
  database: GeoipDatabaseInfo
  message?: string
  reload_hint?: boolean
}

export interface SubscriptionStatus {
  enabled: boolean
  has_subscriptions?: boolean
  last_refresh?: string
  next_refresh?: string
  node_count?: number
  last_error?: string
  refresh_count?: number
  is_refreshing?: boolean
	message?: string
	parsed?: number
	created?: number
	updated?: number
	duplicates_skipped?: number
	invalid?: number
}

// ---- SSE Probe types ----

export interface ProbeSSEStart {
  type: 'start'
  total: number
}

export interface ProbeSSEProgress {
  type: 'progress'
  tag: string
  name: string
  latency: number
  status: 'success' | 'error'
  error: string
  current: number
  total: number
  progress: number
}

export interface ProbeSSEComplete {
  type: 'complete'
  total: number
  success: number
  failed: number
}

export type ProbeSSEEvent = ProbeSSEStart | ProbeSSEProgress | ProbeSSEComplete

// ---- Manual node diagnostics ----

export interface NodeCheckSettings {
  latency_url: string
  speed_url: string
  landing_ip_url: string
  latency_timeout: string
  speed_duration: string
  speed_request_timeout: string
  quality_timeout: string
  max_download_bytes: number
  peak_sample_interval: string
  latency_concurrency: number
  speed_concurrency: number
  quality_concurrency: number
  include_handshake: boolean
  ippure_enabled: boolean
  ippure_url: string
  ip_api_enabled: boolean
  ip_api_base_url: string
}

export interface NodeCheckStages {
  latency: boolean
  speed: boolean
  quality: boolean
  unlock: boolean
}

export type NodeCheckStatus = 'untested' | 'success' | 'partial' | 'failed' | 'disabled' | 'skipped'

export interface NodeDetectionResult {
  node_id: number
  task_id?: string
  latency_status: NodeCheckStatus
  latency_ms: number | null
  latency_error?: string
  latency_checked_at?: string
  speed_status: NodeCheckStatus
  average_bytes_per_second: number | null
  peak_bytes_per_second: number | null
  bytes_downloaded: number
  speed_duration_ms: number
  speed_error?: string
  speed_checked_at?: string
  exit_ip?: string
  exit_ip_family?: string
  exit_ip_status: NodeCheckStatus
  exit_ip_error?: string
  exit_ip_checked_at?: string
  updated_at: string
}

export interface NodeIPQualityResult {
  node_id: number
  task_id?: string
  provider: 'ippure' | 'ip-api' | string
  status: NodeCheckStatus
  ip?: string
  family?: string
  country?: string
  country_code?: string
  asn?: string
  org?: string
  isp?: string
  is_broadcast: boolean | null
  is_residential: boolean | null
  fraud_score: number | null
  proxy: boolean | null
  hosting: boolean | null
  mobile: boolean | null
  reason?: string
  checked_at: string
}

export interface NodeCheckResultItem {
  node_id: number
  detection?: NodeDetectionResult
  quality: NodeIPQualityResult[]
  exit_ip_drift: boolean
}

export interface NodeCheckStageStats {
  total: number
  completed: number
  success: number
  failed: number
  skipped: number
}

export interface NodeCheckTask {
  id: string
  status: 'pending' | 'running' | 'completed' | 'failed' | 'cancelled' | 'interrupted'
  stages: NodeCheckStages
  settings: Record<string, string | number | boolean>
  stats: Record<string, NodeCheckStageStats>
  total_nodes: number
  completed_nodes: number
  downloaded_bytes: number
  error?: string
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface NodeCheckEvent {
  sequence: number
  type: 'task' | 'progress' | 'result' | 'done'
  task?: NodeCheckTask
  phase?: 'latency' | 'speed' | 'quality' | 'unlock'
  node_id?: number
  tag?: string
  name?: string
  status?: string
  error?: string
  latency_ms?: number
  speed?: {
    average_bytes_per_second: number
    peak_bytes_per_second: number
    bytes_downloaded: number
    duration_ms: number
  }
  speed_progress?: {
    bytes_downloaded: number
    elapsed_ms: number
    average_bytes_per_second: number
  }
  quality?: NodeIPQualityResult
}

export interface ProbeOperationsSettings {
  probe_target: string
  health_check_interval: string
  probe_concurrency: number
  startup_probe_timeout: string
  routine_probe_timeout: string
  probe_dial_timeout: string
  probe_response_timeout: string
  routine_probe_retries: number
}

export interface ProbeRoundStatus {
  in_flight: boolean
  kind?: 'startup' | 'periodic' | 'manual'
  started_at?: string
  total: number
  completed: number
  success: number
  failed: number
}

export interface ProbeOperationsStatus {
  node_count: number
  concurrency_mode: 'auto' | 'fixed'
  configured_concurrency: number
  effective_concurrency: number
  estimated_startup_worst_case: string
  estimated_startup_worst_seconds: number
  estimated_routine_worst_case: string
  estimated_routine_worst_seconds: number
  round: ProbeRoundStatus
}

// ---- Unlock detection types ----

// A single streaming/AI service unlock result.
export interface UnlockServiceResult {
  name: 'netflix' | 'disney_plus' | 'chatgpt' | 'gemini' | 'claude' | 'youtube' | 'bahamut' | 'tiktok' | 'amazon' | 'reddit'
  display_name: string
  category?: 'streaming' | 'ai' | 'social' | 'custom'
  description?: string
  status: 'unlocked' | 'partial' | 'originals_only' | 'locked' | 'failed'
  region?: string
  detail?: string
}

export interface UnlockProviderMeta {
  value: UnlockServiceResult['name'] | string
  label: string
  description?: string
  category?: 'streaming' | 'ai' | 'social' | 'custom'
  aliases?: string[]
  order: number
}

export interface UnlockStatusMeta {
  value: UnlockServiceResult['status'] | string
  label: string
  short_label?: string
  description?: string
  color?: string
  severity?: string
}

export interface UnlockMetaResponse {
  providers: UnlockProviderMeta[]
  statuses: UnlockStatusMeta[]
}

// Native IP purity info for the node's exit IP.
export interface UnlockIPInfo {
  ip: string
  country?: string
  iso_code?: string
  region?: string
  pure: boolean
  asn?: string
  org?: string
  ip_type?: string
  usage_type?: string
  fraud_score?: number
  risk_level?: string
}

// Full unlock report for one node (matches unlock.Result on the backend).
export interface UnlockResult {
  tag: string
  name: string
  services: UnlockServiceResult[]
  ip: UnlockIPInfo
  error?: string
  duration_ms: number
  // Only present on results loaded from the persisted store
  // (/api/nodes/unlock-results); omitted on live check responses.
  checked_at?: string
}

// Response from GET /api/nodes/unlock-results: last-saved detection per tag.
export interface UnlockResultsResponse {
  results: Record<string, UnlockResult>
}

// SSE events for the batch unlock-all stream.
export interface UnlockSSEStart {
  type: 'start'
  total: number
}

export interface UnlockSSEProgress {
  type: 'progress'
  tag?: string
  name?: string
  status: 'success' | 'error'
  error?: string
  result?: UnlockResult
  current: number
  total: number
  progress: number
}

export interface UnlockSSEComplete {
  type: 'complete'
  total: number
  success: number
  failed: number
}

export type UnlockSSEEvent = UnlockSSEStart | UnlockSSEProgress | UnlockSSEComplete


// ---- SSE Traffic stream types ----

export interface TrafficStreamNode {
  tag: string
  upload_speed: number
  download_speed: number
  total_upload: number
  total_download: number
}

export interface TrafficStreamEvent {
  type: 'traffic'
  node_count: number
  total_upload: number
  total_download: number
  upload_speed: number
  download_speed: number
  sampled_at: string
  nodes: TrafficStreamNode[]
}
