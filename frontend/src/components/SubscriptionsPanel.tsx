import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { Subscription, SubscriptionPayload } from '../types'
import {
  activateSubscription,
  createSubscription,
  deleteSubscription,
  fetchSubscriptionStatus,
  listSubscriptions,
  refreshOneSubscription,
  refreshSubscription,
  toggleSubscription,
  updateSubscription,
} from '../api/client'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import { toast } from 'sonner'
import { cn } from '../utils/cn'
import { Rss, RefreshCw } from 'lucide-react'

export default function SubscriptionsPanel() {
  const queryClient = useQueryClient()

  const { data: subData, isLoading: subLoading } = useQuery({ queryKey: ['subscriptions'], queryFn: listSubscriptions })
  const { data: status, isLoading: statusLoading } = useQuery({ queryKey: ['subscriptionStatus'], queryFn: fetchSubscriptionStatus })

  const subscriptions = subData?.subscriptions || []
  const loading = subLoading || statusLoading

  const [action, setAction] = useState<string | null>(null)
  const [newName, setNewName] = useState('')
  const [newUrl, setNewUrl] = useState('')
  const [editing, setEditing] = useState<Subscription | null>(null)
  const [editName, setEditName] = useState('')
  const [editUrl, setEditUrl] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<Subscription | null>(null)

  const payloadFor = (name: string, url: string, current?: Subscription): SubscriptionPayload => ({
    name: name.trim(), url: url.trim(), enabled: current?.enabled ?? true,
    refresh_interval_seconds: current?.refresh_interval_seconds ?? 0,
    refresh_timeout_seconds: current?.refresh_timeout_seconds ?? 0,
    sort_order: current?.sort_order ?? subscriptions.length,
  })

  const runAction = async (key: string, operation: () => Promise<unknown>, message: string) => {
    setAction(key)
    try {
      await operation()
      queryClient.invalidateQueries({ queryKey: ['subscriptions'] })
      queryClient.invalidateQueries({ queryKey: ['subscriptionStatus'] })
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      toast.success(message)
      return true
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '订阅操作失败')
      return false
    } finally {
      setAction(null)
    }
  }

  const addSubscription = async () => {
    if (!newName.trim() || !newUrl.trim()) return
    if (await runAction('create', () => createSubscription(payloadFor(newName, newUrl)), '订阅已添加')) {
      setNewName('')
      setNewUrl('')
    }
  }

  const startEditing = (subscription: Subscription) => {
    setEditing(subscription)
    setEditName(subscription.name)
    setEditUrl(subscription.url)
  }

  const saveSubscription = async () => {
    if (!editing || !editName.trim() || !editUrl.trim()) return
    if (await runAction(`edit-${editing.id}`, () => updateSubscription(editing.id, payloadFor(editName, editUrl, editing)), '订阅已更新')) setEditing(null)
  }

  const enabledCount = subscriptions.filter((subscription) => subscription.enabled).length
  const nodeCount = subscriptions.reduce((total, subscription) => total + subscription.node_count, 0)
  const errorCount = subscriptions.filter((subscription) => subscription.last_error).length
  const busy = action !== null || status?.is_refreshing

  if (loading) return <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>

  return (
    <PageLayout>
      <PageHeader
        title="订阅管理"
        description="增量同步订阅源；旧节点不会因链接或上游内容变化而丢失"
        icon={<Rss className="h-5 w-5" />}
        actions={<button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" disabled={busy || subscriptions.length === 0} onClick={() => void runAction('refresh-all', refreshSubscription, '全部订阅刷新完成')} title="刷新全部订阅" aria-label="刷新全部订阅">
            {action === 'refresh-all' || status?.is_refreshing ? <span className="loading loading-spinner loading-sm" /> : null}
            {action !== 'refresh-all' && !status?.is_refreshing && <RefreshCw className="h-4 w-4 sm:hidden" />}
            <span className="hidden sm:inline">全量刷新</span>
          </button>}
      />

      <PageContent>
        {status?.last_error && <div role="alert" className="alert alert-warning alert-soft"><span>最近同步错误：{status.last_error}</span></div>}

        <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">订阅总数</div><div className="mt-2 text-3xl font-black tabular-nums text-primary">{subscriptions.length}</div><div className="mt-2 text-xs text-base-content/40">已配置的订阅源</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">已启用</div><div className="mt-2 text-3xl font-black tabular-nums text-success">{enabledCount}</div><div className="mt-2 text-xs text-base-content/40">参与运行时同步</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">节点总数</div><div className="mt-2 text-3xl font-black tabular-nums">{nodeCount}</div><div className="mt-2 text-xs text-base-content/40">累计保留成员</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">异常订阅</div><div className={cn("mt-2 text-3xl font-black tabular-nums", errorCount ? 'text-error' : 'text-base-content')}>{errorCount}</div><div className="mt-2 text-xs text-base-content/40">最近同步状态</div></div>
        </div>

        <section className={cn(surfaceClass, "p-5 lg:p-6")}>
          <div className="mb-5 flex flex-wrap items-center justify-between gap-3 border-b border-base-200 pb-4">
            <div><h3 className="text-lg font-bold">添加订阅源</h3><p className="mt-0.5 text-xs text-base-content/50">填写名称和订阅地址，添加后会立即同步</p></div>
            <span className={cn("badge", status?.enabled ? 'badge-success' : 'badge-ghost')}>{status?.enabled ? '自动刷新已开启' : '自动刷新未开启'}</span>
          </div>
          <div className="grid items-end gap-4 lg:grid-cols-[minmax(0,0.7fr)_minmax(0,1.3fr)_auto]">
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">订阅名称</legend><input className={cn(`input input-md w-full`, controlClass)} placeholder="例如：主力节点" value={newName} onChange={(event) => setNewName(event.target.value)} /></fieldset>
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">订阅地址</legend><input type="url" className={cn(`input input-md w-full font-mono text-sm`, controlClass)} placeholder="https://example.com/subscribe" value={newUrl} onChange={(event) => setNewUrl(event.target.value)} onKeyDown={(event) => event.key === 'Enter' && void addSubscription()} /></fieldset>
            <button className="btn btn-primary btn-md lg:min-w-28" disabled={!newName.trim() || !newUrl.trim() || busy} onClick={() => void addSubscription()}>{action === 'create' && <span className="loading loading-spinner loading-sm" />}添加订阅</button>
          </div>
        </section>

        <section className={cn(surfaceClass, "overflow-hidden")}>
          <div className="flex items-center justify-between border-b border-base-200 px-5 py-4 lg:px-6"><div><h3 className="text-lg font-bold">订阅源列表</h3><p className="mt-0.5 text-xs text-base-content/50">刷新采用增量合并，缺失节点将继续保留</p></div><span className="badge badge-ghost">{subscriptions.length} 项</span></div>
          {subscriptions.length ? <div className="space-y-3 p-4 lg:p-6">{subscriptions.map((subscription) => (
            <article key={subscription.id} className={cn(`rounded-xl border bg-base-200/30 p-4`, subscription.enabled ? 'border-base-300' : 'border-base-200 opacity-70')}>
              {editing?.id === subscription.id ? (
                <div className="grid gap-3 sm:grid-cols-[minmax(0,0.7fr)_minmax(0,1.3fr)_auto]">
                  <input className="input input-sm w-full" value={editName} onChange={(event) => setEditName(event.target.value)} />
                  <input className="input input-sm w-full font-mono text-xs" value={editUrl} onChange={(event) => setEditUrl(event.target.value)} />
                  <div className="flex gap-2"><button className="btn btn-primary btn-sm" disabled={!editName.trim() || !editUrl.trim() || busy} onClick={() => void saveSubscription()}>保存</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => setEditing(null)}>取消</button></div>
                </div>
              ) : (
                <div className="flex flex-col gap-4 lg:flex-row lg:items-center">
                  <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><strong>{subscription.name}</strong><span className={cn("badge badge-sm", subscription.enabled ? 'badge-success' : 'badge-ghost')}>{subscription.enabled ? '已启用' : '已禁用'}</span><span className="badge badge-outline badge-sm">{subscription.node_count} 节点</span></div><code className="mt-1 block break-all text-xs text-base-content/55">{subscription.url}</code><div className="mt-2 flex flex-wrap gap-x-4 text-xs text-base-content/55"><span>最近成功：{subscription.last_success && !subscription.last_success.startsWith('0001-') ? new Date(subscription.last_success).toLocaleString() : '尚未成功'}</span>{subscription.last_error && <span className="break-all text-error">错误：{subscription.last_error}</span>}</div></div>
                  <div className="flex flex-wrap gap-2 lg:justify-end"><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => startEditing(subscription)}>编辑</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => void runAction(`toggle-${subscription.id}`, () => toggleSubscription(subscription.id, !subscription.enabled), subscription.enabled ? '订阅已禁用' : '订阅已启用')}>{action === `toggle-${subscription.id}` && <span className="loading loading-spinner loading-xs" />}{subscription.enabled ? '禁用' : '启用'}</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => void runAction(`refresh-${subscription.id}`, () => refreshOneSubscription(subscription.id), `${subscription.name} 刷新完成`)}>{action === `refresh-${subscription.id}` && <span className="loading loading-spinner loading-xs" />}刷新</button><button className="btn btn-ghost btn-sm text-primary" disabled={busy || (subscription.enabled && enabledCount === 1)} onClick={() => void runAction(`activate-${subscription.id}`, () => activateSubscription(subscription.id), `已独占启用 ${subscription.name}`)}>独占启用</button><button className="btn btn-ghost btn-sm text-error" disabled={busy} onClick={() => setDeleteTarget(subscription)}>删除</button></div>
                </div>
              )}
              {deleteTarget?.id === subscription.id && <div className="alert alert-warning mt-3 flex-col items-start sm:flex-row sm:items-center"><span>确认删除订阅“{subscription.name}”？其独占节点将保留为手动节点并继续运行。</span><div className="flex gap-2 sm:ml-auto"><button className="btn btn-error btn-sm" disabled={busy} onClick={() => void runAction(`delete-${subscription.id}`, () => deleteSubscription(subscription.id), '订阅已删除，节点已保留').then((deleted) => deleted && setDeleteTarget(null))}>确认删除</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => setDeleteTarget(null)}>取消</button></div></div>}
            </article>
          ))}</div> : <div className="m-4 rounded-xl border border-dashed border-base-300 bg-base-200/20 px-4 py-12 text-center lg:m-6"><p className="font-medium text-base-content/60">暂无订阅链接</p><p className="mt-1 text-sm text-base-content/40">在上方添加节点订阅地址</p></div>}
        </section>
      </PageContent>
    </PageLayout>
  )
}
