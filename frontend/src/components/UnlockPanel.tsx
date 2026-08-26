import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type {
  NodeSnapshot,
  UnlockResult,
  UnlockSSEEvent,
} from '../types'
import { fetchNodes, unlockNode, unlockAllNodes, fetchUnlockResults } from '../api/client'
import { regionFlag } from '../utils/region'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import UnlockDrawer from './UnlockDrawer'
import {
  ShieldCheck, X, Play, RefreshCw, ArrowUpDown, ArrowUp, ArrowDown,
  Search, Sparkles, AlertTriangle, CheckCircle2, XCircle, RotateCcw,
  Globe2,
} from 'lucide-react'
import { toast } from 'sonner'
import { cn } from '../utils/cn'

type StatusFilterType = '' | 'unlocked' | 'pure_ip' | 'locked' | 'mixed' | 'failed' | 'untested'

export default function UnlockPanel() {
  const [nodes, setNodes] = useState<NodeSnapshot[]>([])
  const [loading, setLoading] = useState(true)

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
  const [statusFilter, setStatusFilter] = useState<StatusFilterType>('')
  const [countryFilter, setCountryFilter] = useState('')

  // Sorting
  const [sortKey, setSortKey] = useState<'name' | 'latency' | null>(null)
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('asc')

  const handleSort = (key: 'name' | 'latency') => {
    if (sortKey === key) {
      setSortDir(sortDir === 'asc' ? 'desc' : 'asc')
    } else {
      setSortKey(key)
      setSortDir('asc')
    }
  }

  // ---- Node list ----
  const loadNodes = useCallback(async () => {
    try {
      const res = await fetchNodes()
      // unlock-checked; the dialer is registered per member tag.
      const usable = (res.nodes || []).filter((n) => n.tag && n.initial_check_done && n.available && !n.blacklisted)
      setNodes(usable)
      // Load any previously persisted detection results so the user sees
      // last-saved state without re-running checks.
      try {
        const saved = await fetchUnlockResults()
        if (saved?.results) {
          setResults((prev) => ({ ...saved.results, ...prev }))
        }
      } catch {
        /* persisted results are optional */
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '加载节点失败')
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
      toast.success(`节点 ${tag} 检测完成`)
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
          toast.success('批量检测完成')
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

  // ---- Summary metrics ----
  const summary = useMemo(() => {
    let unlocked = 0
    let locked = 0
    let mixed = 0
    let failed = 0
    let pureIp = 0
    let testedCount = 0

    for (const node of nodes) {
      const r = results[node.tag]
      const err = errors[node.tag]
      if (!r && !err) continue

      testedCount++
      if (err || r?.error) {
        failed++
        continue
      }
      if (!r) continue

      if (r.ip?.pure) {
        pureIp++
      }

      const statuses = r.services?.map((s) => s.status) || []
      const isAllUnlocked = statuses.length > 0 && statuses.every((s) => s === 'unlocked')
      const hasUnlocked = statuses.some((s) => s === 'unlocked')
      const hasLocked = statuses.some((s) => s === 'locked' || s === 'originals_only')

      if (isAllUnlocked) {
        unlocked++
      } else if (hasUnlocked && hasLocked) {
        mixed++
      } else if (hasLocked) {
        locked++
      } else {
        failed++
      }
    }

    const untested = Math.max(0, nodes.length - testedCount)
    return {
      total: nodes.length,
      tested: testedCount,
      untested,
      unlocked,
      pureIp,
      locked,
      mixed,
      failed,
    }
  }, [nodes, results, errors])

  // ---- Countries breakdown ----
  const countries = useMemo(() => {
    const counts = new Map<string, number>()
    for (const n of nodes) {
      const code = n.country || n.region
      if (code) {
        counts.set(code.toUpperCase(), (counts.get(code.toUpperCase()) || 0) + 1)
      }
    }
    return Array.from(counts.entries())
      .sort((a, b) => b[1] - a[1])
      .map(([code, count]) => ({ code, count }))
  }, [nodes])

  // ---- Filtered and sorted data ----
  const filteredNodes = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return nodes.filter((n) => {
      if (countryFilter) {
        const c = (n.country || n.region || '').toUpperCase()
        if (c !== countryFilter.toUpperCase()) return false
      }

      if (q) {
        const r = results[n.tag]
        const hay = `${n.name} ${n.region || ''} ${n.country || ''} ${n.tag} ${r?.ip?.ip || ''} ${r?.ip?.org || ''} ${r?.ip?.asn || ''}`.toLowerCase()
        if (!hay.includes(q)) return false
      }

      if (statusFilter) {
        const r = results[n.tag]
        const err = errors[n.tag]
        if (statusFilter === 'untested') {
          return !r && !err
        }
        if (statusFilter === 'failed') {
          return Boolean(err || r?.error)
        }
        if (!r || r.error || err) return false

        if (statusFilter === 'pure_ip') {
          return Boolean(r.ip?.pure)
        }
        const statuses = r.services?.map((s) => s.status) || []
        if (statusFilter === 'unlocked') {
          return statuses.length > 0 && statuses.every((s) => s === 'unlocked')
        }
        if (statusFilter === 'locked') {
          const hasLocked = statuses.some((s) => s === 'locked' || s === 'originals_only')
          const hasUnlocked = statuses.some((s) => s === 'unlocked')
          return hasLocked || hasUnlocked
        }
        if (statusFilter === 'mixed') {
          const hasUnlocked = statuses.some((s) => s === 'unlocked')
          const hasLocked = statuses.some((s) => s === 'locked' || s === 'originals_only')
          return hasUnlocked && hasLocked
        }
      }
      return true
    })
  }, [nodes, filter, statusFilter, countryFilter, results, errors])

  const sortedNodes = useMemo(() => {
    if (!sortKey) return filteredNodes
    return [...filteredNodes].sort((a, b) => {
      let cmp = 0
      if (sortKey === 'name') {
        cmp = a.name.localeCompare(b.name)
      } else if (sortKey === 'latency') {
        const valA = a.last_latency_ms < 0 ? Infinity : a.last_latency_ms
        const valB = b.last_latency_ms < 0 ? Infinity : b.last_latency_ms
        cmp = valA - valB
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
  }, [filteredNodes, sortKey, sortDir])

  const progressPct =
    batchProgress.total > 0
      ? Math.round((batchProgress.current / batchProgress.total) * 100)
      : 0

  const openDrawer = (node: NodeSnapshot) => {
    setSelectedNode(node)
    setDrawerOpen(true)
  }

  const isFilterActive = Boolean(filter || countryFilter || statusFilter)
  const resetFilters = () => {
    setFilter('')
    setCountryFilter('')
    setStatusFilter('')
  }

  return (
    <PageLayout>
      <PageHeader
        title="解锁检测"
        description="通过节点出口发送特定请求，检测 Netflix、Disney+、ChatGPT 解锁状态及原生 IP 纯净度"
        icon={<ShieldCheck className="h-5 w-5" />}
        actions={
          <div className="flex items-center gap-2">
            {batchRunning ? (
              <button className="btn btn-error btn-sm gap-2 lg:btn-md" onClick={stopBatch}>
                <X className="h-4 w-4" />
                停止检测
              </button>
            ) : (
              <button
                className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md"
                onClick={runBatch}
                disabled={loading || nodes.length === 0}
              >
                <Play className="h-4 w-4 fill-current" />
                全部检测
              </button>
            )}
            <button
              className="btn btn-ghost btn-sm gap-2 border border-base-300 shadow-sm lg:btn-md"
              onClick={() => void loadNodes()}
              disabled={loading}
              title="重新加载可用节点列表"
            >
              <RefreshCw className={cn('h-4 w-4', loading && 'animate-spin')} />
              <span className="hidden sm:inline">刷新节点</span>
            </button>
          </div>
        }
      />

      <PageContent fill>
        {/* ---- Batch progress banner ---- */}
        {batchRunning && (
          <div className={cn(surfaceClass, 'mb-5 border-primary/30 bg-primary/5 p-4 sm:p-5')}>
            <div className="mb-2.5 flex items-center justify-between">
              <div className="flex items-center gap-2">
                <span className="relative flex h-2.5 w-2.5">
                  <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary opacity-75" />
                  <span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary" />
                </span>
                <span className="text-sm font-bold text-primary">正在批量检测流媒体解锁与 IP 纯净度…</span>
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
          </div>
        )}

        {/* ---- Redesigned Status Summary Cards ---- */}
        <section className="mb-5 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <SummaryCard
            label="已检测 / 节点"
            value={nodes.length > 0 ? `${summary.tested} / ${summary.total}` : '0'}
            hint={
              nodes.length === 0
                ? '暂无可用节点'
                : summary.untested === 0
                  ? '覆盖率 100% · 全部已完成'
                  : `覆盖率 ${Math.round((summary.tested / summary.total) * 100)}% · 待检测 ${summary.untested}`
            }
            icon={<ShieldCheck className="h-5 w-5" />}
            toneBg="bg-primary/10"
            toneText="text-primary"
            active={statusFilter === ''}
            onClick={() => setStatusFilter('')}
          />
          <SummaryCard
            label="原生 IP (纯净)"
            value={summary.pureIp}
            hint={
              summary.tested > 0
                ? `原生率 ${Math.round((summary.pureIp / summary.tested) * 100)}% · 纯净出口`
                : '纯净住宅/机房原生'
            }
            icon={<Sparkles className="h-5 w-5" />}
            toneBg="bg-cyan-500/10"
            toneText="text-cyan-600 dark:text-cyan-400"
            active={statusFilter === 'pure_ip'}
            onClick={() => setStatusFilter(statusFilter === 'pure_ip' ? '' : 'pure_ip')}
          />
          <SummaryCard
            label="完整解锁"
            value={summary.unlocked}
            hint={
              summary.tested > 0
                ? `占比 ${Math.round((summary.unlocked / summary.tested) * 100)}% · 全平台畅通`
                : '全部平台完全解锁'
            }
            icon={<CheckCircle2 className="h-5 w-5" />}
            toneBg="bg-success/10"
            toneText="text-success"
            active={statusFilter === 'unlocked'}
            onClick={() => setStatusFilter(statusFilter === 'unlocked' ? '' : 'unlocked')}
          />
          <SummaryCard
            label="受限 / 部分"
            value={summary.locked + summary.mixed}
            hint="仅自制剧或部分流媒体"
            icon={<AlertTriangle className="h-5 w-5" />}
            toneBg="bg-warning/10"
            toneText="text-warning"
            active={statusFilter === 'locked'}
            onClick={() => setStatusFilter(statusFilter === 'locked' ? '' : 'locked')}
          />
          <SummaryCard
            label="检测失败"
            value={summary.failed}
            hint="超时或节点不可达"
            icon={<XCircle className="h-5 w-5" />}
            toneBg={summary.failed > 0 ? 'bg-error/10' : 'bg-base-200'}
            toneText={summary.failed > 0 ? 'text-error' : 'text-base-content/40'}
            active={statusFilter === 'failed'}
            onClick={() => setStatusFilter(statusFilter === 'failed' ? '' : 'failed')}
          />
        </section>

        {/* ---- Redesigned Search & Filter Toolbar ---- */}
        <section className={cn(surfaceClass, 'mb-5 p-4 sm:p-5')}>
          <div className="flex flex-col gap-4">
            {/* Row 1: Search input + Country selector */}
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
              {/* Search Bar */}
              <div className="relative min-w-0 flex-1">
                <div className="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5 text-base-content/40">
                  <Search className="h-4 w-4" />
                </div>
                <input
                  type="text"
                  className="input input-md w-full rounded-xl border border-base-300/70 bg-base-200/40 pl-10 pr-9 text-sm transition-colors focus:border-primary/50 focus:bg-base-100 focus:outline-none"
                  placeholder="搜索节点名称、地区代号 (如 HK/JP)、Tag 或 IP 地址…"
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                />
                {filter && (
                  <button
                    type="button"
                    className="absolute inset-y-0 right-0 flex items-center pr-3 text-base-content/40 hover:text-base-content"
                    onClick={() => setFilter('')}
                    aria-label="清空搜索"
                  >
                    <X className="h-4 w-4" />
                  </button>
                )}
              </div>

              {/* Country Selector */}
              <div className="sm:w-60">
                <select
                  className="select select-md w-full rounded-xl border border-base-300/70 bg-base-200/40 text-sm font-medium focus:border-primary/50 focus:bg-base-100"
                  value={countryFilter}
                  onChange={(e) => setCountryFilter(e.target.value)}
                  aria-label="筛选国家和地区"
                >
                  <option value="">🌐 全部国家 / 地区 ({nodes.length})</option>
                  {countries.map(({ code, count }) => (
                    <option key={code} value={code}>
                      {regionFlag(code)} {code} ({count} 个节点)
                    </option>
                  ))}
                </select>
              </div>
            </div>

            {/* Row 2: Status Filter Chips & Result Counter */}
            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-base-200 pt-3.5">
              {/* Segmented Filter Pills */}
              <div className="flex flex-wrap items-center gap-1.5 sm:gap-2">
                <FilterChip
                  active={statusFilter === ''}
                  onClick={() => setStatusFilter('')}
                  label="全部"
                  count={nodes.length}
                />
                <FilterChip
                  active={statusFilter === 'pure_ip'}
                  onClick={() => setStatusFilter(statusFilter === 'pure_ip' ? '' : 'pure_ip')}
                  label="✨ 原生 IP"
                  count={summary.pureIp}
                  tone="cyan"
                />
                <FilterChip
                  active={statusFilter === 'unlocked'}
                  onClick={() => setStatusFilter(statusFilter === 'unlocked' ? '' : 'unlocked')}
                  label="✅ 全解锁"
                  count={summary.unlocked}
                  tone="success"
                />
                <FilterChip
                  active={statusFilter === 'locked'}
                  onClick={() => setStatusFilter(statusFilter === 'locked' ? '' : 'locked')}
                  label="⚠️ 受限 / 部分"
                  count={summary.locked + summary.mixed}
                  tone="warning"
                />
                <FilterChip
                  active={statusFilter === 'failed'}
                  onClick={() => setStatusFilter(statusFilter === 'failed' ? '' : 'failed')}
                  label="❌ 失败"
                  count={summary.failed}
                  tone="error"
                />
                {summary.untested > 0 && (
                  <FilterChip
                    active={statusFilter === 'untested'}
                    onClick={() => setStatusFilter(statusFilter === 'untested' ? '' : 'untested')}
                    label="⏳ 未检测"
                    count={summary.untested}
                    tone="ghost"
                  />
                )}
              </div>

              {/* Status information & Reset */}
              <div className="flex items-center gap-3 text-xs text-base-content/55">
                <span>
                  显示 <strong className="font-bold text-base-content">{sortedNodes.length}</strong> / {nodes.length} 个节点
                </span>
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
        <section className={cn(surfaceClass, 'flex flex-1 min-h-0 flex-col overflow-hidden')}>
          <div className="flex-1 overflow-auto">
            <table className="table table-sm w-full relative">
              <thead className="sticky top-0 z-10 border-b border-base-200 bg-base-100 text-xs font-semibold uppercase tracking-wider text-base-content/60 backdrop-blur-md">
                <tr>
                  <th className="w-12 text-center">#</th>
                  <th
                    className="w-56 cursor-pointer select-none transition-colors hover:bg-base-200/60"
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
                      <span>延迟</span>
                      <SortIcon active={sortKey === 'latency'} dir={sortDir} />
                    </div>
                  </th>
                  <th className="w-52">出口 IP / 运营商</th>
                  <th className="w-32">IP 属性</th>
                  <th className="min-w-[240px]">解锁状态</th>
                  <th className="w-24 text-right pr-4">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-base-200/60 text-sm">
                {loading && (
                  <tr>
                    <td colSpan={7} className="py-16 text-center text-base-content/50">
                      <span className="loading loading-spinner loading-md text-primary mr-2" />
                      正在加载节点与检测数据…
                    </td>
                  </tr>
                )}
                {!loading && sortedNodes.length === 0 && (
                  <tr>
                    <td colSpan={7} className="py-16 text-center text-base-content/50">
                      <div className="flex flex-col items-center justify-center gap-2">
                        <Globe2 className="h-8 w-8 text-base-content/30" />
                        <p className="font-medium text-base-content/70">
                          {nodes.length === 0 ? '暂无可用节点' : '没有找到匹配的节点'}
                        </p>
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
                    key={n.tag}
                    index={i}
                    node={n}
                    result={results[n.tag]}
                    nodeError={errors[n.tag]}
                    checking={Boolean(checking[n.tag])}
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
        </section>
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

function SortIcon({ active, dir }: { active: boolean; dir: 'asc' | 'desc' }) {
  if (!active) return <ArrowUpDown className="h-3 w-3 opacity-35" />
  return dir === 'asc' ? (
    <ArrowUp className="h-3 w-3 text-primary" />
  ) : (
    <ArrowDown className="h-3 w-3 text-primary" />
  )
}

function SummaryCard({
  label,
  value,
  hint,
  icon,
  toneBg,
  toneText,
  active = false,
  onClick,
}: {
  label: string
  value: number | string
  hint: string
  icon: React.ReactNode
  toneBg: string
  toneText: string
  active?: boolean
  onClick?: () => void
}) {
  return (
    <article
      onClick={onClick}
      className={cn(
        surfaceClass,
        'cursor-pointer p-4 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-md sm:p-5',
        active && 'ring-2 ring-primary/60 border-primary/40',
      )}
    >
      <div className="flex items-center justify-between">
        <span className="text-xs font-semibold uppercase tracking-wider text-base-content/55">{label}</span>
        <span className={cn('rounded-xl p-2 transition-colors', toneBg, toneText)}>{icon}</span>
      </div>
      <div className={cn('mt-2.5 text-2xl font-black tabular-nums sm:text-3xl', toneText)}>
        {value}
      </div>
      <p className="mt-1.5 truncate text-[11px] font-medium text-base-content/50" title={hint}>
        {hint}
      </p>
    </article>
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
  count: number
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
        {count}
      </span>
    </button>
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
  const err = nodeError || result?.error

  const renderIpRisk = () => {
    if (!result && !err) return <span className="text-xs text-base-content/30">—</span>
    if (err) return <span className="text-xs text-base-content/30">—</span>

    return (
      <div className="flex flex-wrap items-center gap-1.5">
        {result?.ip?.pure ? (
          <span className="badge badge-success badge-sm gap-1 border-none font-semibold text-[10px] text-white">
            <Sparkles className="h-2.5 w-2.5" />
            原生IP
          </span>
        ) : (
          <span className="badge badge-ghost badge-sm border-base-300 text-[10px] text-base-content/60">
            广播IP
          </span>
        )}
        {result?.ip?.risk_level === 'High' && (
          <span className="badge badge-error badge-sm border-none font-bold text-[10px] text-white">
            高风险
          </span>
        )}
        {result?.ip?.risk_level === 'Medium' && (
          <span className="badge badge-warning badge-sm border-none font-bold text-[10px]">
            中风险
          </span>
        )}
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
    const originalsList = services.filter((s) => s.status === 'originals_only')

    return (
      <div className="flex flex-wrap items-center gap-1.5">
        {services.slice(0, 5).map((svc) => {
          if (svc.status === 'unlocked') {
            const isNetflix = svc.name === 'netflix'
            const isDisney = svc.name === 'disney_plus'
            const isChatgpt = svc.name === 'chatgpt'
            const isYT = svc.name === 'youtube'
            const isAmazon = svc.name === 'amazon'
            const isTikTok = svc.name === 'tiktok'

            const colorClass = isNetflix
              ? 'bg-[#E50914] text-white'
              : isDisney
                ? 'bg-[#113CCF] text-white'
                : isChatgpt
                  ? 'bg-[#10A37F] text-white'
                  : isYT
                    ? 'bg-[#FF0000] text-white'
                    : isAmazon
                      ? 'bg-[#00A8E1] text-white'
                      : isTikTok
                        ? 'bg-neutral-900 text-white'
                        : 'badge-primary text-primary-content'

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
          return null
        })}
        {services.length > 5 && (
          <span className="badge badge-ghost badge-sm text-[10px] text-base-content/50">
            +{services.length - 5}
          </span>
        )}
        {unlockedList.length === 0 && originalsList.length === 0 && (
          <span className="text-xs text-base-content/40">无完全解锁项</span>
        )}
      </div>
    )
  }

  const rowTint = isSelected ? 'bg-primary/8' : ''

  return (
    <tr
      className={cn('cursor-pointer transition-colors hover:bg-base-200/50', rowTint)}
      onClick={onClick}
    >
      <td className="text-center font-mono text-xs text-base-content/40">
        {checking ? (
          <span className="loading loading-spinner loading-xs text-primary" />
        ) : (
          index + 1
        )}
      </td>
      <td className="max-w-[220px]">
        <div className="flex items-center gap-2">
          <span className="text-lg leading-none shrink-0" title={node.region || node.country}>
            {regionFlag(node.country || node.region || '')}
          </span>
          <div className="min-w-0 flex-1">
            <div className="truncate font-semibold text-base-content" title={node.name}>
              {node.name}
            </div>
            {node.tags && node.tags.length > 0 && (
              <div className="mt-0.5 flex flex-wrap gap-1">
                {node.tags.slice(0, 2).map((tag) => (
                  <span key={tag} className="badge badge-ghost badge-xs text-[10px] text-base-content/50">
                    {tag}
                  </span>
                ))}
              </div>
            )}
          </div>
        </div>
      </td>
      <td className="w-24">
        {node.last_latency_ms > 0 ? (
          <span
            className={cn(
              'font-mono text-xs font-semibold tabular-nums',
              node.last_latency_ms <= 100
                ? 'text-success'
                : node.last_latency_ms <= 300
                  ? 'text-warning'
                  : 'text-error',
            )}
          >
            {node.last_latency_ms} ms
          </span>
        ) : (
          <span className="font-mono text-xs text-base-content/35">—</span>
        )}
      </td>
      <td className="max-w-[200px]">
        {result?.ip?.ip ? (
          <div className="flex flex-col min-w-0">
            <span className="font-mono text-xs font-bold text-base-content/85">{result.ip.ip}</span>
            <span
              className="truncate text-[10px] text-base-content/50"
              title={`${result.ip.iso_code || ''} ${result.ip.asn || ''} ${result.ip.org || ''}`}
            >
              {result.ip.iso_code} {result.ip.asn} {result.ip.org}
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
          disabled={checking}
          title={result ? '重新测试该节点解锁与 IP 属性' : '立即测试该节点'}
        >
          {checking ? <span className="loading loading-spinner loading-xs" /> : result ? '重测' : '检测'}
        </button>
      </td>
    </tr>
  )
}
