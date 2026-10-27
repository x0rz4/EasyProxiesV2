import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type {
  ConfigNodeConfig,
  NodeSnapshot,
  UnlockResult,
  NodeCheckResultItem,
  NodeCheckSettings,
  NodeCheckStages,
  NodeCheckTask,
  NodeCheckEvent,
  Subscription,
} from '../types'
import { queryNodeIDs, cancelNodeCheckTask, createNodeCheckTask, fetchConfigNodes, fetchNodeCheckResults, fetchNodeCheckSettings, fetchNodeCheckTasks, fetchNodes, fetchUnlockResults, listSubscriptions, streamNodeCheckTask, unlockNode, updateNodeCheckSettings } from '../api/client'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useNodeFilter } from '../hooks/useNodeFilter'
import useModalFocus from '../hooks/useModalFocus'
import type { NodeViewItem } from '../types/nodeView'
import NodeFilterBar from './NodeFilterBar'
import { regionFlag } from '../utils/region'
import { displayLatency, unlockNetworkInfo } from '../utils/unlockDisplay'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import UnlockDrawer from './UnlockDrawer'
import {
  ShieldCheck, X, Play, RefreshCw, ArrowUpDown, ArrowUp, ArrowDown,
  XCircle, RotateCcw,
  Globe2,
  Gauge, Settings2, Wifi, Database, Save,
} from 'lucide-react'
import { toast } from 'sonner'
import { cn } from '../utils/cn'

type StatusFilterType = '' | 'unlocked' | 'residential' | 'broadcast' | 'proxy' | 'quality_success' | 'quality_failed' | 'speed_fast' | 'speed_failed' | 'fraud_high' | 'locked' | 'mixed' | 'failed' | 'untested'
type DiagnosticSortKey = 'name' | 'latency' | 'speed' | 'fraud'

function sourceLabel(source?: string) {
  switch (source) {
    case 'inline': return '配置文件'
    case 'nodes_file': return '节点文件'
    case 'subscription': return '订阅'
    case 'manual': return '手动添加'
    default: return source || '-'
  }
}

function nodeSourceLabel(config: ConfigNodeConfig | undefined, subscriptionNames: ReadonlyMap<number, string>) {
  const names = (config?.subscription_ids || [])
    .map((id) => subscriptionNames.get(id))
    .filter((name): name is string => Boolean(name))
  if (names.length > 0) return `订阅 · ${names.join('、')}`
  return config?.source ? sourceLabel(config.source) : ''
}

const unlockBadgeClasses: Record<string, string> = {
  netflix: 'bg-[#E50914] text-white',
  disney_plus: 'bg-[#113CCF] text-white',
  chatgpt: 'bg-[#10A37F] text-white',
  gemini: 'bg-[#4285F4] text-white',
  claude: 'bg-[#D97757] text-white',
  youtube: 'bg-[#FF0000] text-white',
  bahamut: 'bg-[#00A0E9] text-white',
  amazon: 'bg-[#00A8E1] text-white',
  tiktok: 'bg-neutral-900 text-white',
}

// normalizeUnlockResults re-keys the persisted unlock payload by numeric node
// ID. The transport keys are strings because JSON object keys always are; the
// embedded node_id is preferred so the map survives a key format change.
function normalizeUnlockResults(raw: Record<string, UnlockResult>): Record<number, UnlockResult> {
  const out: Record<number, UnlockResult> = {}
  for (const [key, value] of Object.entries(raw)) {
    const nodeID = value.node_id ?? Number(key)
    if (Number.isFinite(nodeID) && nodeID > 0) out[nodeID] = value
  }
  return out
}

export default function UnlockPanel() {
  const queryClient = useQueryClient()
  const nodeFilter = useNodeFilter({ version: 2 }, true, { runtimeOnly: true, healthyOnly: true })
  const [preparingTask, setPreparingTask] = useState(false)
  const [subscriptions, setSubscriptions] = useState<Subscription[]>([])
  const [metadataLoading, setMetadataLoading] = useState(true)
  const pageIDs = useMemo(() => (nodeFilter.query.data?.items || []).map(node => node.id), [nodeFilter.query.data?.items])
  const pageDetails = useQuery({
    queryKey: ['nodeDiagnosticPage', pageIDs],
    enabled: nodeFilter.query.isSuccess,
    queryFn: async ({ signal }) => {
      const [runtime, config, unlock, checks] = await Promise.all([
        fetchNodes(pageIDs, signal), fetchConfigNodes(undefined, pageIDs, signal),
        fetchUnlockResults(pageIDs, signal), fetchNodeCheckResults(pageIDs, signal),
      ])
      return { nodes: runtime.nodes, configNodes: config.nodes, unlock: normalizeUnlockResults(unlock.results),
        diagnostics: Object.fromEntries(checks.results.map(item => [item.node_id, item])) }
    },
    refetchInterval: 10_000,
  })
  const nodes = useMemo(() => pageDetails.data?.nodes || [], [pageDetails.data?.nodes])
  const configNodes = useMemo(() => pageDetails.data?.configNodes || [], [pageDetails.data?.configNodes])
  const loading = metadataLoading || nodeFilter.isDebouncing || nodeFilter.query.isLoading || pageDetails.isLoading

  // node_id -> result. Results are kept across batch runs and single runs alike.
  // Keyed by the stable node ID, never by the runtime tag: tags carry a
  // generation suffix and are regenerated on every reload, which used to strand
  // persisted results and render the node as 未检测.
  const [singleResults, setResults] = useState<Record<number, UnlockResult>>({})
  const results = { ...pageDetails.data?.unlock, ...singleResults }
  const [errors, setErrors] = useState<Record<number, string>>({})
  // Per-node in-flight state, keyed by node ID for the same reason.
  const [checking, setChecking] = useState<Record<number, boolean>>({})
  const diagnostics: Record<number, NodeCheckResultItem> = pageDetails.data?.diagnostics || {}
  const [checkSettings, setCheckSettings] = useState<NodeCheckSettings | null>(null)
  const [selectedIds, setSelectedIds] = useState<Set<number>>(new Set())
  const [checkDialogOpen, setCheckDialogOpen] = useState(false)
  const [checkStages, setCheckStages] = useState<NodeCheckStages>({ latency: true, speed: true, quality: true, unlock: true })
  const [checkScope, setCheckScope] = useState<'selected' | 'filtered'>('filtered')
  const [activeTask, setActiveTask] = useState<NodeCheckTask | null>(null)
  const [taskHistory, setTaskHistory] = useState<NodeCheckTask[]>([])
  const taskAbortRef = useRef<AbortController | null>(null)

  // Drawer state
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [selectedNodeID, setSelectedNodeID] = useState<number | null>(null)
  const selectedNode = nodes.find(node => node.node_id === selectedNodeID) || null

  // Batch progress
  const [batchRunning, setBatchRunning] = useState(false)
  const [batchProgress, setBatchProgress] = useState({ current: 0, total: 0 })
  const batchAbortRef = useRef<AbortController | null>(null)

  const statusFilter = (nodeFilter.spec.diagnostic || '') as StatusFilterType
  const setStatusFilter = (diagnostic: StatusFilterType) => nodeFilter.setSpec({ ...nodeFilter.spec, diagnostic })
  const sortNames = { name: 'name', latency: 'diagnostic_latency', speed: 'speed_bps', fraud: 'fraud_score' } as const
  const sortKey = (Object.keys(sortNames) as DiagnosticSortKey[]).find(key => sortNames[key] === nodeFilter.page.sort)
  const sortDir = nodeFilter.page.descending ? 'desc' : 'asc'
  const handleSort = (key: DiagnosticSortKey) => nodeFilter.setPage({ ...nodeFilter.page,
    sort: sortNames[key], descending: sortKey === key ? !nodeFilter.page.descending : false, offset: 0 })

  // ---- Node list ----
  // The runtime node list is refetched whenever detection data is refreshed, so
  // nodes that only finished their initial health check after this panel mounted
  // stop being permanently invisible here.
  const loadNodeList = useCallback(async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['nodeDiagnosticPage'] }),
      queryClient.invalidateQueries({ queryKey: ['nodeQuery'] }),
      queryClient.invalidateQueries({ queryKey: ['nodeFacets'] }),
    ])
  }, [queryClient])

  const loadNodes = useCallback(async () => {
    try {
      await loadNodeList()
      try {
        const [subscriptionRes, settings, tasks] = await Promise.all([listSubscriptions(), fetchNodeCheckSettings(), fetchNodeCheckTasks()])
        setSubscriptions(subscriptionRes.subscriptions || [])
        setCheckSettings(settings)
        setCheckStages((current) => ({ ...current, quality: settings.ippure_enabled || settings.ip_api_enabled }))
        const running = tasks.tasks.find((task) => task.status === 'pending' || task.status === 'running')
        setTaskHistory(tasks.tasks)
        if (running) setActiveTask(running)
      } catch {
        /* diagnostics are optional while migrating older databases */
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '加载节点失败')
    } finally {
      setMetadataLoading(false)
    }
  }, [loadNodeList])

  useEffect(() => {
    const t = setTimeout(() => void loadNodes(), 0)
    return () => clearTimeout(t)
  }, [loadNodes])

  useEffect(() => {
    return () => {
      // Abort any in-flight batch on unmount.
      batchAbortRef.current?.abort()
      taskAbortRef.current?.abort()
    }
  }, [])

  // ---- Single-node unlock ----
  const runSingle = useCallback(async (node: NodeSnapshot) => {
    const nodeID = node.node_id
    setChecking((s) => ({ ...s, [nodeID]: true }))
    setErrors((s) => {
      const next = { ...s }
      delete next[nodeID]
      return next
    })
    try {
      const res = await unlockNode(node.tag)
      setResults((s) => ({ ...s, [nodeID]: res }))
      toast.success(`节点 ${node.name || node.tag} 检测完成`)
    } catch (err) {
      setErrors((s) => ({ ...s, [nodeID]: err instanceof Error ? err.message : '检测失败' }))
    } finally {
      setChecking((s) => {
        const next = { ...s }
        delete next[nodeID]
        return next
      })
    }
  }, [])

  const refreshCheckResults = useCallback(async () => {
    // Refresh the runtime node list alongside the results: a reload during the
    // task may have rebuilt nodes under new runtime tags, and newly available
    // nodes should appear without a manual refresh.
    await loadNodeList().catch(() => undefined)
    const tasks = await fetchNodeCheckTasks()
    setResults({})
    setTaskHistory(tasks.tasks)
  }, [loadNodeList])

  const connectTask = useCallback((task: NodeCheckTask) => {
    taskAbortRef.current?.abort()
    setActiveTask(task)
    setBatchRunning(true)
    setBatchProgress({ current: 0, total: task.total_nodes })
    taskAbortRef.current = streamNodeCheckTask(task.id, (event: NodeCheckEvent) => {
      if (event.task) {
        setActiveTask(event.task)
        const completed = Object.values(event.task.stats).reduce((sum, stage) => sum + stage.completed, 0)
        const total = Object.values(event.task.stats).reduce((sum, stage) => sum + stage.total, 0)
        setBatchProgress({ current: completed, total })
      }
      if (event.type === 'result' && event.node_id) {
        const nodeID = event.node_id
        setChecking((current) => { const next = { ...current }; delete next[nodeID]; return next })
        // A failed unlock stage persists no result row, so without recording the
        // error here the node would fall back to 未检测 instead of 解锁失败.
        if (event.phase === 'unlock') {
          setErrors((current) => {
            const next = { ...current }
            if (event.status === 'failed') next[nodeID] = event.error || '解锁检测失败'
            else delete next[nodeID]
            return next
          })
        }
      }
      if (event.type === 'done') {
        taskAbortRef.current = null
        setBatchRunning(false)
        setChecking({})
        void refreshCheckResults()
        toast.success(event.task?.status === 'cancelled' ? '综合检测已取消' : '综合检测完成')
      }
    }, (error) => {
      taskAbortRef.current = null
      setBatchRunning(false)
      toast.error(error.message)
    })
  }, [refreshCheckResults])

  useEffect(() => {
    if (activeTask && (activeTask.status === 'pending' || activeTask.status === 'running') && !taskAbortRef.current) connectTask(activeTask)
  }, [activeTask, connectTask])

  const launchTask = useCallback(async (nodeIds: number[], stages: NodeCheckStages, settings?: NodeCheckSettings) => {
    if (nodeIds.length === 0) { toast.error('请选择至少一个节点'); return }
    try {
      const task = await createNodeCheckTask(nodeIds, stages, settings)
      const checkingMap: Record<number, boolean> = {}
      nodeIds.forEach((nodeID) => { checkingMap[nodeID] = true })
      setChecking(checkingMap)
      // Drop stale per-node errors for the nodes about to be re-checked.
      setErrors((current) => {
        const next = { ...current }
        nodeIds.forEach((nodeID) => { delete next[nodeID] })
        return next
      })
      setCheckDialogOpen(false)
      connectTask(task)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '创建检测任务失败')
    }
  }, [connectTask])

  // Quick unlock uses the same task scheduler as comprehensive diagnostics.
  const runFilteredTask = async (stages: NodeCheckStages, settings?: NodeCheckSettings) => {
    if (preparingTask) return
    setPreparingTask(true)
    try {
      const result = await queryNodeIDs(nodeFilter.querySpec)
      await launchTask(result.ids, stages, settings)
    } catch (error) { toast.error(error instanceof Error ? error.message : '获取筛选节点失败') }
    finally { setPreparingTask(false) }
  }
  const runBatch = () => void runFilteredTask({ latency: false, speed: false, quality: false, unlock: true })

  const runSelectedTask = async () => {
    if (preparingTask) return
    setPreparingTask(true)
    try {
      const { ids } = await queryNodeIDs({ version: 2, runtime_only: true, healthy_only: true })
      const available = new Set(ids)
      const targets = [...selectedIds].filter(id => available.has(id))
      if (targets.length !== selectedIds.size) {
        setSelectedIds(new Set(targets))
        toast.info(`已跳过 ${selectedIds.size - targets.length} 个不再在线可用的节点`)
      }
      if (!targets.length) { toast.error('已选节点中没有在线可用节点'); return }
      await launchTask(targets, checkStages, checkSettings || undefined)
    } catch (error) { toast.error(error instanceof Error ? error.message : '获取在线可用节点失败') }
    finally { setPreparingTask(false) }
  }

  const stopBatch = useCallback(() => {
    if (activeTask) void cancelNodeCheckTask(activeTask.id)
    taskAbortRef.current?.abort()
    taskAbortRef.current = null
    batchAbortRef.current?.abort()
    batchAbortRef.current = null
    setBatchRunning(false)
    setChecking({})
    setActiveTask((current) => current ? { ...current, status: 'cancelled' } : current)
  }, [activeTask])

  // Diagnostic counts come from the same fact predicates as the query.
  const diagnosticCount = (name: string) => nodeFilter.facets.isError || !nodeFilter.facets.data
    ? undefined : nodeFilter.facets.data.diagnostics.find(item => item.value === name)?.count || 0
  const availableTotal = nodeFilter.facets.data?.total
  const matchedTotal = nodeFilter.query.data?.total

  const nodeConfigByID = useMemo(() => new Map(
    configNodes.flatMap(node => node.id ? [[node.id, node] as const] : []),
  ), [configNodes])

  const subscriptionNamesByID = useMemo(
    () => new Map(subscriptions.map((subscription) => [subscription.id, subscription.name])),
    [subscriptions],
  )

  const sortedNodes = useMemo(() => {
    const byID = new Map(nodes.map(node => [node.node_id, node]))
    return (nodeFilter.query.data?.items || []).map(item => ({ ...item, runtime: byID.get(item.id) }))
  }, [nodes, nodeFilter.query.data])

  // A periodic refresh can remove the last page while its details are in flight.
  const { page, setPage, query } = nodeFilter
  useEffect(() => {
    if (!query.isSuccess || query.isFetching) return
    const lastOffset = Math.max(0, Math.ceil(query.data.total / page.limit) - 1) * page.limit
    if (page.offset > lastOffset) setPage(current => ({ ...current, offset: lastOffset }))
  }, [page.limit, page.offset, query.data, query.isFetching, query.isSuccess, setPage])

  const progressPct =
    batchProgress.total > 0
      ? Math.round((batchProgress.current / batchProgress.total) * 100)
      : 0

  const openDrawer = (node: NodeSnapshot) => {
    setSelectedNodeID(node.node_id)
    setDrawerOpen(true)
  }

  const isFilterActive = Object.keys(nodeFilter.spec).some(key => key !== 'version')
  const resetFilters = () => nodeFilter.setSpec({ version: 2 })
  const allFilteredSelected = sortedNodes.length > 0 && sortedNodes.every((node) => selectedIds.has(node.id))
  const toggleAllFiltered = () => setSelectedIds((current) => {
    const next = new Set(current)
    if (allFilteredSelected) sortedNodes.forEach((node) => next.delete(node.id))
    else sortedNodes.forEach((node) => next.add(node.id))
    return next
  })
  const selectedTargets = [...selectedIds]

  return (
    <PageLayout>
      <PageHeader
        title="解锁检测"
        description="手动检测节点延迟、下载速度、独立 IP 质量来源与流媒体解锁；结果不影响路由健康"
        icon={<ShieldCheck className="h-5 w-5" />}
        actions={
          <div className="flex items-center gap-2">
            {batchRunning ? (
              <button className="btn btn-error btn-sm gap-2 lg:btn-md" onClick={stopBatch} aria-label="停止检测" title="停止检测">
                <X className="h-4 w-4" />
                <span className="hidden sm:inline">停止检测</span>
              </button>
            ) : (
              <>
                <button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" onClick={() => setCheckDialogOpen(true)} disabled={loading || nodeFilter.query.isError || !matchedTotal} aria-label="综合检测" title="综合检测">
                  <Gauge className="h-4 w-4" /><span className="hidden sm:inline">综合检测</span>
                </button>
                <button className="btn btn-ghost btn-sm gap-2 border border-base-300 lg:btn-md" onClick={runBatch} disabled={preparingTask || loading || nodeFilter.query.isError || !matchedTotal} aria-label="快速解锁" title="快速解锁">
                  <Play className="h-4 w-4" /><span className="hidden sm:inline">快速解锁</span>
                </button>
              </>
            )}
            <button
              className="btn btn-ghost btn-sm gap-2 border border-base-300 shadow-sm lg:btn-md"
              onClick={() => void loadNodes()}
              disabled={loading}
              title="重新加载在线可用节点列表"
              aria-label="刷新节点"
            >
              <RefreshCw className={cn('h-4 w-4', loading && 'animate-spin')} />
              <span className="hidden sm:inline">刷新节点</span>
            </button>
          </div>
        }
      />

      <PageContent>
        {(pageDetails.error || nodeFilter.query.error) && <div role="alert" className="alert alert-error mb-4 text-sm">{(pageDetails.error || nodeFilter.query.error)?.message}</div>}
        {/* ---- Batch progress banner ---- */}
        {batchRunning && (
          <div className={cn(surfaceClass, 'mb-5 shrink-0 border-primary/30 bg-primary/5 p-4 sm:p-5')}>
            <div className="mb-2.5 flex items-center justify-between">
              <div className="flex items-center gap-2">
                <span className="relative flex h-2.5 w-2.5">
                  <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary opacity-75" />
                  <span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary" />
                </span>
                <span className="text-sm font-bold text-primary">综合检测任务运行中</span>
              </div>
              <div className="flex items-center gap-3 font-mono text-xs text-base-content/60">
                <span>{batchProgress.current} / {batchProgress.total} 节点</span>
                <span className="font-bold text-primary">{progressPct}%</span>
              </div>
            </div>
            <div className="relative h-2 w-full overflow-hidden rounded-full bg-base-300/60">
              <div
                className="h-full bg-primary transition-all duration-300 ease-out"
                style={{ width: `${progressPct}%` }}
              />
            </div>
            {activeTask && (
              <div className="mt-3 flex flex-wrap gap-2 text-xs" aria-live="polite">
                {Object.entries(activeTask.stats).map(([phase, stat]) => (
                  <span key={phase} className="badge badge-ghost gap-1.5">
                    {phaseLabel(phase)} {stat.completed}/{stat.total}
                    <span className="text-success">{stat.success}</span>
                    {stat.failed > 0 && <span className="text-error">{stat.failed}</span>}
                    {stat.skipped > 0 && <span className="text-base-content/45">跳过 {stat.skipped}</span>}
                  </span>
                ))}
                <span className="badge badge-outline">流量 {formatBytes(activeTask.downloaded_bytes)}</span>
              </div>
            )}
          </div>
        )}

        {taskHistory.length > 0 && !batchRunning && (
          <details className={cn(surfaceClass, 'mb-5 shrink-0 overflow-hidden')}>
            <summary className="cursor-pointer px-4 py-3 text-sm font-semibold transition-colors hover:bg-base-200/50">最近检测任务（{taskHistory.length}）</summary>
            <div className="max-h-64 overflow-auto border-t border-base-200">
              <table className="table table-sm"><thead><tr><th>时间</th><th>状态</th><th>节点</th><th>流量</th><th>阶段</th></tr></thead><tbody>{taskHistory.map((task) => <tr key={task.id}><td className="text-xs">{new Date(task.created_at).toLocaleString('zh-CN')}</td><td><div className="flex min-w-32 flex-col items-start gap-1"><span className={cn('badge badge-sm', task.status === 'completed' ? 'badge-success' : task.status === 'running' ? 'badge-warning' : task.status === 'cancelled' || task.status === 'interrupted' ? 'badge-ghost' : 'badge-error')}>{taskStatusLabel(task.status)}</span>{task.error && <span className="max-w-56 text-xs text-base-content/55" title={task.error}>{task.error}</span>}</div></td><td className="font-mono text-xs">{task.completed_nodes}/{task.total_nodes}</td><td className="font-mono text-xs">{formatBytes(task.downloaded_bytes)}</td><td className="text-xs">{Object.entries(task.stages).filter(([, enabled]) => enabled).map(([phase]) => phaseLabel(phase)).join('、')}</td></tr>)}</tbody></table>
            </div>
          </details>
        )}

        {/* ---- Redesigned Search & Filter Toolbar ---- */}
        <section className={cn(surfaceClass, 'mb-5 shrink-0 p-4 sm:p-5')}>
          <div className="flex flex-col gap-4">
            <NodeFilterBar filter={nodeFilter} countDisplay="healthy" />

            {/* Row 2: Status Filter Chips & Result Counter */}
            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-base-200 pt-3.5">
              {/* Segmented Filter Pills */}
              <div className="flex flex-wrap items-center gap-1.5 sm:gap-2">
                <FilterChip
                  active={statusFilter === ''}
                  onClick={() => setStatusFilter('')}
                  label="全部"
                  count={nodeFilter.facets.isError ? undefined : availableTotal}
                />
                <FilterChip
                  active={statusFilter === 'residential'}
                  onClick={() => setStatusFilter(statusFilter === 'residential' ? '' : 'residential')}
                  label="住宅 IP"
                  count={diagnosticCount('residential')}
                  tone="cyan"
                />
                <FilterChip
                  active={statusFilter === 'broadcast'}
                  onClick={() => setStatusFilter(statusFilter === 'broadcast' ? '' : 'broadcast')}
                  label="广播 IP"
                  count={diagnosticCount('broadcast')}
                  tone="warning"
                />
                <FilterChip
                  active={statusFilter === 'proxy'}
                  onClick={() => setStatusFilter(statusFilter === 'proxy' ? '' : 'proxy')}
                  label="代理 / 托管"
                  count={diagnosticCount('proxy')}
                  tone="warning"
                />
                <FilterChip
                  active={statusFilter === 'quality_success'}
                  onClick={() => setStatusFilter(statusFilter === 'quality_success' ? '' : 'quality_success')}
                  label="质量已测"
                  count={diagnosticCount('quality_success')}
                  tone="success"
                />
                <FilterChip
                  active={statusFilter === 'quality_failed'}
                  onClick={() => setStatusFilter(statusFilter === 'quality_failed' ? '' : 'quality_failed')}
                  label="质量失败"
                  count={diagnosticCount('quality_failed')}
                  tone="error"
                />
                <FilterChip
                  active={statusFilter === 'speed_fast'}
                  onClick={() => setStatusFilter(statusFilter === 'speed_fast' ? '' : 'speed_fast')}
                  label="≥5 MB/s"
                  count={diagnosticCount('speed_fast')}
                  tone="success"
                />
                <FilterChip
                  active={statusFilter === 'speed_failed'}
                  onClick={() => setStatusFilter(statusFilter === 'speed_failed' ? '' : 'speed_failed')}
                  label="测速失败"
                  count={diagnosticCount('speed_failed')}
                  tone="error"
                />
                <FilterChip
                  active={statusFilter === 'fraud_high'}
                  onClick={() => setStatusFilter(statusFilter === 'fraud_high' ? '' : 'fraud_high')}
                  label="欺诈分 ≥71"
                  count={diagnosticCount('fraud_high')}
                  tone="error"
                />
                <FilterChip
                  active={statusFilter === 'unlocked'}
                  onClick={() => setStatusFilter(statusFilter === 'unlocked' ? '' : 'unlocked')}
                  label="全解锁"
                  count={diagnosticCount('unlocked')}
                  tone="success"
                />
                <FilterChip
                  active={statusFilter === 'locked'}
                  onClick={() => setStatusFilter(statusFilter === 'locked' ? '' : 'locked')}
                  label="受限 / 部分"
                  count={diagnosticCount('locked')}
                  tone="warning"
                />
                <FilterChip
                  active={statusFilter === 'failed'}
                  onClick={() => setStatusFilter(statusFilter === 'failed' ? '' : 'failed')}
                  label="解锁失败"
                  count={diagnosticCount('failed')}
                  tone="error"
                />
                {(diagnosticCount('untested') ?? 0) > 0 && (
                  <FilterChip
                    active={statusFilter === 'untested'}
                    onClick={() => setStatusFilter(statusFilter === 'untested' ? '' : 'untested')}
                    label="未检测"
                    count={diagnosticCount('untested')}
                    tone="ghost"
                  />
                )}
              </div>

              {/* Status information & Reset */}
              <div className="flex items-center gap-3 text-xs text-base-content/55">
                {isFilterActive && (
                  <button
                    type="button"
                    className="flex cursor-pointer items-center gap-1 font-semibold text-primary transition-colors hover:underline"
                    onClick={resetFilters}
                  >
                    <RotateCcw className="h-3 w-3" />
                    重置筛选
                  </button>
                )}
              </div>
            </div>
          </div>
        </section>

        {/* ---- Results Table ---- */}
        <section className={cn(surfaceClass, 'overflow-hidden')}>
          <div className="max-h-[calc(100vh-280px)] min-h-[400px] overflow-auto">
            <table className="table table-sm w-full relative">
              <thead className="sticky top-0 z-10 border-b border-base-200 bg-base-100 text-xs font-semibold uppercase tracking-wider text-base-content/60 backdrop-blur-md">
                <tr>
                  <th className="w-12 text-center"><input type="checkbox" className="checkbox checkbox-sm" checked={allFilteredSelected} onChange={toggleAllFiltered} aria-label="选择当前页节点" /></th>
                  <th
                    className="w-72 cursor-pointer select-none transition-colors hover:bg-base-200/60"
                    onClick={() => handleSort('name')}
                  >
                    <div className="flex items-center gap-1.5">
                      <span>节点名称</span>
                      <SortIcon active={sortKey === 'name'} dir={sortDir} />
                    </div>
                  </th>
                  <th
                    className="w-24 cursor-pointer select-none transition-colors hover:bg-base-200/60"
                    onClick={() => handleSort('latency')}
                  >
                    <div className="flex items-center gap-1.5">
                      <span>检测延迟</span>
                      <SortIcon active={sortKey === 'latency'} dir={sortDir} />
                    </div>
                  </th>
                  <th className="w-32 cursor-pointer select-none transition-colors hover:bg-base-200/60" onClick={() => handleSort('speed')}>
                    <div className="flex items-center gap-1.5"><span>平均 / 峰值</span><SortIcon active={sortKey === 'speed'} dir={sortDir} /></div>
                  </th>
                  <th className="w-52">出口 IP / 运营商</th>
                  <th className="w-44 cursor-pointer select-none transition-colors hover:bg-base-200/60" onClick={() => handleSort('fraud')}>
                    <div className="flex items-center gap-1.5"><span>IP 质量（最高风险）</span><SortIcon active={sortKey === 'fraud'} dir={sortDir} /></div>
                  </th>
                  <th className="min-w-[240px]">解锁状态</th>
                  <th className="w-24 text-right pr-4">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-base-200/60 text-sm">
                {loading && (
                  <tr>
                    <td colSpan={8} className="py-16 text-center text-base-content/50">
                      <span className="loading loading-spinner loading-md text-primary mr-2" />
                      正在加载节点与检测数据…
                    </td>
                  </tr>
                )}
                {!loading && !nodeFilter.query.isError && sortedNodes.length === 0 && (
                  <tr>
                    <td colSpan={8} className="py-16 text-center text-base-content/50">
                      <div className="flex flex-col items-center justify-center gap-2">
                        <Globe2 className="h-8 w-8 text-base-content/30" />
                        <p className="font-medium text-base-content/70">
                          {availableTotal === 0 ? '暂无在线可用节点' : '没有找到匹配的节点'}
                        </p>
                        {(availableTotal ?? 0) > 0 && nodeFilter.spec.text?.trim() && (
                          <p className="text-xs text-base-content/45">请尝试缩短关键词或检查拼写</p>
                        )}
                        {isFilterActive && (
                          <button
                            type="button"
                            className="btn btn-ghost btn-xs text-primary mt-1 gap-1"
                            onClick={resetFilters}
                          >
                            <RotateCcw className="h-3.5 w-3.5" />
                            重置所有筛选条件
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                )}
                {sortedNodes.map((n, i) => (
                  <UnlockRow
                    key={n.id}
                    index={nodeFilter.page.offset + i}
                    node={n}
                    runtime={n.runtime}
                    config={nodeConfigByID.get(n.id)}
                    source={nodeSourceLabel(nodeConfigByID.get(n.id), subscriptionNamesByID)}
                    result={results[n.id]}
                    diagnostic={diagnostics[n.id]}
                    nodeError={errors[n.id]}
                    checking={Boolean(checking[n.id])}
                    isSelected={selectedNodeID === n.id}
                    checked={selectedIds.has(n.id)}
                    onToggle={() => setSelectedIds((current) => { const next = new Set(current); if (next.has(n.id)) next.delete(n.id); else next.add(n.id); return next })}
                    onClick={() => { if (n.runtime) openDrawer(n.runtime) }}
                    onCheck={(e) => {
                      e.stopPropagation()
                      if (n.runtime) void runSingle(n.runtime)
                    }}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </section>
      </PageContent>

      <UnlockDrawer
        node={selectedNode}
        result={selectedNode ? results[selectedNode.node_id] || null : null}
        diagnostic={selectedNode ? diagnostics[selectedNode.node_id] || null : null}
        isOpen={drawerOpen}
        onClose={() => setDrawerOpen(false)}
      />

      {checkDialogOpen && checkSettings && (
        <NodeCheckDialog
          settings={checkSettings}
          stages={checkStages}
          scope={checkScope}
          selectedCount={selectedTargets.length}
          filteredCount={nodeFilter.query.data?.total || 0}
          targetCount={checkScope === 'selected' ? selectedTargets.length : nodeFilter.query.data?.total || 0}
          onStagesChange={setCheckStages}
          onScopeChange={setCheckScope}
          onSettingsChange={setCheckSettings}
          onSaveSettings={async () => {
            try { const saved = await updateNodeCheckSettings(checkSettings); setCheckSettings(saved); toast.success('综合检测设置已保存') }
            catch (error) { toast.error(error instanceof Error ? error.message : '保存设置失败') }
          }}
          onClose={() => setCheckDialogOpen(false)}
          onStart={() => { if (!preparingTask) void (checkScope === 'selected' ? runSelectedTask() : runFilteredTask(checkStages, checkSettings)) }}
        />
      )}
    </PageLayout>
  )
}

// ---- Sub-components ----

function SortIcon({ active, dir }: { active: boolean; dir: 'asc' | 'desc' }) {
  if (!active) return <ArrowUpDown className="h-3 w-3 opacity-35" />
  return dir === 'asc' ? (
    <ArrowUp className="h-3 w-3 text-primary" />
  ) : (
    <ArrowDown className="h-3 w-3 text-primary" />
  )
}

function FilterChip({
  active,
  onClick,
  label,
  count,
  tone = 'default',
}: {
  active: boolean
  onClick: () => void
  label: string
  count?: number
  tone?: 'default' | 'cyan' | 'success' | 'warning' | 'error' | 'ghost'
}) {
  const toneClasses = {
    default: active ? 'bg-primary text-primary-content' : 'bg-base-200/60 text-base-content/75 hover:bg-base-200',
    cyan: active ? 'bg-cyan-600 text-white' : 'bg-cyan-500/10 text-cyan-700 dark:text-cyan-300 hover:bg-cyan-500/20',
    success: active ? 'bg-success text-success-content' : 'bg-success/10 text-success hover:bg-success/20',
    warning: active ? 'bg-warning text-warning-content' : 'bg-warning/10 text-warning hover:bg-warning/20',
    error: active ? 'bg-error text-error-content' : 'bg-error/10 text-error hover:bg-error/20',
    ghost: active ? 'bg-base-300 text-base-content' : 'bg-base-200/50 text-base-content/50 hover:bg-base-200',
  }

  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'inline-flex cursor-pointer items-center gap-1.5 rounded-xl px-3 py-1.5 text-xs font-medium transition-all',
        toneClasses[tone],
      )}
    >
      <span>{label}</span>
      <span
        className={cn(
          'rounded-full px-1.5 py-0.2 text-[10px] font-bold tabular-nums',
          active ? 'bg-black/20 text-current' : 'bg-base-300/60 text-current opacity-80',
        )}
      >
        {count ?? '—'}
      </span>
    </button>
  )
}

function NodeCheckDialog({ settings, stages, scope, selectedCount, filteredCount, targetCount, onStagesChange, onScopeChange, onSettingsChange, onSaveSettings, onClose, onStart }: {
  settings: NodeCheckSettings
  stages: NodeCheckStages
  scope: 'selected' | 'filtered'
  selectedCount: number
  filteredCount: number
  targetCount: number
  onStagesChange: (value: NodeCheckStages) => void
  onScopeChange: (value: 'selected' | 'filtered') => void
  onSettingsChange: (value: NodeCheckSettings) => void
  onSaveSettings: () => Promise<void>
  onClose: () => void
  onStart: () => void
}) {
  const [showSettings, setShowSettings] = useState(false)
  const closeRef = useRef<HTMLButtonElement>(null)
  const dialogRoot = useModalFocus(onClose)
  const providerEnabled = settings.ippure_enabled || settings.ip_api_enabled
  const effectiveStages = { ...stages, quality: stages.quality && providerEnabled }
  const canStart = targetCount > 0 && Object.values(effectiveStages).some(Boolean)
  const updateSetting = <K extends keyof NodeCheckSettings>(key: K, value: NodeCheckSettings[K]) => onSettingsChange({ ...settings, [key]: value })
  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center bg-black/50 p-0 backdrop-blur-sm sm:items-center sm:p-6" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
      <section ref={dialogRoot} tabIndex={-1} role="dialog" aria-modal="true" aria-labelledby="node-check-title" className="max-h-[92vh] w-full overflow-y-auto rounded-t-2xl bg-base-100 shadow-2xl sm:max-w-3xl sm:rounded-2xl">
        <header className="sticky top-0 z-10 flex items-center justify-between border-b border-base-200 bg-base-100/95 px-5 py-4 backdrop-blur">
          <div><h2 id="node-check-title" className="flex items-center gap-2 text-lg font-bold"><Gauge className="h-5 w-5 text-primary" />节点综合检测</h2><p className="mt-1 text-xs text-base-content/55">手动诊断不会修改节点健康、黑名单或分组出口</p></div>
          <button ref={closeRef} className="btn btn-ghost btn-sm btn-square" onClick={onClose} aria-label="关闭综合检测"><X className="h-4 w-4" /></button>
        </header>
        <div className="space-y-5 p-5 sm:p-6">
          <fieldset><legend className="mb-2 text-sm font-semibold">检测范围</legend><div className="grid gap-2 sm:grid-cols-2">
            <label className={cn('flex cursor-pointer items-center gap-3 rounded-xl border p-3 transition-colors', scope === 'filtered' ? 'border-primary bg-primary/5' : 'border-base-300')}><input type="radio" className="radio radio-primary radio-sm" checked={scope === 'filtered'} onChange={() => onScopeChange('filtered')} /><span><strong className="block text-sm">当前筛选结果</strong><span className="text-xs text-base-content/50">{filteredCount} 个节点</span></span></label>
            <label className={cn('flex cursor-pointer items-center gap-3 rounded-xl border p-3 transition-colors', scope === 'selected' ? 'border-primary bg-primary/5' : 'border-base-300', selectedCount === 0 && 'opacity-50')}><input type="radio" className="radio radio-primary radio-sm" checked={scope === 'selected'} disabled={selectedCount === 0} onChange={() => onScopeChange('selected')} /><span><strong className="block text-sm">已勾选节点</strong><span className="text-xs text-base-content/50">{selectedCount} 个节点</span></span></label>
          </div></fieldset>
          <fieldset><legend className="mb-2 text-sm font-semibold">检测项目</legend><div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
            {([['latency','检测延迟',Wifi],['speed','下载速度',Gauge],['quality','IP 质量',Database],['unlock','流媒体解锁',ShieldCheck]] as const).map(([key,label,Icon]) => <label key={key} className={cn('flex cursor-pointer items-center gap-2 rounded-xl border border-base-300 p-3', key === 'quality' && !providerEnabled && 'cursor-not-allowed opacity-45')}><input type="checkbox" className="checkbox checkbox-primary checkbox-sm" checked={key === 'quality' ? stages[key] && providerEnabled : stages[key]} disabled={key === 'quality' && !providerEnabled} onChange={(event) => onStagesChange({ ...stages, [key]: event.target.checked })} /><Icon className="h-4 w-4 text-primary" /><span className="text-sm font-medium">{label}</span></label>)}
          </div>{!providerEnabled && <p className="mt-2 text-xs text-warning">IP 质量提供商默认关闭；请在下方设置中确认隐私与许可后启用。</p>}</fieldset>
          <div className="grid gap-3 rounded-xl border border-base-200 bg-base-200/30 p-4 sm:grid-cols-3"><div><div className="text-xs text-base-content/50">目标节点</div><div className="mt-1 text-xl font-bold tabular-nums">{targetCount}</div></div><div><div className="text-xs text-base-content/50">最坏下载流量</div><div className="mt-1 text-xl font-bold tabular-nums">{stages.speed ? formatBytes(targetCount * settings.max_download_bytes) : '0 B'}</div></div><div><div className="text-xs text-base-content/50">下载并发</div><div className="mt-1 text-xl font-bold tabular-nums">{settings.speed_concurrency}</div></div></div>
          <button type="button" className="btn btn-ghost btn-sm gap-2" onClick={() => setShowSettings((value) => !value)} aria-expanded={showSettings}><Settings2 className="h-4 w-4" />检测参数</button>
          {showSettings && <div className="grid gap-4 rounded-xl border border-base-300 p-4 sm:grid-cols-2">
            <CheckField id="check-latency-url" label="延迟 URL"><input id="check-latency-url" className="input input-bordered w-full font-mono text-xs" value={settings.latency_url} onChange={(event) => updateSetting('latency_url', event.target.value)} /></CheckField>
            <CheckField id="check-speed-url" label="测速 URL"><input id="check-speed-url" className="input input-bordered w-full font-mono text-xs" value={settings.speed_url} onChange={(event) => updateSetting('speed_url', event.target.value)} /></CheckField>
            <CheckField id="check-landing-ip-url" label="出口 IP URL（经节点隧道请求）"><input id="check-landing-ip-url" className="input input-bordered w-full font-mono text-xs" value={settings.landing_ip_url} onChange={(event) => updateSetting('landing_ip_url', event.target.value)} /></CheckField>
            <CheckField id="latency-timeout" label="延迟超时"><input id="latency-timeout" className="input input-bordered w-full" value={settings.latency_timeout} onChange={(event) => updateSetting('latency_timeout', event.target.value)} /></CheckField>
            <CheckField id="speed-duration" label="测速持续时间"><input id="speed-duration" className="input input-bordered w-full" value={settings.speed_duration} onChange={(event) => updateSetting('speed_duration', event.target.value)} /></CheckField>
            <CheckField id="speed-request-timeout" label="测速请求超时"><input id="speed-request-timeout" className="input input-bordered w-full" value={settings.speed_request_timeout} onChange={(event) => updateSetting('speed_request_timeout', event.target.value)} /></CheckField>
            <CheckField id="quality-timeout" label="质量检测超时"><input id="quality-timeout" className="input input-bordered w-full" value={settings.quality_timeout} onChange={(event) => updateSetting('quality_timeout', event.target.value)} /></CheckField>
            <CheckField id="peak-sample-interval" label="峰值采样窗口 (50-500ms)"><input id="peak-sample-interval" className="input input-bordered w-full" value={settings.peak_sample_interval} onChange={(event) => updateSetting('peak_sample_interval', event.target.value)} /></CheckField>
            <CheckField id="max-download" label="单节点最大字节"><input id="max-download" type="number" min={10240} className="input input-bordered w-full" value={settings.max_download_bytes} onChange={(event) => updateSetting('max_download_bytes', Number(event.target.value))} /></CheckField>
            <CheckField id="latency-concurrency" label="延迟并发 (1-128)"><input id="latency-concurrency" type="number" min={1} max={128} className="input input-bordered w-full" value={settings.latency_concurrency} onChange={(event) => updateSetting('latency_concurrency', Number(event.target.value))} /></CheckField>
            <CheckField id="speed-concurrency" label="测速并发 (1-8)"><input id="speed-concurrency" type="number" min={1} max={8} className="input input-bordered w-full" value={settings.speed_concurrency} onChange={(event) => updateSetting('speed_concurrency', Number(event.target.value))} /></CheckField>
            <CheckField id="quality-concurrency" label="质量并发 (1-10)"><input id="quality-concurrency" type="number" min={1} max={10} className="input input-bordered w-full" value={settings.quality_concurrency} onChange={(event) => updateSetting('quality_concurrency', Number(event.target.value))} /></CheckField>
            <label className="flex cursor-pointer items-center gap-3 rounded-xl border border-base-300 p-3"><input type="checkbox" className="toggle toggle-primary toggle-sm" checked={settings.include_handshake} onChange={(event) => updateSetting('include_handshake', event.target.checked)} /><span><strong className="block text-sm">延迟包含首次握手</strong><span className="text-xs text-base-content/50">关闭时先预热，再记录第二次请求</span></span></label>
            <label className="flex cursor-pointer items-center gap-3 rounded-xl border border-base-300 p-3"><input type="checkbox" className="toggle toggle-primary toggle-sm" checked={settings.ippure_enabled} onChange={(event) => updateSetting('ippure_enabled', event.target.checked)} /><span className="text-sm font-medium">启用 ippure</span></label>
            <CheckField id="ippure-url" label="ippure 兼容 URL"><input id="ippure-url" className="input input-bordered w-full font-mono text-xs" value={settings.ippure_url} disabled={!settings.ippure_enabled} onChange={(event) => updateSetting('ippure_url', event.target.value)} /></CheckField>
            <label className="sm:col-span-2 flex cursor-pointer items-start gap-3 rounded-xl border border-warning/40 bg-warning/5 p-3"><input type="checkbox" className="toggle toggle-warning toggle-sm mt-0.5" checked={settings.ip_api_enabled} onChange={(event) => updateSetting('ip_api_enabled', event.target.checked)} /><span><strong className="block text-sm">启用 ip-api</strong><span className="text-xs text-base-content/55">免费接口为 HTTP、仅限非商业使用；系统会批量查询并遵守 X-Rl/X-Ttl 限流。</span></span></label>
            <CheckField id="ip-api-url" label="ip-api 批量接口"><input id="ip-api-url" className="input input-bordered w-full font-mono text-xs" value={settings.ip_api_base_url} disabled={!settings.ip_api_enabled} onChange={(event) => updateSetting('ip_api_base_url', event.target.value)} /></CheckField>
            <button type="button" className="btn btn-outline btn-sm gap-2 sm:col-span-2 sm:justify-self-end" onClick={() => void onSaveSettings()}><Save className="h-4 w-4" />保存默认参数</button>
          </div>}
        </div>
        <footer className="sticky bottom-0 flex items-center justify-between gap-3 border-t border-base-200 bg-base-100 px-5 py-4"><span className="text-xs text-base-content/50">任务启动后关闭页面也会继续运行</span><div className="flex gap-2"><button className="btn btn-ghost btn-sm" onClick={onClose}>取消</button><button className="btn btn-primary btn-sm gap-2" disabled={!canStart} onClick={onStart}><Play className="h-4 w-4" />开始检测</button></div></footer>
      </section>
    </div>
  )
}

function CheckField({ id, label, children }: { id: string; label: string; children: React.ReactNode }) { return <fieldset><label htmlFor={id} className="mb-1.5 block text-xs font-semibold text-base-content/65">{label}</label>{children}</fieldset> }

function QualityBadges({ item, drift }: { item: NodeCheckResultItem['quality'][number]; drift: boolean }) {
  if (item.status === 'disabled') return <span className="text-[10px] text-base-content/35">{item.provider} 未启用</span>
  if (item.status === 'untested') return <span className="text-[10px] text-base-content/35">{item.provider} 未检测</span>
  if (item.status === 'failed') return <span className="badge badge-error badge-outline badge-xs" title={item.reason}>{item.provider} 失败</span>
  return <div className="flex flex-wrap items-center gap-1"><span className="badge badge-ghost badge-xs">{item.provider}</span>{item.status === 'partial' && <span className="badge badge-warning badge-outline badge-xs">信息不全</span>}{item.is_residential != null && <span className={cn('badge badge-xs', item.is_residential ? 'badge-success' : 'badge-ghost')}>{item.is_residential ? '住宅' : '机房'}</span>}{item.is_broadcast != null && <span className={cn('badge badge-xs', item.is_broadcast ? 'badge-warning' : 'badge-success')}>{item.is_broadcast ? '广播' : '原生'}</span>}{item.proxy === true && <span className="badge badge-warning badge-xs">代理</span>}{item.hosting === true && <span className="badge badge-warning badge-xs">托管</span>}{item.fraud_score != null && <span className={cn('badge badge-xs border-none', fraudClass(item.fraud_score))} title={`${item.fraud_score} · ${fraudLabel(item.fraud_score)}`}>{item.fraud_score} {fraudLabel(item.fraud_score)}</span>}{drift && <span className="badge badge-error badge-xs">漂移</span>}</div>
}

function fraudClass(score: number) { if (score <= 10) return 'bg-emerald-600 text-white'; if (score <= 30) return 'bg-green-500 text-white'; if (score <= 50) return 'bg-lime-500 text-lime-950'; if (score <= 70) return 'bg-amber-400 text-amber-950'; if (score <= 89) return 'bg-orange-500 text-white'; return 'bg-red-600 text-white' }
function fraudLabel(score: number) { if (score <= 10) return '极佳'; if (score <= 30) return '优秀'; if (score <= 50) return '良好'; if (score <= 70) return '中等'; if (score <= 89) return '差'; return '极差' }
function speedColor(bytesPerSecond: number) { const mb = bytesPerSecond / 1024 / 1024; return mb >= 5 ? 'text-success' : mb >= 1 ? 'text-warning' : 'text-error' }
function formatSpeed(bytesPerSecond: number) { return `${(bytesPerSecond / 1024 / 1024).toFixed(2)} MB/s` }
function formatBytes(bytes: number) { if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(2)} GB`; if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MB`; if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`; return `${bytes} B` }
function phaseLabel(phase: string) { return ({ latency: '延迟', speed: '测速', quality: '质量', unlock: '解锁' } as Record<string, string>)[phase] || phase }
function taskStatusLabel(status: NodeCheckTask['status']) { return ({ pending: '等待中', running: '运行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '已中断' } as Record<NodeCheckTask['status'], string>)[status] }

function UnlockRow({
  index,
  node,
  runtime,
  config,
  source,
  result,
  diagnostic,
  nodeError,
  checking,
  isSelected,
  checked,
  onToggle,
  onClick,
  onCheck,
}: {
  index: number
  node: NodeViewItem
  runtime?: NodeSnapshot
  config?: ConfigNodeConfig
  source: string
  result?: UnlockResult
  diagnostic?: NodeCheckResultItem
  nodeError?: string
  checking: boolean
  isSelected: boolean
  checked: boolean
  onToggle: () => void
  onClick: () => void
  onCheck: (e: React.MouseEvent) => void
}) {
  const err = nodeError || result?.error
  const network = unlockNetworkInfo(diagnostic, result)
  const exitCountryCode = network.countryCode
  const latency = displayLatency(runtime, diagnostic)
  const configuredRegion = config?.region || node.region || config?.country || node.country || ''
  const configuredTags = config?.tags || []

  const renderIpRisk = () => {
    const quality = diagnostic?.quality || []
    if (quality.length === 0) return <span className="text-xs text-base-content/30">未检测</span>
    return (
      <div className="flex flex-col gap-1">
        {quality.map((item) => <QualityBadges key={item.provider} item={item} drift={Boolean(diagnostic?.exit_ip_drift)} />)}
      </div>
    )
  }

  const renderBadges = () => {
    if (!result && !err) {
      return (
        <span className="badge badge-ghost badge-sm text-xs text-base-content/40">
          未检测
        </span>
      )
    }
    if (err) {
      return (
        <span className="badge badge-error badge-sm gap-1 text-white">
          <XCircle className="h-3 w-3" />
          检测失败
        </span>
      )
    }

    const services = result?.services || []
    const unlockedList = services.filter((s) => s.status === 'unlocked')
    const partialList = services.filter((s) => s.status === 'partial')
    const originalsList = services.filter((s) => s.status === 'originals_only')

    return (
      <div className="flex flex-wrap items-center gap-1.5">
        {services.slice(0, 7).map((svc) => {
          if (svc.status === 'unlocked') {
            const colorClass = unlockBadgeClasses[svc.name] ?? 'badge-primary text-primary-content'

            return (
              <span key={svc.name} className={cn('badge badge-sm border-none font-semibold text-[10px]', colorClass)}>
                {svc.display_name}
              </span>
            )
          }
          if (svc.status === 'originals_only') {
            return (
              <span key={svc.name} className="badge badge-warning badge-outline badge-sm text-[10px]">
                {svc.display_name} 自制剧
              </span>
            )
          }
          if (svc.status === 'partial') {
            return (
              <span key={svc.name} className="badge badge-warning badge-outline badge-sm text-[10px]">
                {svc.display_name} 部分可用
              </span>
            )
          }
          return null
        })}
        {services.length > 7 && (
          <span className="badge badge-ghost badge-sm text-[10px] text-base-content/50">
            +{services.length - 7}
          </span>
        )}
        {unlockedList.length === 0 && partialList.length === 0 && originalsList.length === 0 && (
          <span className="text-xs text-base-content/40">无完全解锁项</span>
        )}
      </div>
    )
  }

  const rowTint = isSelected ? 'bg-primary/8' : ''

  return (
    <tr
      className={cn('transition-colors hover:bg-base-200/50', runtime && 'cursor-pointer', rowTint)}
      onClick={onClick}
    >
      <td className="text-center font-mono text-xs text-base-content/40">
        <input type="checkbox" className="checkbox checkbox-sm" checked={checked} onClick={(event) => event.stopPropagation()} onChange={onToggle} aria-label={`选择 ${node.name}`} />
      </td>
      <td className="min-w-[18rem] max-w-[320px]">
        <div className="flex items-start gap-2">
          <span className="mt-0.5 shrink-0 text-lg leading-none" title={configuredRegion ? `配置地区 ${configuredRegion}` : '未配置地区'}>
            {regionFlag(configuredRegion)}
          </span>
          <div className="min-w-0 flex-1">
            <div className="truncate font-semibold text-base-content" title={node.name}>
              <span className="mr-1.5 font-mono text-[10px] text-base-content/35">{index + 1}</span>{node.name}
            </div>
            {!runtime && <span className="badge badge-ghost badge-xs mt-1">运行态更新中</span>}
            {(configuredTags.length > 0 || source) && (
              <div className="mt-1 flex min-w-0 flex-wrap items-center gap-1 text-[10px] text-base-content/50">
                {configuredTags.slice(0, 2).map((tag) => (
                  <span key={tag} className="badge badge-ghost badge-xs max-w-24 truncate" title={tag}>{tag}</span>
                ))}
                {configuredTags.length > 2 && (
                  <span className="badge badge-ghost badge-xs" title={configuredTags.slice(2).join('、')}>+{configuredTags.length - 2}</span>
                )}
                {source && <span className="badge badge-ghost badge-xs max-w-52 truncate border-base-300 bg-transparent opacity-70" title={source}>{source}</span>}
              </div>
            )}
          </div>
        </div>
      </td>
      <td className="w-24">
        {latency.value != null ? (
          <div title={`${latency.source}延迟`}>
            <span
              className={cn(
                'font-mono text-xs font-semibold tabular-nums',
                latency.value < 200
                  ? 'text-success'
                  : latency.value < 500
                    ? 'text-warning'
                    : 'text-error',
              )}
            >
              {latency.value} ms
            </span>
            <div className="mt-0.5 text-[10px] text-base-content/40">{latency.source}</div>
          </div>
        ) : (
          <span className="font-mono text-xs text-base-content/35">—</span>
        )}
      </td>
      <td className="w-32">
        {diagnostic?.detection?.average_bytes_per_second != null ? (
          <div className="font-mono text-xs tabular-nums" title={`${((diagnostic.detection.average_bytes_per_second * 8) / 1_000_000).toFixed(2)} Mbps`}><div className={speedColor(diagnostic.detection.average_bytes_per_second)}>{formatSpeed(diagnostic.detection.average_bytes_per_second)}</div><div className="text-[10px] text-base-content/45">峰值 {formatSpeed(diagnostic.detection.peak_bytes_per_second || 0)} · {((diagnostic.detection.average_bytes_per_second * 8) / 1_000_000).toFixed(1)} Mbps</div></div>
        ) : <span className="text-xs text-base-content/35">—</span>}
      </td>
      <td className="max-w-[200px]">
        {(diagnostic?.detection?.exit_ip || result?.ip?.ip) ? (
          <div className="flex flex-col min-w-0">
            <span className="flex items-center gap-1.5 font-mono text-xs font-bold text-base-content/85">
              {exitCountryCode && <span className="text-sm leading-none" title={`落地国家 ${exitCountryCode}`}>{regionFlag(exitCountryCode)}</span>}
              <span>{diagnostic?.detection?.exit_ip || result?.ip?.ip}</span>
            </span>
            <span
              className="truncate text-[10px] text-base-content/50"
              title={`${network.asn} ${network.organization}`.trim() || undefined}
            >
              {exitCountryCode} {network.asn} {network.organization}
            </span>
          </div>
        ) : (
          <span className="font-mono text-xs text-base-content/35">—</span>
        )}
      </td>
      <td className="w-32">{renderIpRisk()}</td>
      <td>
        <div className="flex flex-col gap-1 max-w-[320px]">
          {renderBadges()}
          {err && (
            <span className="truncate text-[10px] text-error/80" title={err}>
              {err}
            </span>
          )}
        </div>
      </td>
      <td className="pr-4 text-right whitespace-nowrap">
        <button
          type="button"
          className={cn(
            'btn btn-xs rounded-lg transition-all',
            result ? 'btn-ghost text-primary hover:bg-primary/15' : 'btn-primary btn-outline',
          )}
          onClick={onCheck}
          disabled={checking || !runtime}
          title={!runtime ? '运行态更新中，暂不可检测' : result ? '重新测试该节点解锁与 IP 属性' : '立即测试该节点'}
        >
          {checking ? <span className="loading loading-spinner loading-xs" /> : result ? '重测' : '检测'}
        </button>
      </td>
    </tr>
  )
}
