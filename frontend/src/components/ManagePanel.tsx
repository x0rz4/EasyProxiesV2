import { useState, useMemo } from 'react'
import type { ConfigNodeConfig, ConfigNodePayload, NodeSnapshot } from '../types'
import {
  fetchConfigNodes, createConfigNode, updateConfigNode, deleteConfigNode,
  toggleConfigNode, batchToggleConfigNodes, batchDeleteConfigNodes, triggerReload,
  importNodes, exportProxies,
  fetchNodes, probeNode, releaseNode, listSubscriptions,
} from '../api/client'
import { regionFlag } from '../utils/region'
import { PageContent, PageHeader, PageLayout } from './ui/PageLayout'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { cn } from '../utils/cn'
import {
  ArrowDown, ArrowUp, ArrowUpDown, Ban, Server, SlidersHorizontal, ChevronDown,
  Download, Upload, RefreshCw, Plus, Search, Activity, FolderX, ShieldCheck,
  Check, Edit2, Trash2, FileUp, AlertTriangle
} from 'lucide-react'

// ---- Merged node type ----
interface MergedNode extends ConfigNodeConfig {
  // Runtime state from monitor
  runtimeStatus: 'normal' | 'unavailable' | 'blacklisted' | 'pending' | 'disabled'
  latency_ms: number
  region?: string
  country?: string
  active_connections: number
  success_count: number
  failure_count: number
  tag?: string
  tags?: string[]
}

// ---- Helpers ----

type ManageSortKey = 'name' | 'status' | 'latency' | 'region' | 'port' | 'source'
type SortDir = 'asc' | 'desc'
type StatusFilter = '' | 'normal' | 'unavailable' | 'blacklisted' | 'pending' | 'disabled'

function statusOrder(s: MergedNode['runtimeStatus']): number {
  switch (s) {
    case 'normal': return 0
    case 'pending': return 1
    case 'unavailable': return 2
    case 'blacklisted': return 3
    case 'disabled': return 4
    default: return 5
  }
}

function compareManageNodes(a: MergedNode, b: MergedNode, key: ManageSortKey, dir: SortDir): number {
  let cmp = 0
  switch (key) {
    case 'name':
      cmp = a.name.localeCompare(b.name)
      break
    case 'status':
      cmp = statusOrder(a.runtimeStatus) - statusOrder(b.runtimeStatus)
      break
    case 'latency': {
      const la = a.latency_ms < 0 ? Infinity : a.latency_ms
      const lb = b.latency_ms < 0 ? Infinity : b.latency_ms
      cmp = la - lb
      break
    }
    case 'region':
      cmp = (a.region || a.country || '').localeCompare(b.region || b.country || '')
      break
    case 'port':
      cmp = (a.port || 0) - (b.port || 0)
      break
    case 'source':
      cmp = (a.source || '').localeCompare(b.source || '')
      break
  }
  return dir === 'asc' ? cmp : -cmp
}

function latencyColor(ms: number): string {
  if (ms < 0) return 'text-base-content/50'
  if (ms <= 100) return 'text-success'
  if (ms <= 300) return 'text-warning'
  return 'text-error'
}

// Extract a normalized proxy type ("ss", "vless", "hysteria2", ...) from a node URI.
function nodeType(uri: string): string {
  const idx = uri.indexOf('://')
  if (idx === -1) return ''
  const scheme = uri.slice(0, idx).toLowerCase()
  // "hy2" is an alias for "hysteria2".
  if (scheme === 'hy2') return 'hysteria2'
  return scheme
}

function typeLabel(t: string): string {
  switch (t) {
    case 'ss': return 'Shadowsocks'
    case 'ssr': return 'SSR'
    case 'vmess': return 'VMess'
    case 'vless': return 'VLESS'
    case 'trojan': return 'Trojan'
    case 'hysteria': return 'Hysteria'
    case 'hysteria2': return 'Hysteria2'
    case 'anytls': return 'AnyTLS'
    case 'http': return 'HTTP'
    case 'socks5': return 'SOCKS5'
    case 'socks': return 'SOCKS'
    default: return t ? t.toUpperCase() : '-'
  }
}

function SortIcon({ active, dir }: { active: boolean; dir: SortDir }) {
  if (!active) {
    return <ArrowUpDown className="h-3 w-3 opacity-30 ml-0.5 inline" />
  }
  return dir === 'asc' 
    ? <ArrowUp className="h-3 w-3 opacity-70 ml-0.5 inline" />
    : <ArrowDown className="h-3 w-3 opacity-70 ml-0.5 inline" />
}

function StatusBadge({ status }: { status: MergedNode['runtimeStatus'] }) {
  switch (status) {
    case 'normal':
      return <span className={cn("badge badge-success badge-sm border-none bg-success/15 text-success font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-success"></div>正常</span>
    case 'unavailable':
      return <span className={cn("badge badge-error badge-sm border-none bg-error/15 text-error font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-error"></div>不可用</span>
    case 'blacklisted':
      return <span className={cn("badge badge-error badge-sm border-none bg-error/30 text-error font-bold flex gap-1 items-center px-2 py-3.5")}><Ban className="h-3 w-3" />黑名单</span>
    case 'pending':
      return <span className={cn("badge badge-warning badge-sm border-none bg-warning/15 text-warning-content font-medium flex gap-1 items-center px-2 py-3.5")}><div className="w-1.5 h-1.5 rounded-full bg-warning animate-pulse"></div>待检查</span>
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

  const { data: configRes, isLoading: configLoading } = useQuery({ queryKey: ['configNodes'], queryFn: () => fetchConfigNodes() })
  const { data: monitorRes, isLoading: monitorLoading } = useQuery({ queryKey: ['nodes'], queryFn: () => fetchNodes().catch(() => null) })
  const { data: subRes, isLoading: subLoading } = useQuery({ queryKey: ['subscriptions'], queryFn: listSubscriptions })

  const configNodes = configRes?.nodes || []
  const monitorData = monitorRes
  const subscriptions = subRes?.subscriptions || []
  const loading = configLoading || monitorLoading || subLoading

  const [needReload, setNeedReload] = useState(false)

  // Modal state
  const [modalOpen, setModalOpen] = useState(false)
  const [editingNode, setEditingNode] = useState<string | null>(null)
  const [form, setForm] = useState<ConfigNodePayload>(emptyPayload)
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Delete confirm
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)

  // Toggle state
  const [toggling, setToggling] = useState<string | null>(null)

  // Probe state
  const [probingTag, setProbingTag] = useState<string | null>(null)

  // Batch selection
  const [selectedNodes, setSelectedNodes] = useState<Set<string>>(new Set())
  const [batchProcessing, setBatchProcessing] = useState(false)
  const [batchDeleteConfirm, setBatchDeleteConfirm] = useState(false)
  const [batchProbeProgress, setBatchProbeProgress] = useState<{ current: number; total: number } | null>(null)

  // Filters
  const [filter, setFilter] = useState('')
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('')
  const [regionFilter, setRegionFilter] = useState('')
  const [sourceFilter, setSourceFilter] = useState('')
  const [typeFilter, setTypeFilter] = useState('')
  const [subscriptionFilter, setSubscriptionFilter] = useState('all')

  // Sort
  const [sortKey, setSortKey] = useState<ManageSortKey>('name')
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  // Import state
  const [importModalOpen, setImportModalOpen] = useState(false)
  const [importContent, setImportContent] = useState('')
  const [importing, setImporting] = useState(false)
  const [importError, setImportError] = useState('')
  const [importResult, setImportResult] = useState<{ message: string; imported: number; errors?: string[] } | null>(null)

  // ---- Merge config + monitor data ----

  const mergedNodes = useMemo((): MergedNode[] => {
    const snapshots = monitorData?.nodes || []
    const snapByURI = new Map<string, NodeSnapshot>()
    const snapByName = new Map<string, NodeSnapshot>()
    for (const s of snapshots) {
      if (s.uri) snapByURI.set(s.uri, s)
      snapByName.set(s.name, s)
    }

    return configNodes.map((cfg: ConfigNodeConfig): MergedNode => {
      const snap = snapByURI.get(cfg.uri) || snapByName.get(cfg.name)

      if (cfg.disabled) {
        return {
          ...cfg,
          runtimeStatus: 'disabled',
          latency_ms: -1,
          region: undefined,
          country: undefined,
          active_connections: 0,
          success_count: 0,
          failure_count: 0,
          tag: undefined,
          tags: undefined,
        }
      }

      if (!snap) {
        return {
          ...cfg,
          runtimeStatus: 'pending',
          latency_ms: -1,
          region: undefined,
          country: undefined,
          active_connections: 0,
          success_count: 0,
          failure_count: 0,
          tag: undefined,
          tags: undefined,
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
        region: snap.region,
        country: snap.country,
        active_connections: snap.active_connections,
        success_count: typeof snap.success_count === 'number' ? snap.success_count : 0,
        failure_count: snap.failure_count,
        tag: snap.tag,
        tags: snap.tags,
      }
    })
  }, [configNodes, monitorData])

  // ---- Filtering ----

  const regions = useMemo(() => {
    const set = new Set<string>()
    for (const n of mergedNodes) {
      if (n.region) set.add(n.region)
    }
    return Array.from(set).sort()
  }, [mergedNodes])

  const sources = useMemo(() => {
    const set = new Set<string>()
    for (const n of mergedNodes) {
      if (n.source) set.add(n.source)
    }
    return Array.from(set).sort()
  }, [mergedNodes])

  const types = useMemo(() => {
    const set = new Set<string>()
    for (const n of mergedNodes) {
      const t = nodeType(n.uri)
      if (t) set.add(t)
    }
    // Fixed friendly order for common types, alphabetical for the rest.
    const order = ['ss', 'ssr', 'vmess', 'vless', 'trojan', 'hysteria', 'hysteria2', 'anytls', 'http', 'socks5', 'socks']
    const all = Array.from(set)
    return all.sort((a, b) => {
      const ia = order.indexOf(a)
      const ib = order.indexOf(b)
      if (ia !== -1 && ib !== -1) return ia - ib
      if (ia !== -1) return -1
      if (ib !== -1) return 1
      return a.localeCompare(b)
    })
  }, [mergedNodes])

  const filteredNodes = useMemo(() => {
    return mergedNodes.filter(n => {
      if (filter) {
        const q = filter.toLowerCase()
        if (!n.name.toLowerCase().includes(q) &&
            !n.uri.toLowerCase().includes(q) &&
            !(n.country || '').toLowerCase().includes(q) &&
            !(n.region || '').toLowerCase().includes(q)) {
          return false
        }
      }
      if (statusFilter && n.runtimeStatus !== statusFilter) return false
      if (regionFilter && n.region !== regionFilter) return false
      if (sourceFilter && n.source !== sourceFilter) return false
      if (typeFilter && nodeType(n.uri) !== typeFilter) return false
      if (subscriptionFilter === 'none' && n.source === 'subscription') return false
      if (subscriptionFilter !== 'all' && subscriptionFilter !== 'none' &&
          !n.subscription_ids.includes(Number(subscriptionFilter))) return false
      return true
    })
  }, [mergedNodes, filter, statusFilter, regionFilter, sourceFilter, typeFilter, subscriptionFilter])

  const sortedNodes = useMemo(() => {
    return [...filteredNodes].sort((a, b) => compareManageNodes(a, b, sortKey, sortDir))
  }, [filteredNodes, sortKey, sortDir])

  const visibleSelectedNames = useMemo(
    () => sortedNodes.filter(node => selectedNodes.has(node.name)).map(node => node.name),
    [selectedNodes, sortedNodes],
  )

  // ---- Handlers ----

  const handleSort = (key: ManageSortKey) => {
    if (sortKey === key) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortKey(key)
      setSortDir('asc')
    }
  }

  const openCreateModal = () => {
    setEditingNode(null)
    setForm(emptyPayload)
    setFormError('')
    setModalOpen(true)
  }

  const openEditModal = (node: MergedNode) => {
    setEditingNode(node.name)
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
      if (editingNode) {
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
      const res = await deleteConfigNode(deleteTarget)
      if (res.reload_error) {
        toast.warning(`${res.message || '节点已删除'}，但自动重载失败：${res.reload_error}`)
        setNeedReload(true)
      } else {
        toast.success(`${res.message || '节点已删除'}${res.reloaded ? '，已自动重载' : ''}`)
      }
      setDeleteTarget(null)
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  const handleToggle = async (node: MergedNode) => {
    const newEnabled = !!node.disabled
    setToggling(node.name)
    try {
      const res = await toggleConfigNode(node.name, newEnabled)
      toast.success(res.message || (newEnabled ? '节点已启用' : '节点已禁用'))
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
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

  const toggleSelectNode = (name: string) => {
    setSelectedNodes(prev => {
      const next = new Set(prev)
      if (next.has(name)) next.delete(name)
      else next.add(name)
      return next
    })
  }

  const toggleSelectAll = () => {
    if (visibleSelectedNames.length === sortedNodes.length) {
      setSelectedNodes(new Set())
    } else {
      setSelectedNodes(new Set(sortedNodes.map(n => n.name)))
    }
  }

  const handleBatchToggle = async (enabled: boolean) => {
    if (visibleSelectedNames.length === 0) return
    setBatchProcessing(true)
    try {
      const res = await batchToggleConfigNodes(visibleSelectedNames, enabled)
      toast.success(res.message || '批量操作完成')
      setSelectedNodes(new Set())
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '批量操作失败')
    } finally {
      setBatchProcessing(false)
    }
  }

  const handleBatchProbe = async () => {
    const nodesToProbe = sortedNodes.filter(n => selectedNodes.has(n.name) && !n.disabled && n.tag)
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
    if (visibleSelectedNames.length === 0) return
    setBatchProcessing(true)
    setBatchDeleteConfirm(false)
    try {
      const res = await batchDeleteConfigNodes(visibleSelectedNames)
      toast.success(res.message || '批量删除完成')
      setSelectedNodes(new Set())
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
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

  const handleImport = async () => {
    if (!importContent.trim()) { setImportError('请输入节点 URI'); return }
    setImporting(true)
    setImportError('')
    setImportResult(null)
    try {
      const res = await importNodes(importContent)
      setImportResult(res)
      if (res.imported > 0) {
        setNeedReload(true)
        toast.success(res.message)
        queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      }
    } catch (err) {
      setImportError(err instanceof Error ? err.message : '导入失败')
    } finally {
      setImporting(false)
    }
  }

  const handleExport = async () => {
    try {
      const text = await exportProxies()
      if (!text.trim()) { toast.error('没有可导出的节点'); return }
      const blob = new Blob([text], { type: 'text/plain' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = 'nodes_export.txt'
      a.click()
      URL.revokeObjectURL(url)
      toast.success('节点已导出')
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

  // ---- Render ----

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <span className="loading loading-spinner loading-lg text-primary"></span>
      </div>
    )
  }

  const thClass = "font-semibold cursor-pointer select-none hover:text-primary transition-colors"

  return (
    <PageLayout>
      <PageHeader
        title="节点管理"
        description={<div className="flex flex-wrap items-center gap-2 font-medium">
              <span>共 <strong className="text-base-content/80">{mergedNodes.length}</strong> 个节点</span>
              {normalCount > 0 && <span className="badge badge-success badge-xs border-none bg-success/15 text-success">正常 {normalCount}</span>}
              {blacklistedCount > 0 && <span className="badge badge-error badge-xs border-none bg-error/15 text-error">黑名单 {blacklistedCount}</span>}
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

      {/* Filters Area */}
      <div className="panel-card p-4 lg:p-5">
        <div className="flex flex-col lg:flex-row gap-4 items-center">
          <div className="relative w-full lg:w-80 shrink-0">
            <div className="absolute inset-y-0 left-0 pl-3.5 flex items-center pointer-events-none text-base-content/40">
              <Search className="h-5 w-5" />
            </div>
            <input
              type="text"
              className="input input-md w-full pl-11 bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              placeholder="搜索节点名称、URI 或 地区..."
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </div>

          <div className="flex flex-wrap sm:flex-nowrap gap-3 w-full lg:w-auto">
            <select className="select select-md bg-base-200/50 focus:bg-base-100 flex-1 sm:w-40" value={subscriptionFilter} onChange={(e) => setSubscriptionFilter(e.target.value)}>
              <option value="all">全部节点</option>
              <option value="none">非订阅节点</option>
              {subscriptions.filter(subscription => subscription.enabled).map(subscription => (
                <option key={subscription.id} value={subscription.id}>{subscription.name}</option>
              ))}
            </select>
            {types.length > 0 && (
              <select className="select select-md bg-base-200/50 focus:bg-base-100 flex-1 sm:w-36" value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)}>
                <option value="">全部类型</option>
                {types.map(t => <option key={t} value={t}>{typeLabel(t)}</option>)}
              </select>
            )}
            <select className="select select-md bg-base-200/50 focus:bg-base-100 flex-1 sm:w-36" value={statusFilter} onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}>
              <option value="">全部状态</option>
              <option value="normal">✅ 正常运行</option>
              <option value="unavailable">❌ 不可用</option>
              <option value="blacklisted">🔴 黑名单</option>
              <option value="pending">⚠️ 待检查</option>
              <option value="disabled">🚫 已禁用</option>
            </select>
            {regions.length > 0 && (
              <select className="select select-md bg-base-200/50 focus:bg-base-100 flex-1 sm:w-32" value={regionFilter} onChange={(e) => setRegionFilter(e.target.value)}>
                <option value="">全部地区</option>
                {regions.map(r => <option key={r} value={r}>{regionFlag(r)} {r.toUpperCase()}</option>)}
              </select>
            )}
            {sources.length > 1 && (
              <select className="select select-md bg-base-200/50 focus:bg-base-100 flex-1 sm:w-32" value={sourceFilter} onChange={(e) => setSourceFilter(e.target.value)}>
                <option value="">全部来源</option>
                {sources.map(s => <option key={s} value={s}>{sourceLabel(s)}</option>)}
              </select>
            )}
          </div>
        </div>
      </div>

      {/* Batch action bar */}
      <div className={`overflow-hidden transition-all duration-300 ${visibleSelectedNames.length > 0 ? 'max-h-48 opacity-100 sm:max-h-24' : 'max-h-0 opacity-0'}`}>
        <div className="flex flex-col gap-3 px-5 py-4 bg-primary/5 border border-primary/20 rounded-2xl shadow-inner relative">
          <div className="absolute left-0 top-0 bottom-0 w-1.5 bg-primary rounded-l-2xl"></div>
          <div className="flex items-center gap-4 flex-wrap">
            <span className="text-base font-medium text-base-content/80 flex items-center gap-2">
              <span className="badge badge-primary badge-md font-bold">{visibleSelectedNames.length}</span> 项已选择
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
                    checked={sortedNodes.length > 0 && visibleSelectedNames.length === sortedNodes.length}
                    onChange={toggleSelectAll}
                    ref={(el) => {
                      if (el) el.indeterminate = visibleSelectedNames.length > 0 && visibleSelectedNames.length < sortedNodes.length
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
                        {filter || statusFilter || regionFilter || sourceFilter || typeFilter || subscriptionFilter !== 'all'
                          ? '未找到匹配的节点数据'
                          : '暂无配置节点'}
                      </p>
                      {!(filter || statusFilter || regionFilter || sourceFilter || typeFilter || subscriptionFilter !== 'all') && (
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
                      ${selectedNodes.has(node.name) ? 'bg-primary/5' : 'hover:bg-base-200/40'}
                    `}
                  >
                    <td className="w-8">
                      <input
                        type="checkbox"
                        className="checkbox checkbox-sm"
                        checked={selectedNodes.has(node.name)}
                        onChange={() => toggleSelectNode(node.name)}
                      />
                    </td>
                    <td>
                      <div className="font-semibold text-sm flex items-center gap-2">
                        {node.region && <span className="text-lg leading-none filter drop-shadow-sm">{regionFlag(node.region)}</span>}
                        <span className="truncate max-w-[200px]" title={node.name}>{node.name}</span>
                      </div>
                      {node.tags && node.tags.length > 0 && (
                        <div className="flex flex-wrap gap-1 mt-1">
                          {node.tags.slice(0, 3).map(tag => (
                            <span key={tag} className="badge badge-[10px] badge-ghost opacity-60 px-1 py-0 text-[10px]">{tag}</span>
                          ))}
                        </div>
                      )}
                    </td>
                    <td><StatusBadge status={node.runtimeStatus} /></td>
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
                          disabled={toggling === node.name}
                          title={node.disabled ? '启用该节点' : '禁用该节点'}
                        >
                          {toggling === node.name
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
                          onClick={() => setDeleteTarget(node.name)}
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
      {filteredNodes.length !== mergedNodes.length && (
        <div className="text-center text-xs text-base-content/30">
          筛选显示 {filteredNodes.length} / {mergedNodes.length} 个节点
        </div>
      )}

      {/* Create / Edit Modal */}
      {modalOpen && (
        <div className="modal modal-open">
          <div className="modal-box">
            <h3 className="font-bold text-xl mb-4">
              {editingNode ? `编辑节点: ${editingNode}` : '添加节点'}
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
                  disabled={!!editingNode}
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
                  {submitting ? <span className="loading loading-spinner loading-xs"></span> : (editingNode ? '更新' : '添加')}
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
                  {importResult.errors && importResult.errors.length > 0 && (
                    <details className="mt-2">
                      <summary className="cursor-pointer text-xs opacity-70">{importResult.errors.length} 个错误</summary>
                      <ul className="text-xs mt-1 space-y-0.5">
                        {importResult.errors.map((err, i) => <li key={i} className="opacity-70">• {err}</li>)}
                      </ul>
                    </details>
                  )}
                </div>
              </div>
            )}
            <p className="text-sm text-base-content/60 mb-3">
              支持代理 URI 列表（每行一个，如 trojan://、vless://、ss:// 等）、Clash 配置（含 "proxies:" 的完整 YAML 或行内项）、Base64 编码的订阅内容。
              可以直接粘贴导出文件的内容或从文件导入。
            </p>
            <div className="mb-3">
              <label className="btn btn-soft btn-sm">
                <FileUp className="h-4 w-4" />
                选择文件
                <input type="file" accept=".txt,.conf,.list,.yaml,.yml" className="hidden" onChange={handleFileImport} />
              </label>
            </div>
            <textarea
              className="textarea textarea-bordered w-full font-mono text-xs h-48"
              placeholder="# 支持以下格式：\n# 1. 代理 URI 列表（每行一个）\ntrojan://password@host:port?sni=example.com#节点名称\n\n# 2. Clash YAML 行内项\n- name: my-ss\n  type: ss\n  server: host\n  port: 8388\n  cipher: aes-256-gcm\n  password: pass\n\n# 3. 完整 Clash YAML 文档\nproxies:\n  - name: my-node\n    type: ss\n    ..."
              value={importContent}
              onChange={(e) => setImportContent(e.target.value)}
            />
            <div className="text-xs text-base-content/40 mt-1">
              {importContent.trim() ? `${importContent.trim().split('\n').filter(l => l.trim() && !l.trim().startsWith('#')).length} 行有效内容` : '等待输入...'}
            </div>
            <div className="modal-action">
              <button type="button" className="btn btn-ghost" onClick={() => setImportModalOpen(false)}>
                {importResult?.imported ? '完成' : '取消'}
              </button>
              <button type="button" className="btn btn-primary" onClick={handleImport} disabled={importing || !importContent.trim()}>
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
              确定要删除节点 <strong>{deleteTarget}</strong> 吗？此操作不可撤销。
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
              确定要删除选中的 <strong>{visibleSelectedNames.length}</strong> 个节点吗？此操作不可撤销。
            </p>
            <div className="modal-action">
              <button className="btn btn-ghost" onClick={() => setBatchDeleteConfirm(false)} disabled={batchProcessing}>取消</button>
              <button className="btn btn-error" onClick={handleBatchDelete} disabled={batchProcessing}>
                {batchProcessing ? <span className="loading loading-spinner loading-xs"></span> : `删除 ${visibleSelectedNames.length} 个节点`}
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
