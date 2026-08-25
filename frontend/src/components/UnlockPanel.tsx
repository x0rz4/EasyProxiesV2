import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type {
  NodeSnapshot,
  UnlockResult,
  UnlockSSEEvent,
  UnlockServiceResult,
} from '../types'
import { fetchNodes, unlockNode, unlockAllNodes, fetchUnlockResults } from '../api/client'
import { regionFlag } from '../utils/region'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'

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

  return (
    <PageLayout fill>
      <PageHeader
        title="解锁检测"
        description="通过节点出口发送特定请求，检测 Netflix、Disney+、ChatGPT 解锁状态及原生 IP 纯净度"
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
            <table className="table table-sm">
              <thead>
                <tr className="text-xs uppercase text-base-content/50">
                  <th>节点</th>
                  <th className="text-center">Netflix</th>
                  <th className="text-center">Disney+</th>
                  <th className="text-center">ChatGPT</th>
                  <th>原生 IP</th>
                  <th className="text-right">操作</th>
                </tr>
              </thead>
              <tbody>
                {loading && (
                  <tr>
                    <td colSpan={6} className="text-center py-10 text-base-content/50">
                      <span className="loading loading-spinner loading-sm mr-2" />
                      加载节点中…
                    </td>
                  </tr>
                )}
                {!loading && filteredNodes.length === 0 && (
                  <tr>
                    <td colSpan={6} className="text-center py-10 text-base-content/50">
                      {nodes.length === 0 ? '暂无可用节点' : '没有匹配的节点'}
                    </td>
                  </tr>
                )}
                {filteredNodes.map((n) => (
                  <UnlockRow
                    key={n.tag}
                    node={n}
                    result={results[n.tag]}
                    nodeError={errors[n.tag]}
                    checking={!!checking[n.tag]}
                    onCheck={() => void runSingle(n.tag)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </PageContent>
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

function ServiceCell({ result }: { result?: UnlockServiceResult }) {
  if (!result) {
    return (
      <td className="text-center">
        <span className="text-base-content/30">—</span>
      </td>
    )
  }
  const meta = statusMeta[result.status]
  return (
    <td className="text-center">
      <div className="flex flex-col items-center gap-1">
        <span className={`badge ${meta.badge} badge-sm gap-1`}>
          <span className={`inline-block h-1.5 w-1.5 rounded-full ${meta.dot}`} />
          {meta.label}
        </span>
        {(result.region || result.detail) && (
          <span className="text-[10px] text-base-content/45 leading-tight max-w-[120px] truncate" title={result.detail}>
            {result.region ? `${regionFlag(result.region)} ${result.region}` : result.detail}
          </span>
        )}
      </div>
    </td>
  )
}

function UnlockRow({
  node,
  result,
  nodeError,
  checking,
  onCheck,
}: {
  node: NodeSnapshot
  result?: UnlockResult
  nodeError?: string
  checking: boolean
  onCheck: () => void
}) {
  const svcByName = useMemo(() => {
    const m: Record<string, UnlockServiceResult> = {}
    if (result) for (const s of result.services) m[s.name] = s
    return m
  }, [result])

  const flag = regionFlag(node.region)
  const overall = result ? statusFromResult(result) : undefined
  const rowTint =
    overall === 'unlocked'
      ? 'bg-success/5'
      : overall === 'locked' || overall === 'originals_only'
        ? 'bg-error/5'
        : overall === 'failed'
          ? 'bg-base-200/40'
          : ''

  return (
    <tr className={`${rowTint} hover:bg-base-200/40 transition-colors`}>
      <td>
        <div className="flex items-center gap-2">
          <span className="text-lg leading-none">{flag}</span>
          <div className="min-w-0">
            <div className="font-medium truncate max-w-[200px]" title={node.name}>
              {node.name}
            </div>
            <div className="text-[11px] text-base-content/45 truncate">
              {node.region ? node.region.toUpperCase() : '—'}
              {node.country ? ` · ${node.country}` : ''}
            </div>
          </div>
        </div>
      </td>
      <ServiceCell result={svcByName.netflix} />
      <ServiceCell result={svcByName.disney_plus} />
      <ServiceCell result={svcByName.chatgpt} />

      {/* Native IP / purity */}
      <td>
        {result?.ip && result.ip.ip ? (
          <div className="flex flex-col gap-0.5">
            <div className="flex items-center gap-1.5 text-sm">
              <span>{regionFlag(result.ip.iso_code || result.ip.region)}</span>
              <span className="font-mono text-xs">{result.ip.ip}</span>
              {result.ip.pure ? (
                <span className="badge badge-success badge-xs">原生</span>
              ) : (
                <span className="badge badge-warning badge-xs">疑似</span>
              )}
            </div>
            {result.ip.country && (
              <div className="text-[11px] text-base-content/45">
                {result.ip.country}
                {result.ip.iso_code ? ` (${result.ip.iso_code})` : ''}
              </div>
            )}
          </div>
        ) : nodeError ? (
          <span className="text-xs text-error/80" title={nodeError}>
            ⚠ {nodeError.length > 24 ? nodeError.slice(0, 24) + '…' : nodeError}
          </span>
        ) : (
          <span className="text-base-content/30">—</span>
        )}
      </td>

      <td className="text-right">
        <button
          className="btn btn-ghost btn-xs gap-1"
          onClick={onCheck}
          disabled={checking}
        >
          {checking ? (
            <>
              <span className="loading loading-spinner loading-xs" />
              检测中
            </>
          ) : (
            <>
              <svg xmlns="http://www.w3.org/2000/svg" className="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
              </svg>
              {result ? '重测' : '检测'}
            </>
          )}
        </button>
      </td>
    </tr>
  )
}
