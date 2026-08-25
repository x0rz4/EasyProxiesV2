import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type {
  NodeSnapshot,
  UnlockResult,
  UnlockSSEEvent,
  UnlockServiceResult,
} from '../types'
import { fetchNodes, unlockNode, unlockAllNodes, fetchUnlockResults } from '../api/client'
import { regionFlag } from '../utils/region'
import { formatRelative } from '../utils/format'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import UnlockDrawer from './UnlockDrawer'

// ---- Status styling ----

type Status = UnlockServiceResult['status']

const statusMeta: Record<
  Status,
  { label: string; badge: string; dot: string; emoji: string }
> = {
  unlocked: {
    label: '完整解锁',
    badge: 'badge-success',
    dot: 'bg-success',
    emoji: '✅',
  },
  originals_only: {
    label: '仅自制',
    badge: 'badge-warning',
    dot: 'bg-warning',
    emoji: '🟡',
  },
  locked: {
    label: '已封锁',
    badge: 'badge-error',
    dot: 'bg-error',
    emoji: '❌',
  },
  failed: {
    label: '检测失败',
    badge: 'badge-ghost',
    dot: 'bg-base-content/40',
    emoji: '⚠️',
  },
}

function statusFromResult(r: UnlockResult): Status {
  // A node-level error means every service effectively failed to probe.
  if (r.error) return 'failed'
  return r.services[0]?.status ?? 'failed'
}

// ---- Component ----

export default function UnlockPanel() {
  const [nodes, setNodes] = useState<NodeSnapshot[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // tag -> result. Results are kept across batch runs and single runs alike.
  const [results, setResults] = useState<Record<string, UnlockResult>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  // Per-node in-flight state. Either set (batch) or tag (single).
  const [checking, setChecking] = useState<Record<string, boolean>>({})
  
  // Drawer state
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [selectedNode, setSelectedNode] = useState<NodeSnapshot | null>(null)

  // Batch progress
  const [batchRunning, setBatchRunning] = useState(false)
  const [batchProgress, setBatchProgress] = useState({ current: 0, total: 0 })
  const batchAbortRef = useRef<AbortController | null>(null)

  // Filter / search
  const [filter, setFilter] = useState('')
  const [statusFilter, setStatusFilter] = useState<'' | 'unlocked' | 'locked' | 'mixed' | 'failed'>('')

  // ---- Node list ----
  const loadNodes = useCallback(async () => {
    try {
      setError('')
      const res = await fetchNodes()
      // Only nodes that are actually wired up (have a tag + available) can be
      // unlock-checked; the dialer is registered per member tag.
      const usable = (res.nodes || []).filter((n) => n.tag)
      setNodes(usable)
      // Load any previously persisted detection results so the user sees
      // last-saved state without re-running checks. Best-effort: a failure
      // here is ignored (the panel still works, just without history).
      try {
        const saved = await fetchUnlockResults()
        if (saved?.results) {
          setResults((prev) => ({ ...saved.results, ...prev }))
        }
      } catch {
        /* persisted results are optional */
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载节点失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    const t = setTimeout(() => void loadNodes(), 0)
    return () => clearTimeout(t)
  }, [loadNodes])

  useEffect(() => {
    return () => {
      // Abort any in-flight batch on unmount.
      batchAbortRef.current?.abort()
    }
  }, [])

  // ---- Single-node unlock ----
  const runSingle = useCallback(async (tag: string) => {
    setChecking((s) => ({ ...s, [tag]: true }))
    setErrors((s) => {
      const next = { ...s }
      delete next[tag]
      return next
    })
    try {
      const res = await unlockNode(tag)
      setResults((s) => ({ ...s, [tag]: res }))
    } catch (err) {
      setErrors((s) => ({ ...s, [tag]: err instanceof Error ? err.message : '检测失败' }))
    } finally {
      setChecking((s) => {
        const next = { ...s }
        delete next[tag]
        return next
      })
    }
  }, [])

  // ---- Batch unlock (SSE) ----
  const runBatch = useCallback(() => {
    if (batchRunning) return
    setBatchRunning(true)
    setBatchProgress({ current: 0, total: nodes.length })

    const checkingMap: Record<string, boolean> = {}
    for (const n of nodes) checkingMap[n.tag] = true
    setChecking(checkingMap)

    batchAbortRef.current = unlockAllNodes(
      (ev: UnlockSSEEvent) => {
        if (ev.type === 'start') {
          setBatchProgress({ current: 0, total: ev.total })
        } else if (ev.type === 'progress') {
          setBatchProgress({ current: ev.current, total: ev.total })
          if (ev.status === 'error' || !ev.result) {
            // No result object: mark this node as failed. We don't know the tag
            // from the event in this case, so we leave prior results intact.
            return
          }
          const tag = ev.result.tag
          setResults((s) => ({ ...s, [tag]: ev.result! }))
          setChecking((s) => {
            const next = { ...s }
            delete next[tag]
            return next
          })
        } else if (ev.type === 'complete') {
          setBatchRunning(false)
          setChecking({})
        }
      },
      () => {
        setBatchRunning(false)
        setChecking({})
      },
    )
  }, [batchRunning, nodes])

  const stopBatch = useCallback(() => {
    batchAbortRef.current?.abort()
    batchAbortRef.current = null
    setBatchRunning(false)
    setChecking({})
  }, [])

  // ---- Derived ----
  const filteredNodes = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return nodes.filter((n) => {
      if (q) {
        const hay = `${n.name} ${n.region || ''} ${n.country || ''} ${n.tag}`.toLowerCase()
        if (!hay.includes(q)) return false
      }
      if (statusFilter) {
        const r = results[n.tag]
        if (!r) {
          if (statusFilter === 'failed' && errors[n.tag]) return true
          return false
        }
        if (statusFilter === 'failed') return false
        const statuses = r.services.map((s) => s.status)
        if (statusFilter === 'unlocked') return statuses.every((s) => s === 'unlocked')
        if (statusFilter === 'locked') return statuses.some((s) => s === 'locked' || s === 'originals_only')
        if (statusFilter === 'mixed') {
          const hasUnlocked = statuses.some((s) => s === 'unlocked')
          const hasLocked = statuses.some((s) => s === 'locked' || s === 'originals_only')
          return hasUnlocked && hasLocked
        }
      }
      return true
    })
  }, [nodes, filter, statusFilter, results, errors])

  const summary = useMemo(() => {
    const checked = Object.values(results)
    let unlocked = 0
    let locked = 0
    let failed = 0
    for (const r of checked) {
      if (r.error) {
        failed++
        continue
      }
      const statuses = r.services.map((s) => s.status)
      if (statuses.every((s) => s === 'unlocked')) unlocked++
      else if (statuses.some((s) => s === 'locked' || s === 'originals_only')) locked++
      else failed++
    }
    return { total: checked.length, unlocked, locked, failed }
  }, [results])

  const progressPct =
    batchProgress.total > 0
      ? Math.round((batchProgress.current / batchProgress.total) * 100)
      : 0
  const openDrawer = (node: NodeSnapshot) => {
    setSelectedNode(node)
    setDrawerOpen(true)
  }

  // ---- Render helpers ----
  
  const renderBadges = (result: UnlockResult | undefined) => {
    if (!result) return <span className="opacity-40 text-xs">暂无数据</span>
    if (result.error) return <span className="badge badge-sm badge-error">检测失败</span>
    
    return (
      <div className="flex flex-wrap gap-1">
        {result.ip?.pure && <span className="badge badge-sm badge-success">原生IP</span>}
        {(result.ip?.risk_level === 'High' || result.ip?.risk_level === 'Medium') && 
          <span className="badge badge-sm badge-error">高风险</span>
        }
        {result.services?.slice(0, 3).map(svc => {
          if (svc.status === 'unlocked') {
            const isNetflix = svc.name === 'netflix'
            return (
              <span key={svc.name} className={`badge badge-sm ${isNetflix ? 'bg-[#E50914] text-white border-none' : 'badge-primary'}`}>
                {svc.display_name}
              </span>
            )
          }
          return null
        })}
        {(result.services?.filter(s => s.status === 'unlocked').length || 0) > 3 && (
           <span className="badge badge-sm badge-ghost">+{result.services.filter(s => s.status === 'unlocked').length - 3}</span>
        )}
      </div>
    )
  }

  return (
    <PageLayout title="解锁检测">
      <PageHeader>
        <div className="flex flex-col sm:flex-row gap-4 justify-between w-full">通过节点出口发送特定请求，检测 Netflix、Disney+、ChatGPT 解锁状态及原生 IP 纯净度"
        icon={
          <svg xmlns="http://www.w3.org/2000/svg" className="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M8 11V7a4 4 0 118 0m-4 8v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2z" />
          </svg>
        }
        actions={
          <>
            {batchRunning ? (
              <button className="btn btn-error btn-sm gap-2" onClick={stopBatch}>
                <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
                停止
              </button>
            ) : (
              <button
                className="btn btn-primary btn-sm gap-2"
                onClick={runBatch}
                disabled={loading || nodes.length === 0}
              >
                <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M13 10V3L4 14h7v7l9-11h-7z" />
                </svg>
                全部检测
              </button>
            )}
            <button
              className="btn btn-ghost btn-sm gap-2"
              onClick={() => void loadNodes()}
              disabled={loading}
            >
              <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
              </svg>
              刷新节点
            </button>
          </>
        }
      />

      <PageContent fill>
        {/* ---- Batch progress bar ---- */}
        {batchRunning && (
          <div className={`${surfaceClass} mb-4 p-4`}>
            <div className="mb-2 flex items-center justify-between text-sm">
              <span className="font-medium">批量检测中…</span>
              <span className="text-base-content/60">
                {batchProgress.current} / {batchProgress.total} ({progressPct}%)
              </span>
            </div>
            <progress
              className="progress progress-primary w-full"
              value={batchProgress.current}
              max={batchProgress.total || 1}
            />
          </div>
        )}

        {/* ---- Summary cards ---- */}
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 mb-4">
          <SummaryCard label="已检测" value={summary.total} accent="text-base-content" />
          <SummaryCard label="完整解锁" value={summary.unlocked} accent="text-success" />
          <SummaryCard label="受限 / 封锁" value={summary.locked} accent="text-error" />
          <SummaryCard label="检测失败" value={summary.failed} accent="text-base-content/50" />
        </div>

        {/* ---- Toolbar ---- */}
        <div className={`${surfaceClass} mb-4 p-3 flex flex-col gap-3 sm:flex-row sm:items-center`}>
          <div className="flex-1">
            <input
              type="text"
              className="input input-sm input-bordered w-full"
              placeholder="搜索节点名称 / 地区 / tag…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </div>
          <div className="flex gap-1.5">
            {([
              ['', '全部'],
              ['unlocked', '全解锁'],
              ['locked', '受限'],
              ['mixed', '部分'],
              ['failed', '失败'],
            ] as const).map(([val, label]) => (
              <button
                key={val || 'all'}
                className={`btn btn-xs ${statusFilter === val ? 'btn-primary' : 'btn-ghost'}`}
                onClick={() => setStatusFilter(val)}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        {error && (
          <div className="alert alert-error mb-4 py-2 text-sm">
            <span>{error}</span>
          </div>
        )}

        {/* ---- Results table ---- */}
        <div className={`${surfaceClass} flex-1 min-h-0 flex flex-col`}>
          <div className="overflow-auto flex-1">
            <div className="overflow-x-auto w-full">
            <table className="table table-sm w-full relative">
              <thead>
                <tr>
                  <th className="w-10">#</th>
                  <th className="w-48 max-w-[200px]">节点名称</th>
                  <th className="w-48 max-w-[200px]">IP 地址</th>
                  <th className="w-auto">解锁状态</th>
                  <th className="w-24 text-right">操作</th>
                </tr>
              </thead>
              <tbody>
                {loading && (
                  <tr>
                    <td colSpan={5} className="text-center py-10 text-base-content/50">
                      <span className="loading loading-spinner loading-sm mr-2" />
                      加载节点中…
                    </td>
                  </tr>
                )}
                {!loading && filteredNodes.length === 0 && (
                  <tr>
                    <td colSpan={5} className="text-center py-10 text-base-content/50">
                      {nodes.length === 0 ? '暂无可用节点' : '没有匹配的节点'}
                    </td>
                  </tr>
                )}
                {filteredNodes.map((n, i) => (
                  <UnlockRow
                    key={n.tag}
                    index={i}
                    node={n}
                    result={results[n.tag]}
                    nodeError={errors[n.tag]}
                    checking={!!checking[n.tag]}
                    isSelected={selectedNode?.tag === n.tag}
                    onClick={() => openDrawer(n)}
                    onCheck={(e) => {
                      e.stopPropagation()
                      void runSingle(n.tag)
                    }}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </PageContent>
      <UnlockDrawer 
        node={selectedNode}
        result={selectedNode ? results[selectedNode.tag] || null : null}
        isOpen={drawerOpen}
        onClose={() => setDrawerOpen(false)}
      />
    </PageLayout>
  )
}

// ---- Sub-components ----

function SummaryCard({ label, value, accent }: { label: string; value: number; accent: string }) {
  return (
    <div className={`${surfaceClass} p-3`}>
      <div className="text-xs text-base-content/50">{label}</div>
      <div className={`text-2xl font-bold ${accent}`}>{value}</div>
    </div>
  )
}

function UnlockRow({
  index,
  node,
  result,
  nodeError,
  checking,
  isSelected,
  onClick,
  onCheck,
}: {
  index: number
  node: NodeSnapshot
  result?: UnlockResult
  nodeError?: string
  checking: boolean
  isSelected: boolean
  onClick: () => void
  onCheck: (e: React.MouseEvent) => void
}) {

  const renderBadges = () => {
    if (!result) return <span className="opacity-40 text-xs">—</span>
    if (result.error || nodeError) return <span className="badge badge-sm badge-error">检测失败</span>
    
    return (
      <div className="flex flex-wrap gap-1">
        {result.ip?.pure && <span className="badge badge-sm badge-success border-none text-[10px]">原生IP</span>}
        {(result.ip?.risk_level === 'High' || result.ip?.risk_level === 'Medium') && 
          <span className="badge badge-sm badge-error border-none text-[10px]">高风险</span>
        }
        {result.services?.slice(0, 4).map(svc => {
          if (svc.status === 'unlocked') {
            const isNetflix = svc.name === 'netflix'
            const isDisney = svc.name === 'disney_plus'
            const isChatgpt = svc.name === 'chatgpt'
            const isYT = svc.name === 'youtube'
            const colorClass = isNetflix ? 'bg-[#E50914] text-white' : 
                               isDisney ? 'bg-[#113CCF] text-white' : 
                               isChatgpt ? 'bg-[#10A37F] text-white' : 
                               isYT ? 'bg-[#FF0000] text-white' : 'badge-primary'
            return (
              <span key={svc.name} className={`badge badge-sm border-none text-[10px] ${colorClass}`}>
                {svc.display_name}
              </span>
            )
          }
          return null
        })}
        {(result.services?.filter(s => s.status === 'unlocked').length || 0) > 4 && (
           <span className="badge badge-sm badge-ghost text-[10px]">+{result.services.filter(s => s.status === 'unlocked').length - 4}</span>
        )}
      </div>
    )
  }

  const rowTint = isSelected ? 'bg-base-200' : ''
  const err = nodeError || result?.error

  return (
    <tr className={`${rowTint} hover:bg-base-200/50 cursor-pointer transition-colors`} onClick={onClick}>
      <td className="text-base-content/40 text-xs">
        {checking ? (
          <span className="loading loading-spinner loading-xs text-primary" />
        ) : (
          index + 1
        )}
      </td>
      <td className="max-w-[200px] truncate" title={node.name}>
        <div className="flex items-center gap-2">
          <span className="text-xl leading-none" title={node.region}>
            {regionFlag(node.region || '')}
          </span>
          <span className="truncate font-medium">{node.name}</span>
        </div>
        {node.tags && node.tags.length > 0 && (
           <div className="flex flex-wrap gap-1 mt-1">
             {node.tags.slice(0, 3).map(tag => (
               <span key={tag} className="badge badge-[10px] badge-ghost opacity-60 px-1 py-0">{tag}</span>
             ))}
           </div>
        )}
      </td>
      <td className="max-w-[200px] truncate">
         {result?.ip?.ip ? (
           <div className="flex flex-col">
             <span className="font-mono text-xs">{result.ip.ip}</span>
             <span className="text-[10px] opacity-60 truncate" title={`${result.ip.iso_code} ${result.ip.asn} ${result.ip.org}`}>
               {result.ip.iso_code} {result.ip.asn} {result.ip.org}
             </span>
           </div>
         ) : (
           <span className="opacity-40 text-xs">—</span>
         )}
      </td>
      <td>
        <div className="flex flex-col gap-1 max-w-[300px]">
           {renderBadges()}
           {err && <span className="text-[10px] text-error truncate" title={err}>{err}</span>}
        </div>
      </td>
      <td className="text-right whitespace-nowrap">
        <button
          className="btn btn-xs btn-ghost text-primary hover:bg-primary/20"
          onClick={onCheck}
          disabled={checking}
        >
          {result ? '重测' : '检测'}
        </button>
      </td>
    </tr>
  )
}

