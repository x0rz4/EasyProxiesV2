import { useEffect, useMemo, useRef, useState } from 'react'
import type { ConfigNodeConfig, ConfigNodePayload, NodeSnapshot } from '../types'
import {
  fetchConfigNodes, createConfigNode, updateConfigNode, deleteConfigNode,
  toggleConfigNode, batchToggleConfigNodes, batchDeleteConfigNodes, triggerReload,
  importNodes, exportProxies,
  fetchNodes, fetchProbeStatus, probeNode, releaseNode, listSubscriptions,
} from '../api/client'
import type { ImportIssue, ImportNodesResult } from '../api/client'
import { regionFlag } from '../utils/region'
import { PageContent, PageHeader, PageLayout } from './ui/PageLayout'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { cn } from '../utils/cn'
import { useNodeFilter } from '../hooks/useNodeFilter'
import NodeFilterBar from './NodeFilterBar'
import NodeTagPicker from './tags/NodeTagPicker'
import {
  ArrowDown, ArrowUp, ArrowUpDown, Ban, Server, SlidersHorizontal, ChevronDown,
  Download, Upload, RefreshCw, Plus, Activity, FolderX, ShieldCheck,
  Check, Edit2, Trash2, FileUp, AlertTriangle
} from 'lucide-react'

// ---- Merged node type ----
interface MergedNode extends ConfigNodeConfig {
  // Runtime state from monitor
  runtimeStatus: 'normal' | 'unavailable' | 'blacklisted' | 'pending' | 'unloaded' | 'disabled'
  latency_ms: number
  region?: string
  country?: string
  active_connections: number
  success_count: number
  failure_count: number
  provisional: boolean
  health_source: NodeSnapshot['health_source']
  tag?: string
  tags?: string[]
}

// ---- Helpers ----

/** 导出文件名带时间戳，避免重复下载互相覆盖。 */
function buildExportFileName(now = new Date()): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  const stamp = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`
  return `nodes_export_${stamp}.txt`
}

function importIssueLabel(kind: string): string {
  switch (kind) {
    case 'unsupported_protocol': return '协议不支持'
    case 'invalid_uri': return '无效 URI'
    case 'create_failed': return '写入失败'
    default: return '解析问题'
  }
}

/** 输入内容提示：URI 列表可给出行数，其它格式只能等服务端解析。 */
function describeImportContent(content: string): string {
  const trimmed = content.trim()
  if (!trimmed) return '等待输入...'
  if (trimmed.includes('://')) {
    const lines = trimmed.split('\n').filter(line => line.trim() && !line.trim().startsWith('#')).length
    return `${lines} 行有效内容`
  }
  if (trimmed.startsWith('{') || /^proxies\s*:/m.test(trimmed)) return 'Clash / Sing-box 配置（解析结果以服务端为准）'
  return '疑似 Base64 订阅内容（解析结果以服务端为准）'
}

type ManageSortKey = 'name' | 'status' | 'latency' | 'region' | 'port' | 'source'
type SortDir = 'asc' | 'desc'

const activeProbeRefreshInterval = 1000
const pendingProbeRefreshInterval = 5000

function latencyColor(ms: number): string {
  if (ms < 0) return 'text-base-content/50'
  if (ms <= 100) return 'text-success'
  if (ms <= 300) return 'text-warning'
  return 'text-error'
}

// matchSnapshot resolves a config node to its runtime entry. Node ID is the
// authoritative link (it is what the runtime carries as node_id); URI and name
// are only fallbacks for rows the API served without an id. Matching on
// URI/name alone made healthy nodes look 待检查 whenever two nodes shared a
// name or a snapshot arrived without a URI.
function buildSnapshotMatcher(snapshots: NodeSnapshot[]): (node: ConfigNodeConfig) => NodeSnapshot | undefined {
  const byID = new Map<number, NodeSnapshot>()
  const byURI = new Map<string, NodeSnapshot>()
  const byName = new Map<string, NodeSnapshot>()
  for (const snapshot of snapshots) {
    if (snapshot.node_id) byID.set(snapshot.node_id, snapshot)
    if (snapshot.uri) byURI.set(snapshot.uri, snapshot)
    byName.set(snapshot.name, snapshot)
  }
  return (node) => (node.id ? byID.get(node.id) : undefined) || byURI.get(node.uri) || byName.get(node.name)
}

// hasPendingRuntimeNodes reports whether any enabled node is still waiting for
// its first health check, which is what keeps the panel polling. A node with no
// runtime entry at all is deliberately excluded: it was never loaded into
// sing-box, so no amount of polling will resolve it and the poll would never
// stop.
function hasPendingRuntimeNodes(configNodes: ConfigNodeConfig[], snapshots: NodeSnapshot[]): boolean {
  const enabledNodes = configNodes.filter((node) => !node.disabled)
  if (enabledNodes.length === 0) {
    return snapshots.some((snapshot) => !snapshot.initial_check_done)
  }

  const matchSnapshot = buildSnapshotMatcher(snapshots)
  return enabledNodes.some((node) => {
    const snapshot = matchSnapshot(node)
    return Boolean(snapshot) && !snapshot!.initial_check_done
  })
}

function SortIcon({ active, dir }: { active: boolean; dir: SortDir }) {
  if (!active) {
    return <ArrowUpDown className="h-3 w-3 opacity-30 ml-0.5 inline" />
  }
  return dir === 'asc' 
    ? <ArrowUp className="h-3 w-3 opacity-70 ml-0.5 inline" />
    : <ArrowDown className="h-3 w-3 opacity-70 ml-0.5 inline" />
}

function StatusBadge({ status, provisional }: { status: MergedNode['runtimeStatus']; provisional?: boolean }) {
  switch (status) {
    case 'normal':
      return <span className={cn("badge badge-success badge-sm border-none bg-success/15 text-success font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-success"></div>已验证可用</span>
    case 'unavailable':
      return <span className={cn("badge badge-error badge-sm border-none bg-error/15 text-error font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-error"></div>已验证不可用</span>
    case 'blacklisted':
      return <span className={cn("badge badge-error badge-sm border-none bg-error/30 text-error font-bold flex gap-1 items-center px-2 py-3.5")}><Ban className="h-3 w-3" />黑名单</span>
    case 'pending':
      return <span className={cn("badge badge-warning badge-sm border-none bg-warning/15 text-warning-content font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-warning animate-pulse"></div>{provisional ? '临时可用 · 待复检' : '待复检'}</span>
    case 'unloaded':
      return (
        <span
          className={cn("badge badge-warning badge-sm border border-warning/40 bg-warning/5 text-warning font-medium flex gap-1 items-center px-2 py-3.5")}
          title="该节点已启用但没有进入运行时：通常是协议或参数无法构建出站而被跳过（详见服务端日志），因此永远不会被健康检查。"
        >
          <AlertTriangle className="h-3 w-3" />未加载
        </span>
      )
    case 'disabled':
      return <span className={cn("badge badge-ghost badge-sm border-none bg-base-300/50 text-base-content/50 font-medium px-2 py-3.5")}>已禁用</span>
    default:
      return <span className={cn("badge badge-ghost badge-sm border-none px-2 py-3.5")}>未知</span>
  }
}

const emptyPayload: ConfigNodePayload = {
  name: '',
  uri: '',
  port: 0,
  username: '',
  password: '',
}

// ---- Component ----

export default function ManagePanel() {
  const queryClient = useQueryClient()

  const { data: probeStatus } = useQuery({
    queryKey: ['probeStatus'],
    queryFn: fetchProbeStatus,
    refetchInterval: (query) => query.state.data?.round.in_flight
      ? activeProbeRefreshInterval
      : pendingProbeRefreshInterval,
  })
  const probeInFlight = Boolean(probeStatus?.round.in_flight)
  const { data: configRes, isLoading: configLoading } = useQuery({ queryKey: ['configNodes'], queryFn: () => fetchConfigNodes() })
  const { data: monitorRes, isLoading: monitorLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: () => fetchNodes().catch(() => null),
    refetchInterval: (query) => {
      if (probeInFlight) return activeProbeRefreshInterval
      const hasPendingNodes = hasPendingRuntimeNodes(configRes?.nodes || [], query.state.data?.nodes || [])
      return hasPendingNodes ? pendingProbeRefreshInterval : false
    },
  })
  const { data: subRes, isLoading: subLoading } = useQuery({ queryKey: ['subscriptions'], queryFn: listSubscriptions })

  const previousProbeInFlight = useRef(false)
  useEffect(() => {
    if (previousProbeInFlight.current && !probeInFlight) {
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    }
    previousProbeInFlight.current = probeInFlight
  }, [probeInFlight, queryClient])

  const configNodes = useMemo(() => configRes?.nodes || [], [configRes?.nodes])
  const monitorData = monitorRes
  const subscriptions = subRes?.subscriptions || []
  const loading = configLoading || monitorLoading || subLoading

  const [needReload, setNeedReload] = useState(false)

  // Modal state
  const [modalOpen, setModalOpen] = useState(false)
  const [editingNode, setEditingNode] = useState<number | null>(null)
  const [form, setForm] = useState<ConfigNodePayload>(emptyPayload)
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Delete confirm
  const [deleteTarget, setDeleteTarget] = useState<{ id: number; name: string } | null>(null)
  const [deleting, setDeleting] = useState(false)

  // Toggle state
  const [toggling, setToggling] = useState<number | null>(null)

  // Probe state
  const [probingTag, setProbingTag] = useState<string | null>(null)

  // Batch selection
  const [selectedNodes, setSelectedNodes] = useState<Set<number>>(new Set())
  const [batchProcessing, setBatchProcessing] = useState(false)
  const [batchDeleteConfirm, setBatchDeleteConfirm] = useState(false)
  const [batchProbeProgress, setBatchProbeProgress] = useState<{ current: number; total: number } | null>(null)

  const nodeFilter = useNodeFilter()
  const sortKey: ManageSortKey = nodeFilter.page.sort === 'latency_ms' ? 'latency' : (['name', 'status', 'region', 'port', 'source'] as string[]).includes(nodeFilter.page.sort) ? nodeFilter.page.sort as ManageSortKey : 'name'
  const sortDir: SortDir = nodeFilter.page.descending ? 'desc' : 'asc'
  const hasFilters = Object.keys(nodeFilter.spec).some(key => key !== 'version')

  // Import state
  const [importModalOpen, setImportModalOpen] = useState(false)
  const [importContent, setImportContent] = useState('')
  const [importing, setImporting] = useState(false)
  const [importError, setImportError] = useState('')
  const [importResult, setImportResult] = useState<ImportNodesResult | null>(null)
  const [importPreview, setImportPreview] = useState<ImportNodesResult | null>(null)
  const [previewing, setPreviewing] = useState(false)

  // ---- Merge config + monitor data ----

  const mergedNodes = useMemo((): MergedNode[] => {
    const snapshots = monitorData?.nodes || []
    const matchSnapshot = buildSnapshotMatcher(snapshots)

    return configNodes.map((cfg: ConfigNodeConfig): MergedNode => {
      const snap = matchSnapshot(cfg)

      if (cfg.disabled) {
        return {
          ...cfg,
          runtimeStatus: 'disabled',
          latency_ms: -1,
          region: cfg.region,
          country: cfg.country,
          active_connections: 0,
          success_count: 0,
          failure_count: 0,
          provisional: false,
          health_source: 'none',
          tag: undefined,
          tags: cfg.tags,
        }
      }

      if (!snap) {
        // Enabled but absent from the runtime: the outbound was never built, so
        // it will never be probed. Reporting this as 待检查 made it look like a
        // pending check that would eventually resolve on its own.
        return {
          ...cfg,
          runtimeStatus: 'unloaded',
          latency_ms: -1,
          region: cfg.region,
          country: cfg.country,
          active_connections: 0,
          success_count: 0,
          failure_count: 0,
          provisional: false,
          health_source: 'none',
          tag: undefined,
          tags: cfg.tags,
        }
      }

      let runtimeStatus: MergedNode['runtimeStatus'] = 'pending'
      if (snap.blacklisted) {
        runtimeStatus = 'blacklisted'
      } else if (!snap.initial_check_done) {
        runtimeStatus = 'pending'
      } else if (snap.available) {
        runtimeStatus = 'normal'
      } else {
        runtimeStatus = 'unavailable'
      }

      return {
        ...cfg,
        runtimeStatus,
        latency_ms: snap.last_latency_ms,
        region: snap.region || cfg.region,
        country: snap.country || cfg.country,
        active_connections: snap.active_connections,
        success_count: typeof snap.success_count === 'number' ? snap.success_count : 0,
        failure_count: snap.failure_count,
        provisional: snap.provisional,
        health_source: snap.health_source,
        tag: snap.tag,
        tags: cfg.tags,
      }
    })
  }, [configNodes, monitorData])

  // The backend owns matching and ordering; this map supplies action details.
  const sortedNodes = useMemo(() => {
    const byID = new Map(mergedNodes.map(node => [node.id, node]))
    return (nodeFilter.query.data?.items || []).flatMap(item => {
      const node = byID.get(item.id)
      return node ? [node] : []
    })
  }, [mergedNodes, nodeFilter.query.data])
  // Selection survives pagination. Batch actions use every selected existing node.
  const visibleSelectedIDs = useMemo(
    () => mergedNodes.filter(node => node.id !== undefined && selectedNodes.has(node.id)).map(node => node.id as number),
    [mergedNodes, selectedNodes],
  )
  const pageSelectedCount = sortedNodes.filter(node => node.id !== undefined && selectedNodes.has(node.id)).length

  // ---- Handlers ----

  const handleSort = (key: ManageSortKey) => {
    nodeFilter.setPage({ ...nodeFilter.page, sort: key === 'latency' ? 'latency_ms' : key,
      descending: sortKey === key ? !nodeFilter.page.descending : false, offset: 0 })
  }

  const openCreateModal = () => {
    setEditingNode(null)
    setForm(emptyPayload)
    setFormError('')
    setModalOpen(true)
  }

  const openEditModal = (node: MergedNode) => {
    // Without an ID the update route has nothing stable to address; editing
    // would fall through to the create path and duplicate the node.
    if (node.id === undefined) {
      toast.error('该节点缺少 ID，无法编辑')
      return
    }
    setEditingNode(node.id)
    setForm({
      name: node.name,
      uri: node.uri,
      port: node.port,
      username: node.username || '',
      password: node.password || '',
    })
    setFormError('')
    setModalOpen(true)
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!form.name.trim()) { setFormError('节点名称不能为空'); return }
    if (!form.uri.trim()) { setFormError('URI 不能为空'); return }

    setSubmitting(true)
    setFormError('')
    try {
      if (editingNode !== null) {
        const res = await updateConfigNode(editingNode, form)
        if (res.reload_error) {
          toast.warning(`${res.message || '节点已更新'}，但自动重载失败：${res.reload_error}`)
          setNeedReload(true)
        } else {
          toast.success(`${res.message || '节点已更新'}${res.reloaded ? '，已自动重载' : ''}`)
        }
      } else {
        const res = await createConfigNode(form)
        if (res.reload_error) {
          toast.warning(`${res.message || '节点已添加'}，但自动重载失败：${res.reload_error}`)
          setNeedReload(true)
        } else {
          toast.success(`${res.message || '节点已添加'}${res.reloaded ? '，已自动重载' : ''}`)
        }
      }
      setModalOpen(false)
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
    } catch (err) {
      setFormError(err instanceof Error ? err.message : '操作失败')
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      const res = await deleteConfigNode(deleteTarget.id)
      if (res.reload_error) {
        toast.warning(`${res.message || '节点已删除'}，但自动重载失败：${res.reload_error}`)
        setNeedReload(true)
      } else {
        toast.success(`${res.message || '节点已删除'}${res.reloaded ? '，已自动重载' : ''}`)
      }
      setDeleteTarget(null)
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  const handleToggle = async (node: MergedNode) => {
    if (node.id === undefined) return
    const newEnabled = !!node.disabled
    setToggling(node.id)
    try {
      const res = await toggleConfigNode(node.id, newEnabled)
      toast.success(res.message || (newEnabled ? '节点已启用' : '节点已禁用'))
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '操作失败')
    } finally {
      setToggling(null)
    }
  }

  const handleProbe = async (tag: string) => {
    setProbingTag(tag)
    try {
      await probeNode(tag)
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '探测失败')
    } finally {
      setProbingTag(null)
    }
  }

  const handleRelease = async (tag: string) => {
    try {
      await releaseNode(tag)
      toast.success('已解除黑名单')
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '解除失败')
    }
  }

  // ---- Batch ----

  const toggleSelectNode = (id: number) => {
    setSelectedNodes(prev => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleSelectAll = () => {
    if (pageSelectedCount === sortedNodes.length) {
      setSelectedNodes(current => { const next = new Set(current); for (const node of sortedNodes) if (node.id !== undefined) next.delete(node.id); return next })
    } else {
      setSelectedNodes(current => new Set([...current, ...sortedNodes.flatMap(n => n.id === undefined ? [] : [n.id])]))
    }
  }

  const handleBatchToggle = async (enabled: boolean) => {
    if (visibleSelectedIDs.length === 0) return
    setBatchProcessing(true)
    try {
      const res = await batchToggleConfigNodes(visibleSelectedIDs, enabled)
      toast.success(res.message || '批量操作完成')
      setSelectedNodes(new Set())
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '批量操作失败')
    } finally {
      setBatchProcessing(false)
    }
  }

  const handleBatchProbe = async () => {
    const nodesToProbe = mergedNodes.filter(n => n.id !== undefined && selectedNodes.has(n.id) && !n.disabled && n.tag)
    if (nodesToProbe.length === 0) {
      toast.error('所选节点中没有可探测的节点（已禁用或无运行时标识的节点将被跳过）')
      return
    }

    setBatchProcessing(true)
    setBatchProbeProgress({ current: 0, total: nodesToProbe.length })
    let successCount = 0
    let failCount = 0
    let completed = 0

    const probeOne = async (tag: string) => {
      try {
        await probeNode(tag)
        successCount++
      } catch {
        failCount++
      } finally {
        completed++
        setBatchProbeProgress({ current: completed, total: nodesToProbe.length })
      }
    }

    // Probe concurrently in batches of 10 (matching backend concurrency)
    const concurrency = 10
    for (let i = 0; i < nodesToProbe.length; i += concurrency) {
      const batch = nodesToProbe.slice(i, i + concurrency)
      await Promise.allSettled(batch.map(n => probeOne(n.tag!)))
    }

    setBatchProbeProgress(null)
    setBatchProcessing(false)
    toast.success(`批量探测完成：${successCount} 成功，${failCount} 失败`)
    queryClient.invalidateQueries({ queryKey: ['nodes'] })
  }

  const handleBatchDelete = async () => {
    if (visibleSelectedIDs.length === 0) return
    setBatchProcessing(true)
    setBatchDeleteConfirm(false)
    try {
      const res = await batchDeleteConfigNodes(visibleSelectedIDs)
      toast.success(res.message || '批量删除完成')
      setSelectedNodes(new Set())
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '批量删除失败')
    } finally {
      setBatchProcessing(false)
    }
  }

  // ---- Import / Export ----

  const openImportModal = () => {
    setImportContent('')
    setImportError('')
    setImportResult(null)
    setImportPreview(null)
    setImportModalOpen(true)
  }

  const handleFileImport = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = (ev) => {
      const text = ev.target?.result
      if (typeof text === 'string') setImportContent(text)
    }
    reader.readAsText(file)
    e.target.value = ''
  }

  // Preview runs the same parse/de-duplicate/build-dry-run pipeline on the
  // server with dry_run, so the counts match what the real import will do.
  const handleImportPreview = async () => {
    if (!importContent.trim()) { setImportError('请输入节点 URI'); return }
    setPreviewing(true)
    setImportError('')
    setImportPreview(null)
    try {
      const res = await importNodes(importContent, { dryRun: true })
      setImportPreview(res)
      if (res.failed) {
        toast.warning(`预览：${res.would_create ?? 0} 个可导入，${res.failed} 个构建失败`)
      }
    } catch (err) {
      setImportError(err instanceof Error ? err.message : '预览失败')
    } finally {
      setPreviewing(false)
    }
  }

  const handleImport = async () => {
    if (!importContent.trim()) { setImportError('请输入节点 URI'); return }
    setImporting(true)
    setImportError('')
    setImportResult(null)
    setImportPreview(null)
    try {
      const res = await importNodes(importContent)
      setImportResult(res)
      if (res.imported > 0) {
        setNeedReload(!res.reloaded)
        if (res.reload_error) toast.error(`节点已保存，加载失败：${res.reload_error}`)
        toast.success(res.message)
        queryClient.invalidateQueries({ queryKey: ['configNodes'] })
        void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
        void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
      }
    } catch (err) {
      setImportError(err instanceof Error ? err.message : '导入失败')
    } finally {
      setImporting(false)
    }
  }

  const handleExport = async () => {
    try {
      const { text, count } = await exportProxies()
      if (count === 0 || !text.trim()) { toast.error('没有可导出的节点'); return }
      const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = buildExportFileName()
      a.click()
      URL.revokeObjectURL(url)
      toast.success(`已导出 ${count} 个节点`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '导出失败')
    }
  }

  const handleReload = async () => {
    try {
      const res = await triggerReload()
      toast.success(res.message || '重载成功')
      setNeedReload(false)
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeQuery'] })
      void queryClient.invalidateQueries({ queryKey: ['nodeFacets'] })
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '重载失败')
    }
  }

  // ---- Source label ----
  const sourceLabel = (source?: string) => {
    switch (source) {
      case 'inline': return '配置文件'
      case 'nodes_file': return '节点文件'
      case 'subscription': return '订阅'
      case 'manual': return '手动添加'
      default: return source || '-'
    }
  }

  // ---- Stats ----
  const disabledCount = mergedNodes.filter(n => n.runtimeStatus === 'disabled').length
  const blacklistedCount = mergedNodes.filter(n => n.runtimeStatus === 'blacklisted').length
  const normalCount = mergedNodes.filter(n => n.runtimeStatus === 'normal').length
  const unloadedCount = mergedNodes.filter(n => n.runtimeStatus === 'unloaded').length

  // ---- Render ----

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <span className="loading loading-spinner loading-lg text-primary"></span>
      </div>
    )
  }

  const importIssueList: ImportIssue[] = (importResult ?? importPreview)?.issues
    ?? ((importResult ?? importPreview)?.errors ?? []).map(message => ({ kind: 'parse', message }))

  const thClass = "font-semibold cursor-pointer select-none hover:text-primary transition-colors"

  return (
    <PageLayout>
      <PageHeader
        title="节点管理"
        description={<div className="flex flex-wrap items-center gap-2 font-medium">
              <span>共 <strong className="text-base-content/80">{mergedNodes.length}</strong> 个节点</span>
              {normalCount > 0 && <span className="badge badge-success badge-xs border-none bg-success/15 text-success">正常 {normalCount}</span>}
              {blacklistedCount > 0 && <span className="badge badge-error badge-xs border-none bg-error/15 text-error">黑名单 {blacklistedCount}</span>}
              {unloadedCount > 0 && (
                <span
                  className="badge badge-warning badge-xs border border-warning/40 bg-warning/5 text-warning"
                  title="已启用但未进入运行时，通常是协议或参数无法构建出站而被跳过（详见服务端日志）"
                >
                  未加载 {unloadedCount}
                </span>
              )}
              {disabledCount > 0 && <span className="badge badge-ghost badge-xs bg-base-200 text-base-content/50">禁用 {disabledCount}</span>}
            </div>}
        icon={<Server className="h-5 w-5" />}
        actions={<>
            <div className="dropdown dropdown-end">
              <div tabIndex={0} role="button" className="btn btn-ghost btn-sm gap-2 border border-base-300 shadow-sm lg:btn-md" title="管理操作" aria-label="管理操作">
                <SlidersHorizontal className="h-4 w-4" />
                <span className="hidden sm:inline">管理操作</span>
                <ChevronDown className="hidden h-4 w-4 opacity-50 sm:block" />
              </div>
              <ul tabIndex={0} className="dropdown-content menu bg-base-100 border border-base-200 rounded-xl z-20 w-48 p-2 shadow-xl mt-2">
                <li><a onClick={openImportModal} className="hover:bg-primary/10 hover:text-primary gap-3"><Download className="h-4 w-4" /> 导入节点配置</a></li>
                <li><a onClick={handleExport} className="hover:bg-primary/10 hover:text-primary gap-3"><Upload className="h-4 w-4" /> 导出所有节点</a></li>
              </ul>
            </div>
            {needReload && (
              <button className="btn btn-warning btn-sm gap-2 shadow-sm animate-pulse lg:btn-md" onClick={handleReload} title="重载配置" aria-label="重载配置">
                <RefreshCw className="h-4 w-4" />
                <span className="hidden sm:inline">重载生效</span>
              </button>
            )}
            <button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" onClick={openCreateModal} title="添加节点" aria-label="添加节点">
              <Plus className="h-4 w-4" />
              <span className="hidden sm:inline">添加节点</span>
            </button>
          </>}
      />

      <PageContent>
        {/* Alerts */}
      {needReload && (
        <div role="alert" className="alert alert-warning alert-soft text-sm">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>配置已变更，请点击「重载配置」使其生效</span>
        </div>
      )}

      <NodeFilterBar filter={nodeFilter} />
      <label className="flex items-center gap-3 text-sm">订阅
        <select className="select select-sm" value={nodeFilter.spec.unsubscribed ? 'none' : nodeFilter.spec.subscription_ids?.[0] || 'all'} onChange={event => {
          const value = event.target.value
          nodeFilter.setSpec({ ...nodeFilter.spec, unsubscribed: value === 'none', subscription_ids: value !== 'all' && value !== 'none' ? [Number(value)] : [] })
        }}><option value="all">全部节点</option><option value="none">非订阅节点</option>{subscriptions.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
      </label>

      {/* Batch action bar */}
      <div className={`overflow-hidden transition-all duration-300 ${visibleSelectedIDs.length > 0 ? 'max-h-48 opacity-100 sm:max-h-24' : 'max-h-0 opacity-0'}`}>
        <div className="flex flex-col gap-3 px-5 py-4 bg-primary/5 border border-primary/20 rounded-2xl shadow-inner relative">
          <div className="absolute left-0 top-0 bottom-0 w-1.5 bg-primary rounded-l-2xl"></div>
          <div className="flex items-center gap-4 flex-wrap">
            <span className="text-base font-medium text-base-content/80 flex items-center gap-2">
              <span className="badge badge-primary badge-md font-bold">{visibleSelectedIDs.length}</span> 项已选择（跨页）
            </span>
            <div className="flex gap-2 ml-auto flex-wrap">
              <button
                className="btn btn-sm btn-primary shadow-sm gap-1.5"
                onClick={handleBatchProbe}
                disabled={batchProcessing}
                title="对选中的已启用节点逐个探测"
              >
                {batchProbeProgress
                  ? <><span className="loading loading-spinner loading-xs"></span> {batchProbeProgress.current}/{batchProbeProgress.total}</>
                  : <><Activity className="h-4 w-4" /> 批量探测</>}
              </button>
              <div className="w-px h-6 bg-base-300 mx-1 self-center"></div>
              <button
                className="btn btn-sm btn-success border-none bg-success/15 text-success hover:bg-success hover:text-success-content"
                onClick={() => handleBatchToggle(true)}
                disabled={batchProcessing}
              >
                启用
              </button>
              <button
                className="btn btn-sm btn-warning border-none bg-warning/15 text-warning-content hover:bg-warning hover:text-warning-content"
                onClick={() => handleBatchToggle(false)}
                disabled={batchProcessing}
              >
                禁用
              </button>
              <button
                className="btn btn-sm btn-error border-none bg-error/15 text-error hover:bg-error hover:text-error-content"
                onClick={() => setBatchDeleteConfirm(true)}
                disabled={batchProcessing}
              >
                删除
              </button>
              <div className="w-px h-6 bg-base-300 mx-1 self-center"></div>
              <button
                className="btn btn-sm btn-ghost hover:bg-base-300"
                onClick={() => setSelectedNodes(new Set())}
                disabled={batchProcessing}
              >
                取消选择
              </button>
            </div>
          </div>
          {batchProbeProgress && (
            <progress
              className="progress progress-primary w-full h-1.5 bg-primary/20"
              value={batchProbeProgress.current}
              max={batchProbeProgress.total}
            ></progress>
          )}
        </div>
      </div>

      {/* Node Table */}
      <div className="panel-card overflow-hidden">
        <div className="overflow-x-auto overflow-y-auto max-h-[calc(100vh-280px)] min-h-[400px]">
          <table className="table table-md table-pin-rows">
            <thead>
              <tr className="bg-base-200/50 border-b border-base-300/50 shadow-sm text-base-content/70">
                <th className="w-8">
                  <input
                    type="checkbox"
                    className="checkbox checkbox-xs"
                    checked={sortedNodes.length > 0 && pageSelectedCount === sortedNodes.length}
                    onChange={toggleSelectAll}
                    ref={(el) => {
                      if (el) el.indeterminate = pageSelectedCount > 0 && pageSelectedCount < sortedNodes.length
                    }}
                  />
                </th>
                <th className={thClass} onClick={() => handleSort('name')}>
                  名称 <SortIcon active={sortKey === 'name'} dir={sortDir} />
                </th>
                <th className={thClass} onClick={() => handleSort('status')}>
                  状态 <SortIcon active={sortKey === 'status'} dir={sortDir} />
                </th>
                <th className={thClass} onClick={() => handleSort('latency')}>
                  延迟 <SortIcon active={sortKey === 'latency'} dir={sortDir} />
                </th>
                <th className={`hidden md:table-cell ${thClass}`} onClick={() => handleSort('region')}>
                  区域 <SortIcon active={sortKey === 'region'} dir={sortDir} />
                </th>
                <th className={`hidden md:table-cell ${thClass}`} onClick={() => handleSort('port')}>
                  端口 <SortIcon active={sortKey === 'port'} dir={sortDir} />
                </th>
                <th className={`hidden lg:table-cell ${thClass}`} onClick={() => handleSort('source')}>
                  来源 <SortIcon active={sortKey === 'source'} dir={sortDir} />
                </th>
                <th className="font-semibold">操作</th>
              </tr>
            </thead>
            <tbody>
              {sortedNodes.length === 0 ? (
                <tr>
                  <td colSpan={8} className="h-[300px] p-0">
                    <div className="flex flex-col items-center justify-center h-full w-full opacity-60">
                      <div className="w-16 h-16 bg-base-200 rounded-full flex items-center justify-center mb-4">
                        <FolderX className="h-8 w-8 text-base-content/40" />
                      </div>
                      <p className="text-base font-medium text-base-content">
                        {hasFilters
                          ? '未找到匹配的节点数据'
                          : '暂无配置节点'}
                      </p>
                      {nodeFilter.spec.text?.trim() && (
                        <p className="text-sm text-base-content/50 mt-1">请尝试缩短关键词或检查拼写</p>
                      )}
                      {!(hasFilters) && (
                        <p className="text-sm text-base-content/50 mt-1">请点击右上角「添加节点」或导入配置以开始</p>
                      )}
                    </div>
                  </td>
                </tr>
              ) : (
                sortedNodes.map((node) => (
                  <tr
                    key={node.name}
                    className={`
                      transition-colors border-b border-base-200/50 last:border-none group
                      ${node.runtimeStatus === 'disabled' ? 'opacity-50 grayscale-[0.5]' : ''}
                      ${node.runtimeStatus === 'blacklisted' ? 'opacity-80' : ''}
                      ${node.id !== undefined && selectedNodes.has(node.id) ? 'bg-primary/5' : 'hover:bg-base-200/40'}
                    `}
                  >
                    <td className="w-8">
                      <input
                        type="checkbox"
                        className="checkbox checkbox-sm"
                        checked={node.id !== undefined && selectedNodes.has(node.id)}
                        onChange={() => node.id !== undefined && toggleSelectNode(node.id)}
                      />
                    </td>
                    <td>
                      <div className="font-semibold text-sm flex items-center gap-2">
                        {node.region && <span className="text-lg leading-none filter drop-shadow-sm">{regionFlag(node.region)}</span>}
                        <span className="truncate max-w-[200px]" title={node.name}>{node.name}</span>
                      </div>
                      <NodeTagPicker nodeId={node.id} currentTagNames={node.tags} />
                    </td>
                    <td><div className="flex flex-wrap items-center gap-1"><StatusBadge status={node.runtimeStatus} provisional={node.provisional} />{(node.health_source === 'persisted' || node.health_source === 'previous_generation') && <span className="badge badge-ghost badge-xs">历史延迟</span>}</div></td>
                    <td className={`font-mono text-sm font-medium ${latencyColor(node.latency_ms)}`}>
                      {node.latency_ms < 0 ? <span className="text-base-content/30">-</span> : `${node.latency_ms} ms`}
                    </td>
                    <td className="hidden md:table-cell text-sm text-base-content/70">
                      {node.country || node.region
                        ? <div className="badge badge-ghost badge-sm gap-1">
                            <span className="leading-none">{regionFlag(node.region)}</span>
                            {node.country || node.region}
                          </div>
                        : '-'}
                    </td>
                    <td className="hidden md:table-cell font-mono text-sm text-base-content/70">{node.port || '-'}</td>
                    <td className="hidden lg:table-cell">
                      <div className="badge badge-ghost badge-sm opacity-70 bg-transparent border-base-300">{sourceLabel(node.source)}</div>
                    </td>
                    <td>
                      <div className="flex gap-1.5 opacity-60 group-hover:opacity-100 transition-opacity">
                        {/* Probe - only for enabled nodes with a tag */}
                        {!node.disabled && node.tag && (
                          <button
                            className="btn btn-sm btn-square btn-ghost text-primary hover:bg-primary/10"
                            onClick={() => handleProbe(node.tag!)}
                            disabled={probingTag === node.tag}
                            title="探测延迟"
                          >
                            {probingTag === node.tag
                              ? <span className="loading loading-spinner loading-xs"></span>
                              : <Activity className="h-4 w-4" />}
                          </button>
                        )}
                        {/* Release from blacklist */}
                        {node.runtimeStatus === 'blacklisted' && node.tag && (
                          <button
                            className="btn btn-sm btn-square btn-ghost text-warning hover:bg-warning/10"
                            onClick={() => handleRelease(node.tag!)}
                            title="解除黑名单"
                          >
                            <ShieldCheck className="h-4 w-4" />
                          </button>
                        )}
                        {/* Toggle enable/disable */}
                        <button
                          className={`btn btn-sm btn-square btn-ghost ${node.disabled ? 'text-success hover:bg-success/10' : 'text-warning hover:bg-warning/10'}`}
                          onClick={() => handleToggle(node)}
                          disabled={node.id !== undefined && toggling === node.id}
                          title={node.disabled ? '启用该节点' : '禁用该节点'}
                        >
                          {node.id !== undefined && toggling === node.id
                            ? <span className="loading loading-spinner loading-xs"></span>
                            : node.disabled
                                ? <Check className="h-4 w-4" />
                                : <Ban className="h-4 w-4" />
                          }
                        </button>
                        {/* Edit */}
                        <button
                          className="btn btn-sm btn-square btn-ghost text-info hover:bg-info/10"
                          onClick={() => openEditModal(node)}
                          title="编辑节点配置"
                        >
                          <Edit2 className="h-4 w-4" />
                        </button>
                        {/* Delete */}
                        <button
                          className="btn btn-sm btn-square btn-ghost text-error hover:bg-error/10"
                          onClick={() => node.id !== undefined && setDeleteTarget({ id: node.id, name: node.name })}
                          title="删除节点"
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>

      {/* Summary */}
      {(nodeFilter.query.data?.total || 0) !== mergedNodes.length && (
        <div className="text-center text-xs text-base-content/30">
          筛选显示 {(nodeFilter.query.data?.total || 0)} / {mergedNodes.length} 个节点
        </div>
      )}

      {/* Create / Edit Modal */}
      {modalOpen && (
        <div className="modal modal-open">
          <div className="modal-box">
            <h3 className="font-bold text-xl mb-4">
              {editingNode !== null ? `编辑节点: ${mergedNodes.find(node => node.id === editingNode)?.name ?? `#${editingNode}`}` : '添加节点'}
            </h3>
            <form onSubmit={handleSubmit}>
              {formError && (
                <div className="alert alert-error mb-3 py-2 text-sm"><span>{formError}</span></div>
              )}
              <fieldset className="fieldset mb-3">
                <legend className="fieldset-legend">名称 *</legend>
                <input
                  type="text" className="input input-sm w-full" placeholder="节点名称"
                  value={form.name}
                  onChange={(e) => setForm(f => ({ ...f, name: e.target.value }))}
                  disabled={editingNode !== null}
                />
              </fieldset>
              <fieldset className="fieldset mb-3">
                <legend className="fieldset-legend">URI *</legend>
                <input
                  type="text" className="input input-sm w-full font-mono text-xs"
                  placeholder="trojan://password@host:port?..."
                  value={form.uri}
                  onChange={(e) => setForm(f => ({ ...f, uri: e.target.value }))}
                />
              </fieldset>
              <fieldset className="fieldset mb-3">
                <legend className="fieldset-legend">本地代理端口</legend>
                <input
                  type="number" className="input input-sm w-full" placeholder="0 = 自动分配"
                  value={form.port || ''}
                  onChange={(e) => setForm(f => ({ ...f, port: parseInt(e.target.value) || 0 }))}
                  min={0} max={65535}
                />
              </fieldset>
              <div className="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
                <fieldset className="fieldset">
                  <legend className="fieldset-legend">用户名</legend>
                  <input
                    type="text" className="input input-sm w-full" placeholder="可选"
                    value={form.username}
                    onChange={(e) => setForm(f => ({ ...f, username: e.target.value }))}
                  />
                </fieldset>
                <fieldset className="fieldset">
                  <legend className="fieldset-legend">密码</legend>
                  <input
                    type="text" className="input input-sm w-full" placeholder="可选"
                    value={form.password}
                    onChange={(e) => setForm(f => ({ ...f, password: e.target.value }))}
                  />
                </fieldset>
              </div>
              <div className="modal-action">
                <button type="button" className="btn btn-ghost" onClick={() => setModalOpen(false)}>取消</button>
                <button type="submit" className="btn btn-primary" disabled={submitting}>
                  {submitting ? <span className="loading loading-spinner loading-xs"></span> : (editingNode !== null ? '更新' : '添加')}
                </button>
              </div>
            </form>
          </div>
          <form method="dialog" className="modal-backdrop" onClick={() => setModalOpen(false)}>
            <button>close</button>
          </form>
        </div>
      )}

      {/* Import Modal */}
      {importModalOpen && (
        <div className="modal modal-open">
          <div className="modal-box max-w-2xl">
            <h3 className="font-bold text-xl mb-4">导入节点</h3>
            {importError && (
              <div className="alert alert-error mb-3 py-2 text-sm"><span>{importError}</span></div>
            )}
            {importResult && (
              <div className={`alert mb-3 py-2 text-sm ${importResult.imported > 0 ? 'alert-success' : 'alert-warning'}`}>
                <div>
                  <span>{importResult.message}</span>
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    <span className="badge badge-sm badge-outline">解析 {importResult.parsed}</span>
                    <span className="badge badge-sm badge-success">新增 {importResult.created}</span>
                    {importResult.duplicates_skipped > 0 && <span className="badge badge-sm badge-warning">重复跳过 {importResult.duplicates_skipped}</span>}
                    {importResult.invalid > 0 && <span className="badge badge-sm badge-error">无效 {importResult.invalid}</span>}
                    {(importResult.failed ?? 0) > 0 && <span className="badge badge-sm badge-error badge-outline">构建/写入失败 {importResult.failed}</span>}
                  </div>
                  {importResult.duplicate_groups && importResult.duplicate_groups.length > 0 && (
                    <details className="mt-2">
                      <summary className="cursor-pointer text-xs opacity-70">查看重复节点</summary>
                      <ul className="mt-1 space-y-0.5 text-xs">
                        {importResult.duplicate_groups.map((group, i) => <li key={i} className="opacity-70">• {group.incoming_node || '未命名节点'} 与已有节点 {group.existing_node} 的连接配置相同</li>)}
                      </ul>
                    </details>
                  )}
                  {importResult.endpoint_collisions && importResult.endpoint_collisions.length > 0 && (
                    <details className="mt-2">
                      <summary className="cursor-pointer text-xs opacity-70">{importResult.endpoint_collisions.length} 个同端点不同配置（已保留）</summary>
                      <ul className="mt-1 space-y-0.5 text-xs">
                        {importResult.endpoint_collisions.map((group, i) => <li key={i} className="opacity-70">• {group.endpoint}：{group.incoming_node || '未命名节点'} / {group.existing_nodes.join('、')}</li>)}
                      </ul>
                    </details>
                  )}
                  {importIssueList.length > 0 && (
                    <details className="mt-2" open={importIssueList.length <= 3}>
                      <summary className="cursor-pointer text-xs opacity-70">{importIssueList.length} 个未导入项</summary>
                      <ul className="text-xs mt-1 space-y-0.5">
                        {importIssueList.map((issue, i) => (
                          <li key={i} className="opacity-70">• {issue.line ? `第 ${issue.line} 行` : importIssueLabel(issue.kind)}：{issue.message}</li>
                        ))}
                      </ul>
                    </details>
                  )}
                </div>
              </div>
            )}
            {importPreview && (
              <div className="alert alert-info mb-3 py-2 text-sm">
                <div>
                  <span>{importPreview.message}</span>
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    <span className="badge badge-sm badge-outline">解析 {importPreview.parsed}</span>
                    <span className="badge badge-sm badge-success">将新增 {importPreview.would_create ?? 0}</span>
                    {importPreview.duplicates_skipped > 0 && <span className="badge badge-sm badge-warning">重复跳过 {importPreview.duplicates_skipped}</span>}
                    {importPreview.invalid > 0 && <span className="badge badge-sm badge-error">无效 {importPreview.invalid}</span>}
                    {(importPreview.failed ?? 0) > 0 && <span className="badge badge-sm badge-error badge-outline">构建失败 {importPreview.failed}</span>}
                  </div>
                </div>
              </div>
            )}
            <p className="text-sm text-base-content/60 mb-3">
              支持代理 URI、Markdown 链接、Clash YAML、Sing-box JSON/JSONC 及最多两层 Base64 订阅内容。
              系统按协议、端点、认证、TLS 和传输配置进行语义去重；同 IP/端口但配置不同的节点会保留并提示。
            </p>
            <div className="mb-3">
              <label className="btn btn-soft btn-sm">
                <FileUp className="h-4 w-4" />
                选择文件
                <input type="file" accept=".txt,.conf,.list,.yaml,.yml,.json,.jsonc" className="hidden" onChange={handleFileImport} />
              </label>
            </div>
            <textarea
              className="textarea textarea-bordered w-full font-mono text-xs h-48"
              placeholder="# 支持以下格式：\n# 1. 代理 URI 列表（每行一个）\ntrojan://password@host:port?sni=example.com#节点名称\n\n# 2. Clash YAML 行内项\n- name: my-ss\n  type: ss\n  server: host\n  port: 8388\n  cipher: aes-256-gcm\n  password: pass\n\n# 3. 完整 Clash YAML 文档\nproxies:\n  - name: my-node\n    type: ss\n    ..."
              value={importContent}
              onChange={(e) => setImportContent(e.target.value)}
            />
            <div className="text-xs text-base-content/40 mt-1">
              {describeImportContent(importContent)}
            </div>
            <div className="modal-action">
              <button type="button" className="btn btn-ghost" onClick={() => setImportModalOpen(false)}>
                {importResult?.imported ? '完成' : '取消'}
              </button>
              <button type="button" className="btn btn-ghost" onClick={handleImportPreview} disabled={previewing || importing || !importContent.trim()}>
                {previewing ? <span className="loading loading-spinner loading-xs"></span> : '预览'}
              </button>
              <button type="button" className="btn btn-primary" onClick={handleImport} disabled={importing || previewing || !importContent.trim()}>
                {importing ? <span className="loading loading-spinner loading-xs"></span> : '导入'}
              </button>
            </div>
          </div>
          <form method="dialog" className="modal-backdrop" onClick={() => !importing && setImportModalOpen(false)}>
            <button>close</button>
          </form>
        </div>
      )}

      {/* Delete Confirmation Modal */}
      {deleteTarget && (
        <div className="modal modal-open">
          <div className="modal-box max-w-sm">
            <h3 className="font-bold text-lg mb-2">确认删除</h3>
            <p className="text-base-content/70">
              确定要删除节点 <strong>{deleteTarget.name}</strong> 吗？此操作不可撤销。
            </p>
            <div className="modal-action">
              <button className="btn btn-ghost" onClick={() => setDeleteTarget(null)} disabled={deleting}>取消</button>
              <button className="btn btn-error" onClick={handleDelete} disabled={deleting}>
                {deleting ? <span className="loading loading-spinner loading-xs"></span> : '删除'}
              </button>
            </div>
          </div>
          <form method="dialog" className="modal-backdrop" onClick={() => !deleting && setDeleteTarget(null)}>
            <button>close</button>
          </form>
        </div>
      )}

      {/* Batch Delete Confirmation Modal */}
      {batchDeleteConfirm && (
        <div className="modal modal-open">
          <div className="modal-box max-w-sm">
            <h3 className="font-bold text-lg mb-2">确认批量删除</h3>
            <p className="text-base-content/70">
              确定要删除选中的 <strong>{visibleSelectedIDs.length}</strong> 个节点吗？此操作不可撤销。
            </p>
            <div className="modal-action">
              <button className="btn btn-ghost" onClick={() => setBatchDeleteConfirm(false)} disabled={batchProcessing}>取消</button>
              <button className="btn btn-error" onClick={handleBatchDelete} disabled={batchProcessing}>
                {batchProcessing ? <span className="loading loading-spinner loading-xs"></span> : `删除 ${visibleSelectedIDs.length} 个节点`}
              </button>
            </div>
          </div>
          <form method="dialog" className="modal-backdrop" onClick={() => !batchProcessing && setBatchDeleteConfirm(false)}>
            <button>close</button>
          </form>
        </div>
      )}
    </PageContent>
    </PageLayout>
  )
}
