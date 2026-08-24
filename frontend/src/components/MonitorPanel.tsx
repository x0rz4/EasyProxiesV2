import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type { NodeSnapshot, NodesResponse, ConfigNodeConfig, ConfigNodesResponse, TrafficStreamEvent } from '../types'
import { fetchNodes, fetchConfigNodes, streamTraffic } from '../api/client'
import { formatBytes, formatSpeed } from '../utils/format'
import DonutChart from './charts/DonutChart'
import BarChart from './charts/BarChart'
import LatencyRanking from './charts/LatencyRanking'
import TrafficRanking from './charts/TrafficRanking'
import RegionCards from './charts/RegionCards'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'

function latencyColor(ms: number): string {
  if (ms < 0) return 'text-base-content/50'
  if (ms <= 100) return 'text-success'
  if (ms <= 300) return 'text-warning'
  return 'text-error'
}

type AutoRefreshInterval = 0 | 5 | 10 | 30 | 60

export default function MonitorPanel() {
  const [data, setData] = useState<NodesResponse | null>(null)
  const [configData, setConfigData] = useState<ConfigNodesResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [autoRefresh, setAutoRefresh] = useState<AutoRefreshInterval>(5)
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const trafficAbortRef = useRef<AbortController | null>(null)
  const [trafficRealtime, setTrafficRealtime] = useState<{
    connected: boolean
    uploadSpeed: number
    downloadSpeed: number
    sampledAt: string
  }>({
    connected: false,
    uploadSpeed: 0,
    downloadSpeed: 0,
    sampledAt: '',
  })

  const loadData = useCallback(async () => {
    try {
      setError('')
      const [monitorRes, configRes] = await Promise.all([
        fetchNodes(),
        fetchConfigNodes().catch(() => null),
      ])

      setData(monitorRes)
      if (configRes) setConfigData(configRes)
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    const initialLoad = setTimeout(loadData, 0)
    return () => clearTimeout(initialLoad)
  }, [loadData])

  // Auto-refresh timer
  useEffect(() => {
    if (timerRef.current) {
      clearInterval(timerRef.current)
      timerRef.current = null
    }
    if (autoRefresh > 0) {
      timerRef.current = setInterval(() => {
        loadData()
      }, autoRefresh * 1000)
    }
    return () => {
      if (timerRef.current) {
        clearInterval(timerRef.current)
      }
    }
  }, [autoRefresh, loadData])

  const handleRefresh = async () => {
    setLoading(true)
    await loadData()
  }

  // Real-time speed stream (SSE): reconnect automatically on disconnect.
  useEffect(() => {
    let stopped = false
    let retryTimer: ReturnType<typeof setTimeout> | null = null

    const connect = () => {
      if (stopped) return

      if (trafficAbortRef.current) {
        trafficAbortRef.current.abort()
        trafficAbortRef.current = null
      }

      trafficAbortRef.current = streamTraffic(
        (event: TrafficStreamEvent) => {
          setTrafficRealtime({
            connected: true,
            uploadSpeed: event.upload_speed || 0,
            downloadSpeed: event.download_speed || 0,
            sampledAt: event.sampled_at || '',
          })
        },
        () => {
          setTrafficRealtime((prev) => ({ ...prev, connected: false }))
          if (!stopped) {
            retryTimer = setTimeout(connect, 2000)
          }
        },
      )
    }

    connect()

    return () => {
      stopped = true
      if (retryTimer) {
        clearTimeout(retryTimer)
      }
      if (trafficAbortRef.current) {
        trafficAbortRef.current.abort()
        trafficAbortRef.current = null
      }
    }
  }, [])

  // ---- Computed stats ----
  const allNodes = useMemo(() => data?.nodes || [], [data])
  const allConfigNodes = useMemo(() => configData?.nodes || [], [configData])

  // Config-level counts (includes disabled nodes)
  const totalConfigNodes = allConfigNodes.length
  const disabledNodes = useMemo(
    () => allConfigNodes.filter((n: ConfigNodeConfig) => n.disabled).length,
    [allConfigNodes]
  )
  // enabledConfigNodes not directly displayed but used implicitly in monitor node count

  // Runtime monitor counts (only enabled nodes that are loaded in the box)
  const availableNodes = useMemo(
    () => allNodes.filter((n: NodeSnapshot) => n.initial_check_done && n.available && !n.blacklisted).length,
    [allNodes]
  )

  const unavailableNodes = useMemo(
    () => allNodes.filter((n: NodeSnapshot) => n.initial_check_done && !n.available && !n.blacklisted).length,
    [allNodes]
  )

  const blacklistedNodes = useMemo(
    () => allNodes.filter((n: NodeSnapshot) => n.blacklisted).length,
    [allNodes]
  )

  const pendingNodes = useMemo(
    () => allNodes.filter((n: NodeSnapshot) => !n.initial_check_done && !n.blacklisted).length,
    [allNodes]
  )

  const healthRate = useMemo(() => {
    const checked = allNodes.filter((n: NodeSnapshot) => n.initial_check_done).length
    if (checked === 0) return -1
    return Math.round((availableNodes / checked) * 100)
  }, [allNodes, availableNodes])

  const avgLatency = useMemo(() => {
    const validNodes = allNodes.filter((n: NodeSnapshot) => n.last_latency_ms > 0)
    if (validNodes.length === 0) return -1
    return Math.round(validNodes.reduce((sum: number, n: NodeSnapshot) => sum + n.last_latency_ms, 0) / validNodes.length)
  }, [allNodes])

  const totalConnections = useMemo(
    () => allNodes.reduce((sum: number, n: NodeSnapshot) => sum + n.active_connections, 0),
    [allNodes]
  )

  // Traffic totals
  const totalUpload = useMemo(() => data?.total_upload || 0, [data])
  const totalDownload = useMemo(() => data?.total_download || 0, [data])

  // Speed from backend realtime stream, fallback to periodic /api/nodes snapshot values.
  const uploadSpeed = trafficRealtime.connected
    ? trafficRealtime.uploadSpeed
    : (data?.upload_speed || 0)
  const downloadSpeed = trafficRealtime.connected
    ? trafficRealtime.downloadSpeed
    : (data?.download_speed || 0)

  // ---- Chart data ----

  // Status donut - includes all node states
  const statusSegments = useMemo(() => [
    { label: '正常', value: availableNodes, color: 'oklch(0.72 0.19 142)' },
    { label: '不可用', value: unavailableNodes, color: 'oklch(0.63 0.24 29)' },
    { label: '黑名单', value: blacklistedNodes, color: 'oklch(0.55 0.20 15)' },
    { label: '待检查', value: pendingNodes, color: 'oklch(0.80 0.18 84)' },
    { label: '已禁用', value: disabledNodes, color: 'oklch(0.65 0.05 250)' },
  ], [availableNodes, unavailableNodes, blacklistedNodes, pendingNodes, disabledNodes])

  // Latency distribution bars
  const latencyBars = useMemo(() => {
    const buckets = [
      { label: '0-50', min: 0, max: 50, count: 0, color: 'oklch(0.72 0.19 142)' },
      { label: '50-100', min: 50, max: 100, count: 0, color: 'oklch(0.75 0.18 120)' },
      { label: '100-200', min: 100, max: 200, count: 0, color: 'oklch(0.80 0.18 84)' },
      { label: '200-300', min: 200, max: 300, count: 0, color: 'oklch(0.75 0.18 55)' },
      { label: '300+', min: 300, max: Infinity, count: 0, color: 'oklch(0.63 0.24 29)' },
      { label: '超时', min: -1, max: 0, count: 0, color: 'oklch(0.50 0.10 250)' },
    ]
    for (const node of allNodes) {
      const ms = node.last_latency_ms
      if (ms < 0 || !node.initial_check_done) {
        buckets[5].count++
      } else if (ms <= 50) {
        buckets[0].count++
      } else if (ms <= 100) {
        buckets[1].count++
      } else if (ms <= 200) {
        buckets[2].count++
      } else if (ms <= 300) {
        buckets[3].count++
      } else {
        buckets[4].count++
      }
    }
    return buckets.map(b => ({ label: b.label, value: b.count, color: b.color }))
  }, [allNodes])

  // Latency ranking
  const rankingItems = useMemo(() =>
    allNodes
      .filter((n: NodeSnapshot) => n.last_latency_ms > 0)
      .map((n: NodeSnapshot) => ({
        name: n.name || n.tag,
        latency: n.last_latency_ms,
        region: n.region,
      })),
    [allNodes]
  )

  // Traffic ranking
  const trafficRankItems = useMemo(() =>
    allNodes
      .filter((n: NodeSnapshot) => (n.total_upload || 0) + (n.total_download || 0) > 0)
      .map((n: NodeSnapshot) => ({
        name: n.name || n.tag,
        upload: n.total_upload || 0,
        download: n.total_download || 0,
        region: n.region,
      })),
    [allNodes]
  )

  // Region stats
  const regionStats = useMemo(() => {
    const map = new Map<string, { total: number; healthy: number; latencies: number[] }>()
    for (const node of allNodes) {
      const region = node.region || 'other'
      const entry = map.get(region) || { total: 0, healthy: 0, latencies: [] }
      entry.total++
      if (node.initial_check_done && node.available && !node.blacklisted) {
        entry.healthy++
      }
      if (node.last_latency_ms > 0) {
        entry.latencies.push(node.last_latency_ms)
      }
      map.set(region, entry)
    }
    return Array.from(map.entries()).map(([region, stats]) => ({
      region,
      total: stats.total,
      healthy: stats.healthy,
      avgLatency: stats.latencies.length > 0
        ? Math.round(stats.latencies.reduce((a, b) => a + b, 0) / stats.latencies.length)
        : 0,
    }))
  }, [allNodes])

  // ---- Render ----

  if (loading && !data) {
    return (
      <div className="flex items-center justify-center h-64">
        <span className="loading loading-spinner loading-lg text-primary"></span>
      </div>
    )
  }

  return (
    <PageLayout>
      <PageHeader
        title="节点监控"
        description="实时数据仪表盘 · 可视化节点健康与流量状况"
        icon={<svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2.5" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" /></svg>}
        actions={<>
            {/* Auto refresh selector */}
            <div className="flex items-center gap-2 bg-base-200/50 px-2 py-1 lg:py-1.5 rounded-lg border border-base-300/50 transition-colors">
              {autoRefresh > 0 ? (
                <div className="hidden sm:flex items-center gap-1.5 px-2 py-0.5 rounded-md text-xs font-medium text-success whitespace-nowrap">
                  <div className="w-1.5 h-1.5 rounded-full bg-success animate-pulse"></div>
                  <span>自动刷新开启</span>
                </div>
              ) : (
                <span className="hidden sm:inline text-sm font-medium pl-2 whitespace-nowrap text-base-content/70">自动刷新</span>
              )}
              <select
                className="select select-sm border-0 focus:outline-none focus:ring-0 bg-transparent min-w-[90px] text-sm"
                value={autoRefresh}
                onChange={(e) => setAutoRefresh(Number(e.target.value) as AutoRefreshInterval)}
              >
                <option value={0}>关闭</option>
                <option value={5}>每 5 秒</option>
                <option value={10}>每 10 秒</option>
                <option value={30}>每 30 秒</option>
                <option value={60}>每 60 秒</option>
              </select>
            </div>
            <button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" onClick={handleRefresh} disabled={loading} title="刷新监控数据" aria-label="刷新监控数据">
              {loading ? (
                <span className="loading loading-spinner loading-sm"></span>
              ) : (
                <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
              )}
              <span className="hidden sm:inline">刷新</span>
            </button>
          </>}
      />

      <PageContent>
        {/* Error */}
        {error && (
        <div role="alert" className="alert alert-error alert-soft">
          <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M10 14l2-2m0 0l2-2m-2 2l-2-2m2 2l2 2m7-2a9 9 0 11-18 0 9 9 0 0118 0z" /></svg>
          <span>{error}</span>
        </div>
      )}

      {/* Stats Cards */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {/* Total Nodes */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-primary/5 rounded-full blur-2xl group-hover:bg-primary/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-primary/10 flex items-center justify-center text-primary">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M5 12h14M5 12a2 2 0 01-2-2V6a2 2 0 012-2h14a2 2 0 012 2v4a2 2 0 01-2 2M5 12a2 2 0 00-2 2v4a2 2 0 002 2h14a2 2 0 002-2v-4a2 2 0 00-2-2" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">总节点</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-base-content mb-2 relative z-10">{totalConfigNodes}</div>
          <div className="text-xs flex flex-wrap gap-1.5 relative z-10">
            <span className="badge badge-success badge-sm badge-outline border-success/30 bg-success/5 font-medium">可用 {availableNodes}</span>
            {unavailableNodes > 0 && <span className="badge badge-error badge-sm badge-outline border-error/30 bg-error/5 font-medium">不可用 {unavailableNodes}</span>}
            {blacklistedNodes > 0 && <span className="badge badge-error badge-sm badge-outline border-error/30 bg-error/5 font-medium">黑名单 {blacklistedNodes}</span>}
            {pendingNodes > 0 && <span className="badge badge-warning badge-sm badge-outline border-warning/30 bg-warning/5 font-medium">待检查 {pendingNodes}</span>}
            {disabledNodes > 0 && <span className="badge badge-ghost badge-sm bg-base-200/50 font-medium text-base-content/50">禁用 {disabledNodes}</span>}
          </div>
        </div>

        {/* Health Rate */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-success/5 rounded-full blur-2xl group-hover:bg-success/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-success/10 flex items-center justify-center text-success">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">健康率</div>
          </div>
          <div className={`text-3xl font-black tabular-nums tracking-tight mb-2 relative z-10 ${
            healthRate < 0 ? 'text-base-content/30' :
            healthRate >= 80 ? 'text-success' :
            healthRate >= 50 ? 'text-warning' : 'text-error'
          }`}>
            {healthRate >= 0 ? `${healthRate}%` : '-'}
          </div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">
            {healthRate >= 0 ? `${availableNodes} / ${availableNodes + unavailableNodes} 已检查` : '等待检查'}
          </div>
        </div>

        {/* Average Latency */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-warning/5 rounded-full blur-2xl group-hover:bg-warning/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-warning/10 flex items-center justify-center text-warning">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13 10V3L4 14h7v7l9-11h-7z" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">平均延迟</div>
          </div>
          <div className={`text-3xl font-black tabular-nums tracking-tight mb-2 relative z-10 ${latencyColor(avgLatency)}`}>
            {avgLatency > 0 ? `${avgLatency}ms` : '-'}
          </div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">
            {allNodes.filter(n => n.last_latency_ms > 0).length} 个节点有数据
          </div>
        </div>

        {/* Active Connections */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-info/5 rounded-full blur-2xl group-hover:bg-info/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-info/10 flex items-center justify-center text-info">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M8 7h12m0 0l-4-4m4 4l-4 4m0 6H4m0 0l4 4m-4-4l4-4" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">活跃连接</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-info mb-2 relative z-10">{totalConnections}</div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">
            {allNodes.filter(n => n.active_connections > 0).length} 个节点活跃
          </div>
        </div>

        {/* Upload Speed */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-blue-500/5 rounded-full blur-2xl group-hover:bg-blue-500/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-blue-500/10 flex items-center justify-center text-blue-500">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M7 11l5-5m0 0l5 5m-5-5v12" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">上传速度</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-blue-500 mb-2 relative z-10">{formatSpeed(uploadSpeed)}</div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">
            {trafficRealtime.connected ? '实时流数据' : '轮询数据'}
          </div>
        </div>

        {/* Download Speed */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-emerald-500/5 rounded-full blur-2xl group-hover:bg-emerald-500/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-emerald-500/10 flex items-center justify-center text-emerald-500">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M17 13l-5 5m0 0l-5-5m5 5V6" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">下载速度</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-emerald-500 mb-2 relative z-10">{formatSpeed(downloadSpeed)}</div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">
            {trafficRealtime.connected ? '实时流数据' : '轮询数据'}
          </div>
        </div>

        {/* Upload Traffic */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-blue-500/5 rounded-full blur-2xl group-hover:bg-blue-500/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-blue-500/10 flex items-center justify-center text-blue-500">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M3 15a4 4 0 004 4h9a5 5 0 10-.1-9.999 5.002 5.002 0 10-9.78 2.096A4.001 4.001 0 003 15z" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">总上传</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-blue-500 mb-2 relative z-10">{formatBytes(totalUpload)}</div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">累计流量统计</div>
        </div>

        {/* Download Traffic */}
        <div className="rounded-2xl bg-base-100 border border-base-300/50 p-5 shadow-sm hover:shadow-md transition-all group overflow-hidden relative">
          <div className="absolute -right-4 -top-4 w-24 h-24 bg-emerald-500/5 rounded-full blur-2xl group-hover:bg-emerald-500/10 transition-colors"></div>
          <div className="flex items-center gap-3 mb-2 relative z-10">
            <div className="w-8 h-8 rounded-lg bg-emerald-500/10 flex items-center justify-center text-emerald-500">
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M3 15a4 4 0 004 4h9a5 5 0 10-.1-9.999 5.002 5.002 0 10-9.78 2.096A4.001 4.001 0 003 15z" /></svg>
            </div>
            <div className="text-sm font-medium text-base-content/60">总下载</div>
          </div>
          <div className="text-3xl font-black tabular-nums tracking-tight text-emerald-500 mb-2 relative z-10">{formatBytes(totalDownload)}</div>
          <div className="text-xs font-medium text-base-content/40 relative z-10 bg-base-200/50 w-fit px-2 py-1 rounded-md">累计流量统计</div>
        </div>
      </div>

      {/* Charts Row: Donut + Bar */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Status Distribution Donut */}
        <div className={`${surfaceClass} p-5 lg:p-6`}>
          <h3 className="text-sm font-semibold mb-4 flex items-center gap-2">
            <span className="w-1 h-4 bg-primary rounded-full"></span>
            状态分布
          </h3>
          <div className="flex justify-center">
            <DonutChart
              segments={statusSegments}
              size={180}
              strokeWidth={32}
              centerValue={healthRate >= 0 ? `${healthRate}%` : '-'}
              centerLabel="健康率"
            />
          </div>
        </div>

        {/* Latency Distribution Bar */}
        <div className={`${surfaceClass} p-5 lg:p-6`}>
          <BarChart
            bars={latencyBars}
            maxHeight={130}
            title="延迟分布"
          />
        </div>
      </div>

      {/* Region Cards */}
      <div className={`${surfaceClass} p-5 lg:p-6`}>
        <RegionCards
          regions={regionStats}
          title="地区统计"
        />
      </div>

      {/* Rankings Row: Latency + Traffic */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Latency Ranking */}
        <div className={`${surfaceClass} p-5 lg:p-6`}>
          <LatencyRanking
            items={rankingItems}
            maxItems={10}
            title="延迟排行 TOP 10"
          />
        </div>

        {/* Traffic Ranking */}
        <div className={`${surfaceClass} p-5 lg:p-6`}>
          <TrafficRanking
            items={trafficRankItems}
            maxItems={10}
            title="流量排行 TOP 10"
          />
        </div>
      </div>

      {/* Footer info */}
      <div className="text-center text-xs text-base-content/30">
        {data && (
          <>
            共 {totalConfigNodes} 个节点 ·
            {Object.keys(data.region_stats || {}).length} 个地区 ·
            数据来自运行时监控
            {autoRefresh > 0 && ` · 每 ${autoRefresh} 秒自动刷新`}
            {trafficRealtime.connected ? ' · 速度流已连接' : ' · 速度流重连中'}
          </>
        )}
      </div>
      </PageContent>
    </PageLayout>
  )
}
