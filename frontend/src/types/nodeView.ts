import type { TagCondition } from './index'

export interface NodeFilterSpec {
  diagnostic?: string
  version: number
  text?: string
  regions?: string[]
  exclude_regions?: string[]
  include_unknown_region?: boolean
  tag_whitelist?: number[]
  tag_blacklist?: number[]
  tag_match?: 'any' | 'all'
  status?: string[]
  max_latency_ms?: number
  explicit_ids?: number[]
  excluded_ids?: number[]
  sources?: string[]
  protocols?: string[]
  subscription_ids?: number[]
  unsubscribed?: boolean
  runtime_only?: boolean
  condition?: TagCondition
}
export interface NodeViewItem {
  id: number
  name: string
  region: string
  country: string
  tags: string[]
  source: string
  protocol: string
  subscription_ids: number[]
  status: string
  latency_ms: number
  runtime_tag?: string
  healthy: boolean
  enabled: boolean
}
export interface NodeViewPage {
  offset: number
  limit: number
  sort: 'id' | 'name' | 'region' | 'status' | 'latency_ms' | 'port' | 'source' | 'speed_bps' | 'fraud_score' | 'diagnostic_latency'
  descending: boolean
}
export interface NodeViewResult {
  items: NodeViewItem[]
  total: number
  page: NodeViewPage
  snapshot_revision: string
}
export interface NodeFacet { value: string; label: string; count: number; healthy: number; avg_latency_ms: number }
export interface NodeFacets {
  diagnostics: NodeFacet[]
  regions: NodeFacet[]
  tags: NodeFacet[]
  statuses: NodeFacet[]
  sources: NodeFacet[]
  protocols: NodeFacet[]
  total: number
  snapshot_revision: string
}
export interface SavedNodeFilter {
  id: number
  name: string
  revision: number
  updated_at: string
  spec: NodeFilterSpec
}
export interface GroupPreview {
  candidates: (NodeViewItem & { decision: { allowed: boolean; reason: string } })[]
  matched_ids: number[]
  current_ids: number[]
  added_ids: number[]
  removed_ids: number[]
  rule_added_ids: number[]
  rule_removed_ids: number[]
  region_rule_version: number
  snapshot_revision: string
  group_updated_at: string
}
