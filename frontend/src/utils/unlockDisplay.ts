import type { NodeCheckResultItem, NodeSnapshot, UnlockResult } from '../types'

export interface DisplayLatency {
  value: number | null
  source: '综合检测' | '健康探测' | null
}

export interface UnlockNetworkInfo {
  countryCode: string
  country: string
  asn: string
  organization: string
}

export function displayLatency(node: NodeSnapshot, diagnostic?: NodeCheckResultItem | null): DisplayLatency {
  const diagnosticLatency = diagnostic?.detection?.latency_ms
  if (diagnosticLatency != null) return { value: diagnosticLatency, source: '综合检测' }
  if (node.last_latency_ms >= 0) return { value: node.last_latency_ms, source: '健康探测' }
  return { value: null, source: null }
}

export function unlockNetworkInfo(diagnostic?: NodeCheckResultItem | null, result?: UnlockResult | null): UnlockNetworkInfo {
  const quality = diagnostic?.quality || []
  const hasMetadata = (item: NodeCheckResultItem['quality'][number]) => Boolean(item.country_code || item.country || item.asn || item.org || item.isp)
  const metadata = quality.find((item) => item.provider === 'ip-api' && hasMetadata(item)) || quality.find(hasMetadata)
  return {
    countryCode: (diagnostic?.detection?.exit_country_code || metadata?.country_code || result?.ip.iso_code || '').toUpperCase(),
    country: diagnostic?.detection?.exit_country || metadata?.country || result?.ip.country || '',
    asn: metadata?.asn || result?.ip.asn || '',
    organization: metadata?.org || metadata?.isp || result?.ip.org || '',
  }
}
