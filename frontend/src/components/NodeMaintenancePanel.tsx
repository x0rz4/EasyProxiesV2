import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { MaintenanceNode, NodeMaintenanceStatus } from '../types'
import { batchDeleteConfigNodes, fetchNodeMaintenance } from '../api/client'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import { toast } from 'sonner'
import { cn } from '../utils/cn'
import {
  AlertTriangle, CheckCircle2, HeartPulse, RefreshCw, Search, Trash2, WifiOff, XCircle,
} from 'lucide-react'

/** Quick picks offered next to the manual offline-days threshold. */
const DAY_PRESETS = [7, 30, 60]

const statusMeta: Record<NodeMaintenanceStatus, { label: string; badge: string }> = {
  online: { label: '在线', badge: 'badge-success' },
  offline: { label: '离线', badge: 'badge-error' },
  pending: { label: '待检测', badge: 'badge-warning' },
  disabled: { label: '已禁用', badge: 'badge-ghost' },
}

function sourceLabel(source: string): string {
  switch (source) {
    case 'inline': return '配置文件'
    case 'nodes_file': return '节点文件'
    case 'subscription': return '订阅'
    case 'manual': return '手动添加'
    default: return source || '-'
  }
}

/** Renders an offline streak: hours below a day, days above. */
function formatOfflineDays(days: number): string {
  if (days < 1) return `${Math.max(1, Math.round(days * 24))} 小时`
  if (days < 100) return `${days.toFixed(1)} 天`
  return `${Math.round(days)} 天`
}

function formatTime(value?: string): string {
  if (!value || value.startsWith('0001-')) return '尚未成功'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString()
}

function matchesSearch(node: MaintenanceNode, keyword: string): boolean {
  if (!keyword) return true
  const needle = keyword.trim().toLowerCase()
  if (!needle) return true
  return [node.name, node.uri, node.region, node.country, node.source, ...(node.tags ?? [])]
    .filter(Boolean)
    .some((field) => String(field).toLowerCase().includes(needle))
}

export default function NodeMaintenancePanel() {
  const queryClient = useQueryClient()
  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ['nodeMaintenance'],
    queryFn: fetchNodeMaintenance,
  })

  const [thresholdInput, setThresholdInput] = useState('7')
  const [search, setSearch] = useState('')
  const [onlyStale, setOnlyStale] = useState(true)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const nodes = data?.nodes ?? []
  const summary = data?.summary

  // Manual input wins; an empty or non-positive value simply matches nothing.
  const parsedThreshold = Number.parseFloat(thresholdInput)
  const thresholdDays = Number.isFinite(parsedThreshold) && parsedThreshold > 0 ? parsedThreshold : 0

  const staleNodes = nodes.filter((node) => node.status === 'offline' && node.offline_days >= thresholdDays)
  const visibleNodes = (onlyStale ? staleNodes : nodes).filter((node) => matchesSearch(node, search))
  // Selection is kept across filter changes, but only rows still on screen can
  // be acted on — the same rule the node management page uses.
  const selectedVisible = visibleNodes.filter((node) => selected.has(node.id))
  const allVisibleSelected = visibleNodes.length > 0 && selectedVisible.length === visibleNodes.length
  const selectedOnline = selectedVisible.filter((node) => node.status === 'online').length

  const toggleSelectNode = (id: number) => {
    setSelected((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleSelectAllVisible = () => {
    setSelected((current) => {
      const next = new Set(current)
      if (allVisibleSelected) {
        for (const node of visibleNodes) next.delete(node.id)
      } else {
        for (const node of visibleNodes) next.add(node.id)
      }
      return next
    })
  }

  const handleDelete = async () => {
    const ids = selectedVisible.map((node) => node.id)
    if (ids.length === 0) return
    setDeleting(true)
    try {
      const result = await batchDeleteConfigNodes(ids)
      if (result.errors?.length) {
        toast.warning(`${result.message}，${result.errors.length} 个删除失败`, {
          description: result.errors.slice(0, 3).join('；'),
        })
      } else {
        toast.success(result.message || `已删除 ${result.success} 个节点`)
      }
      setSelected(new Set())
      setConfirmOpen(false)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['nodeMaintenance'] }),
        queryClient.invalidateQueries({ queryKey: ['configNodes'] }),
        queryClient.invalidateQueries({ queryKey: ['nodes'] }),
        queryClient.invalidateQueries({ queryKey: ['nodeQuery'] }),
        queryClient.invalidateQueries({ queryKey: ['nodeFacets'] }),
      ])
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '删除节点失败')
    } finally {
      setDeleting(false)
    }
  }

  const statCards = [
    { label: '节点总数', value: summary?.total ?? nodes.length, tone: 'text-base-content', hint: '与本机节点管理一致' },
    { label: '在线', value: summary?.online ?? 0, tone: 'text-success', hint: '最近一次健康检查通过' },
    { label: '离线', value: summary?.offline ?? 0, tone: 'text-error', hint: '已完成检查但当前不可用' },
    { label: `离线 ≥ ${thresholdInput || '?'} 天`, value: staleNodes.length, tone: 'text-warning', hint: '符合当前阈值的无效节点' },
  ]

  return (
    <PageLayout>
      <PageHeader
        title="节点维护"
        description="查看节点在线/离线状态，按连续离线天数筛出无效节点并批量清理"
        icon={<HeartPulse className="h-5 w-5" />}
        actions={<>
          <button
            className="btn btn-ghost btn-sm gap-2 border border-base-300 lg:btn-md"
            disabled={isFetching}
            onClick={() => void refetch()}
            title="刷新节点状态"
            aria-label="刷新节点状态"
          >
            <RefreshCw className={cn('h-4 w-4', isFetching && 'animate-spin')} />
            <span className="hidden sm:inline">刷新</span>
          </button>
          <button
            className="btn btn-error btn-sm gap-2 shadow-sm lg:btn-md"
            disabled={selectedVisible.length === 0 || deleting}
            onClick={() => setConfirmOpen(true)}
            title="删除所选节点"
            aria-label="删除所选节点"
          >
            <Trash2 className="h-4 w-4" />
            <span className="hidden sm:inline">删除所选</span>
            {selectedVisible.length > 0 && <span className="badge badge-sm">{selectedVisible.length}</span>}
          </button>
        </>}
      />

      <PageContent>
        {isLoading ? (
          <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>
        ) : (
          <>
            <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
              {statCards.map((card) => (
                <div key={card.label} className={cn(surfaceClass, 'p-5')}>
                  <div className="text-sm font-medium text-base-content/55">{card.label}</div>
                  <div className={cn('mt-2 text-3xl font-black tabular-nums', card.tone)}>{card.value}</div>
                  <div className="mt-2 text-xs text-base-content/40">{card.hint}</div>
                </div>
              ))}
            </div>

            <p className="px-1 text-xs text-base-content/50">
              待检测 {summary?.pending ?? 0} · 已禁用 {summary?.disabled ?? 0}（禁用与待检测节点不参与离线判定）；
              在线/离线状态来自持久化健康统计，最多滞后约 30 秒。
            </p>

            {/* ---- 筛选 ---- */}
            <section className={cn(surfaceClass, 'p-5 lg:p-6')}>
              <div className="mb-4 flex flex-wrap items-center justify-between gap-3 border-b border-base-200 pb-4">
                <div>
                  <h3 className="text-lg font-bold">筛选条件</h3>
                  <p className="mt-0.5 text-xs text-base-content/50">
                    离线天数 = 距最后一次健康检查成功的时间；从未成功过的节点从入库时间算起
                  </p>
                </div>
                {selectedVisible.length > 0 && (
                  <button className="btn btn-ghost btn-sm" onClick={() => setSelected(new Set())}>清除选择</button>
                )}
              </div>

              <div className="grid items-end gap-4 lg:grid-cols-3">
                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">离线天数阈值</legend>
                  <div className="flex flex-wrap items-center gap-2">
                    <input
                      type="number"
                      min={1}
                      step={1}
                      inputMode="numeric"
                      className={cn('input input-md w-28', controlClass)}
                      value={thresholdInput}
                      onChange={(event) => setThresholdInput(event.target.value)}
                      aria-label="离线天数阈值"
                    />
                    {DAY_PRESETS.map((preset) => (
                      <button
                        key={preset}
                        className={cn('btn btn-sm', thresholdDays === preset ? 'btn-primary' : 'btn-ghost border border-base-300')}
                        onClick={() => setThresholdInput(String(preset))}
                      >
                        {preset} 天
                      </button>
                    ))}
                  </div>
                </fieldset>

                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">关键词</legend>
                  <label className={cn('input input-md w-full', controlClass)}>
                    <Search className="h-4 w-4 opacity-50" />
                    <input
                      type="text"
                      placeholder="名称 / 地区 / URI / 标签"
                      className="grow"
                      value={search}
                      onChange={(event) => setSearch(event.target.value)}
                    />
                  </label>
                </fieldset>

                <label className="flex cursor-pointer items-center justify-between gap-4 rounded-xl border border-base-200 bg-base-200/30 p-4 transition-colors hover:border-base-300">
                  <div>
                    <span className="block font-semibold text-base-content/90">仅显示需清理节点</span>
                    <p className="m-0 text-xs text-base-content/50">关闭后列出全部节点及其在线状态</p>
                  </div>
                  <input
                    type="checkbox"
                    className="toggle toggle-primary toggle-md"
                    checked={onlyStale}
                    onChange={(event) => setOnlyStale(event.target.checked)}
                    aria-label="仅显示需清理节点"
                  />
                </label>
              </div>
            </section>

            {/* ---- 列表 ---- */}
            <section className={cn(surfaceClass, 'overflow-hidden')}>
              <div className="flex items-center justify-between border-b border-base-200 px-5 py-4 lg:px-6">
                <div>
                  <h3 className="text-lg font-bold">{onlyStale ? '待清理节点' : '全部节点'}</h3>
                  <p className="mt-0.5 text-xs text-base-content/50">
                    {onlyStale
                      ? `已连续离线达到 ${thresholdInput || '?'} 天的节点，按离线时长降序`
                      : '按离线时长降序，其余按名称排序'}
                  </p>
                </div>
                <span className={cn('badge', visibleNodes.length > 0 ? 'badge-warning' : 'badge-ghost')}>{visibleNodes.length} 项</span>
              </div>

              <div className="overflow-x-auto">
                <table className="table table-md">
                  <thead>
                    <tr className="bg-base-200/50 text-base-content/70">
                      <th className="w-10">
                        <input
                          type="checkbox"
                          className="checkbox checkbox-xs"
                          checked={allVisibleSelected}
                          onChange={toggleSelectAllVisible}
                          disabled={visibleNodes.length === 0}
                          aria-label="全选当前列表"
                          ref={(el) => {
                            if (el) el.indeterminate = selectedVisible.length > 0 && !allVisibleSelected
                          }}
                        />
                      </th>
                      <th className="font-semibold">名称</th>
                      <th className="font-semibold">状态</th>
                      <th className="font-semibold">连续离线</th>
                      <th className="hidden font-semibold md:table-cell">最后在线</th>
                      <th className="hidden font-semibold lg:table-cell">失败次数</th>
                      <th className="hidden font-semibold xl:table-cell">最近错误</th>
                    </tr>
                  </thead>
                  <tbody>
                    {visibleNodes.length === 0 ? (
                      <tr>
                        <td colSpan={7} className="h-56 p-0">
                          <div className="flex h-full w-full flex-col items-center justify-center opacity-60">
                            <div className="mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-base-200">
                              <CheckCircle2 className="h-8 w-8 text-success/70" />
                            </div>
                            <p className="text-base font-medium text-base-content">
                              {onlyStale
                                ? `没有连续离线达到 ${thresholdInput || '?'} 天的节点`
                                : '没有匹配的节点'}
                            </p>
                            <p className="mt-1 text-sm text-base-content/50">
                              {onlyStale ? '可以调小阈值或关闭「仅显示需清理节点」查看全部节点' : '请调整关键词后重试'}
                            </p>
                          </div>
                        </td>
                      </tr>
                    ) : (
                      visibleNodes.map((node) => {
                        const meta = statusMeta[node.status]
                        const checked = selected.has(node.id)
                        return (
                          <tr key={node.id} className={cn('hover:bg-base-200/40', checked && 'bg-primary/5')}>
                            <td>
                              <input
                                type="checkbox"
                                className="checkbox checkbox-sm checkbox-error"
                                checked={checked}
                                onChange={() => toggleSelectNode(node.id)}
                                aria-label={`选择 ${node.name}`}
                              />
                            </td>
                            <td className="max-w-[22rem]">
                              <div className="flex flex-wrap items-center gap-2">
                                <strong className="truncate">{node.name}</strong>
                                <span className="badge badge-ghost badge-sm">{sourceLabel(node.source)}</span>
                                {(node.region || node.country) && (
                                  <span className="badge badge-outline badge-sm">{node.region || node.country}</span>
                                )}
                                {!node.enabled && <span className="badge badge-sm">已禁用</span>}
                              </div>
                              <code className="mt-0.5 block break-all text-xs text-base-content/50">{node.uri}</code>
                            </td>
                            <td>
                              <span className={cn('badge gap-1', meta.badge)}>
                                {node.status === 'online' ? <CheckCircle2 className="h-3 w-3" /> : null}
                                {node.status === 'offline' ? <WifiOff className="h-3 w-3" /> : null}
                                {node.status === 'pending' ? <AlertTriangle className="h-3 w-3" /> : null}
                                {node.status === 'disabled' ? <XCircle className="h-3 w-3" /> : null}
                                {meta.label}
                              </span>
                              {node.blacklisted && <span className="badge badge-warning badge-sm ml-1">黑名单</span>}
                            </td>
                            <td className="tabular-nums">
                              {node.status === 'offline' ? (
                                <span className="font-semibold text-error">{formatOfflineDays(node.offline_days)}</span>
                              ) : (
                                <span className="text-base-content/40">—</span>
                              )}
                            </td>
                            <td className="hidden text-xs text-base-content/60 md:table-cell">{formatTime(node.last_success_at)}</td>
                            <td className="hidden tabular-nums lg:table-cell">
                              {node.failure_count}
                              {node.success_count > 0 && <span className="text-xs text-base-content/40"> / 成功 {node.success_count}</span>}
                            </td>
                            <td className="hidden max-w-[18rem] truncate text-xs text-base-content/60 xl:table-cell" title={node.last_error || ''}>
                              {node.last_error || '—'}
                            </td>
                          </tr>
                        )
                      })
                    )}
                  </tbody>
                </table>
              </div>
            </section>

            {/* ---- 底部操作条 ---- */}
            {selectedVisible.length > 0 && (
              <div className={cn(surfaceClass, 'flex flex-wrap items-center justify-between gap-3 p-4')}>
                <span className="text-sm text-base-content/70">
                  已选择 <strong className="text-error">{selectedVisible.length}</strong> 个节点
                  {selectedOnline > 0 && <span className="ml-2 text-warning">其中 {selectedOnline} 个当前在线</span>}
                </span>
                <div className="flex gap-2">
                  <button className="btn btn-ghost btn-sm" onClick={() => setSelected(new Set())}>取消选择</button>
                  <button className="btn btn-error btn-sm" disabled={deleting} onClick={() => setConfirmOpen(true)}>
                    <Trash2 className="h-4 w-4" />删除所选节点
                  </button>
                </div>
              </div>
            )}
          </>
        )}

        {/* ---- 二次确认 ---- */}
        {confirmOpen && (
          <div className="modal modal-open">
            <div className="modal-box max-w-lg">
              <h3 className="mb-2 flex items-center gap-2 text-lg font-bold">
                <AlertTriangle className="h-5 w-5 text-error" />确认删除节点
              </h3>
              <p className="text-base-content/70">
                即将删除 <strong className="text-error">{selectedVisible.length}</strong> 个节点，
                其中连续离线达到 {thresholdInput || '?'} 天及以上的有{' '}
                <strong>{selectedVisible.filter((node) => node.status === 'offline' && node.offline_days >= thresholdDays).length}</strong> 个。
              </p>
              {selectedOnline > 0 && (
                <div role="alert" className="alert alert-warning alert-soft mt-3 text-sm">
                  <AlertTriangle className="h-4 w-4 shrink-0" />
                  <span>所选中有 {selectedOnline} 个节点当前处于在线状态，删除后其流量将立即中断。</span>
                </div>
              )}
              <ul className="mt-3 max-h-40 space-y-1 overflow-y-auto rounded-xl border border-base-200 bg-base-200/30 p-3 text-xs">
                {selectedVisible.slice(0, 8).map((node) => (
                  <li key={node.id} className="flex items-center justify-between gap-3">
                    <span className="truncate font-medium">{node.name}</span>
                    <span className="shrink-0 text-base-content/50">
                      {node.status === 'offline' ? `离线 ${formatOfflineDays(node.offline_days)}` : statusMeta[node.status].label}
                    </span>
                  </li>
                ))}
                {selectedVisible.length > 8 && (
                  <li className="text-base-content/50">…等共 {selectedVisible.length} 个节点</li>
                )}
              </ul>
              <p className="mt-3 text-sm text-error">此操作不可撤销，节点将从数据库与运行时一并移除。</p>
              <div className="modal-action">
                <button className="btn btn-ghost" disabled={deleting} onClick={() => setConfirmOpen(false)}>取消</button>
                <button className="btn btn-error" disabled={deleting} onClick={() => void handleDelete()}>
                  {deleting ? <span className="loading loading-spinner loading-xs" /> : <Trash2 className="h-4 w-4" />}
                  确认删除 {selectedVisible.length} 个节点
                </button>
              </div>
            </div>
            <form method="dialog" className="modal-backdrop" onClick={() => !deleting && setConfirmOpen(false)}>
              <button>close</button>
            </form>
          </div>
        )}
      </PageContent>
    </PageLayout>
  )
}
