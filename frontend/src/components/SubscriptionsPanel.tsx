import { useState } from 'react'
import QueryError from './ui/QueryError'
import useUnsavedChanges from '../hooks/useUnsavedChanges'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { APIKey, Subscription, SubscriptionFormat, SubscriptionPayload, SubscriptionRefreshSettings } from '../types'
import {
  activateSubscription,
  createAPIKey,
  createSubscription,
  deleteAPIKey,
  deleteSubscription,
  fetchSubscriptionRefreshSettings,
  fetchSubscriptionStatus,
  listAPIKeys,
  listSubscriptions,
  refreshOneSubscription,
  refreshSubscription,
  toggleSubscription,
  updateSubscription,
  updateSubscriptionRefreshSettings,
} from '../api/client'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import { toast } from 'sonner'
import { cn } from '../utils/cn'
import { Clock, Copy, KeyRound, Rss, RefreshCw, Save, Trash2 } from 'lucide-react'

const defaultRefreshSettings: SubscriptionRefreshSettings = {
  sub_refresh_enabled: false,
  sub_refresh_interval: '1h0m0s',
  sub_refresh_timeout: '30s',
  sub_refresh_health_check_timeout: '1m0s',
  sub_refresh_drain_timeout: '30s',
  sub_refresh_min_available_nodes: 1,
}

const apiBaseUrl = typeof window === 'undefined' ? '' : window.location.origin

const curlSubscriptionExample = `curl -X POST '${apiBaseUrl}/api/public/subscriptions' \\
  -H 'X-API-Key: <API_KEY>' \\
  -H 'Content-Type: application/json' \\
  -d '{"name":"外部订阅","url":"https://example.com/subscribe","format":"auto","enabled":true}'`

const curlImportExample = `curl -X POST '${apiBaseUrl}/api/public/import' \\
  -H 'X-API-Key: <API_KEY>' \\
  -H 'Content-Type: application/json' \\
  -d '{"content":"trojan://password@example.com:443#demo"}'`

function maskToken(token: string): string {
  if (token.length <= 14) return token
  return `${token.slice(0, 8)}…${token.slice(-4)}`
}

export default function SubscriptionsPanel() {
  const queryClient = useQueryClient()

  const { data: status, isLoading: statusLoading, error: statusError } = useQuery({
    queryKey: ['subscriptionStatus'],
    queryFn: fetchSubscriptionStatus,
    // A create/update or manual refresh runs server-side after the response, so
    // poll while anything is syncing and stop as soon as it settles.
    refetchInterval: (query) => (query.state.data?.is_refreshing ? 2000 : false),
  })
  const { data: subData, isLoading: subLoading, error: subscriptionsError } = useQuery({
    queryKey: ['subscriptions'],
    queryFn: listSubscriptions,
    refetchInterval: status?.is_refreshing ? 2000 : false,
  })

  const subscriptions = subData?.subscriptions || []
  const loading = subLoading || statusLoading
  const refreshingIds = new Set(status?.refreshing_ids ?? [])

  const [action, setAction] = useState<string | null>(null)
  const [newName, setNewName] = useState('')
  const [newUrl, setNewUrl] = useState('')
  const [newFormat, setNewFormat] = useState<SubscriptionFormat>('auto')
  const [newUserAgent, setNewUserAgent] = useState('')
  const [editing, setEditing] = useState<Subscription | null>(null)
  const [editName, setEditName] = useState('')
  const [editUrl, setEditUrl] = useState('')
  const [editFormat, setEditFormat] = useState<SubscriptionFormat>('auto')
  const [editUserAgent, setEditUserAgent] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<Subscription | null>(null)

  // --- Subscription auto-refresh settings (migrated from the settings page) ---
  const { data: refreshData, isLoading: refreshLoading, error: refreshError } = useQuery({
    queryKey: ['subRefreshSettings'],
    queryFn: fetchSubscriptionRefreshSettings,
  })
  const [refreshDraft, setRefreshDraft] = useState<SubscriptionRefreshSettings | null>(null)
  const [savingRefresh, setSavingRefresh] = useState(false)
  const refreshSettings = refreshDraft ?? refreshData ?? defaultRefreshSettings
  const refreshDirty = refreshDraft !== null && refreshData !== undefined
    && JSON.stringify(refreshDraft) !== JSON.stringify(refreshData)
  useUnsavedChanges(refreshDirty)

  const updateRefreshField = <K extends keyof SubscriptionRefreshSettings>(key: K, value: SubscriptionRefreshSettings[K]) => {
    setRefreshDraft({ ...(refreshDraft ?? refreshData ?? defaultRefreshSettings), [key]: value })
  }

  // --- Session-free API credentials ---
  const { data: keyData, error: keysError } = useQuery({ queryKey: ['apiKeys'], queryFn: listAPIKeys })
  const apiKeys = keyData?.api_keys || []
  const [newKeyName, setNewKeyName] = useState('')
  const [keyAction, setKeyAction] = useState<string | null>(null)
  const [freshToken, setFreshToken] = useState('')
  const [revokeTarget, setRevokeTarget] = useState<APIKey | null>(null)

  const copyText = async (text: string, label: string) => {
    try {
      await navigator.clipboard.writeText(text)
      toast.success(`${label}已复制到剪贴板`)
    } catch {
      toast.error('复制失败，请手动选择并复制')
    }
  }

  const saveRefreshSettings = async () => {
    setSavingRefresh(true)
    try {
      const res = await updateSubscriptionRefreshSettings(refreshSettings)
      toast.success(res.reloaded ? '订阅自动刷新设置已保存并重载' : (res.message || '订阅自动刷新设置已保存'))
      if (res.reload_error) toast.warning(`设置已保存，但自动重载失败：${res.reload_error}`)
      setRefreshDraft(null)
      void queryClient.invalidateQueries({ queryKey: ['subRefreshSettings'] })
      void queryClient.invalidateQueries({ queryKey: ['subscriptionStatus'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '保存订阅自动刷新设置失败')
    } finally {
      setSavingRefresh(false)
    }
  }

  const generateAPIKey = async () => {
    setKeyAction('create')
    try {
      const created = await createAPIKey(newKeyName.trim())
      setFreshToken(created.token)
      setNewKeyName('')
      toast.success('API Key 已生成')
      void queryClient.invalidateQueries({ queryKey: ['apiKeys'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '生成 API Key 失败')
    } finally {
      setKeyAction(null)
    }
  }

  const revokeAPIKey = async (key: APIKey) => {
    setKeyAction(`delete-${key.id}`)
    try {
      await deleteAPIKey(key.id)
      toast.success('API Key 已撤销')
      setFreshToken(current => (current === key.token ? '' : current))
      void queryClient.invalidateQueries({ queryKey: ['apiKeys'] })
      setRevokeTarget(null)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '撤销 API Key 失败')
    } finally {
      setKeyAction(null)
    }
  }

  const payloadFor = (name: string, url: string, format: SubscriptionFormat, userAgent: string, current?: Subscription): SubscriptionPayload => ({
    name: name.trim(), url: url.trim(), enabled: current?.enabled ?? true,
    format, user_agent: userAgent.trim(),
    refresh_interval_seconds: current?.refresh_interval_seconds ?? 0,
    refresh_timeout_seconds: current?.refresh_timeout_seconds ?? 0,
    sort_order: current?.sort_order ?? subscriptions.length,
  })

  const runAction = async (key: string, operation: () => Promise<unknown>, message: string) => {
    setAction(key)
    try {
      const result = await operation()
      queryClient.invalidateQueries({ queryKey: ['subscriptions'] })
      queryClient.invalidateQueries({ queryKey: ['subscriptionStatus'] })
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      const summary = refreshSummary(result)
      toast.success(message, summary ? { description: summary } : undefined)
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
    if (await runAction('create', () => createSubscription(payloadFor(newName, newUrl, newFormat, newUserAgent)), '订阅已添加，正在同步')) {
      setNewName('')
      setNewUrl('')
      setNewFormat('auto')
      setNewUserAgent('')
    }
  }

  const startEditing = (subscription: Subscription) => {
    setEditing(subscription)
    setEditName(subscription.name)
    setEditUrl(subscription.url)
    setEditFormat(subscription.format || 'auto')
    setEditUserAgent(subscription.user_agent || '')
  }

  const saveSubscription = async () => {
    if (!editing || !editName.trim() || !editUrl.trim()) return
    // Repointing at another upstream drops the old members, so say so.
    const urlChanged = editing.url.trim() !== editUrl.trim()
    const message = urlChanged ? '订阅已更新，旧节点已清空并重新同步' : '订阅已更新，正在同步'
    if (await runAction(`edit-${editing.id}`, () => updateSubscription(editing.id, payloadFor(editName, editUrl, editFormat, editUserAgent, editing)), message)) setEditing(null)
  }

  const enabledCount = subscriptions.filter((subscription) => subscription.enabled).length
  const nodeCount = subscriptions.reduce((total, subscription) => total + subscription.node_count, 0)
  const errorCount = subscriptions.filter((subscription) => subscription.last_error).length
  const busy = action !== null || status?.is_refreshing
  const isRefreshingRow = (subscription: Subscription) => refreshingIds.has(subscription.id)

  if (loading) return <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>

  return (
    <PageLayout>
      <PageHeader
        title="订阅管理"
        description="增量同步订阅源；上游连续 2 次未出现的节点会被移除，修改订阅地址会清空旧节点"
        icon={<Rss className="h-5 w-5" />}
        actions={<button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" disabled={busy || subscriptions.length === 0} onClick={() => void runAction('refresh-all', refreshSubscription, '全部订阅刷新完成')} title="刷新全部订阅" aria-label="刷新全部订阅">
            {action === 'refresh-all' || status?.is_refreshing ? <span className="loading loading-spinner loading-sm" /> : null}
            {action !== 'refresh-all' && !status?.is_refreshing && <RefreshCw className="h-4 w-4 sm:hidden" />}
            <span className="hidden sm:inline">全量刷新</span>
          </button>}
      />

      <PageContent>
        <QueryError error={subscriptionsError || statusError || refreshError || keysError} onRetry={() => { void queryClient.invalidateQueries({ queryKey: ['subscriptions'] }); void queryClient.invalidateQueries({ queryKey: ['subscriptionStatus'] }); void queryClient.invalidateQueries({ queryKey: ['subRefreshSettings'] }); void queryClient.invalidateQueries({ queryKey: ['apiKeys'] }) }} />
        {status?.last_error && <div role="alert" className="alert alert-warning alert-soft"><span>最近同步错误：{status.last_error}</span></div>}

        <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">订阅总数</div><div className="mt-2 text-3xl font-black tabular-nums text-primary">{subscriptions.length}</div><div className="mt-2 text-xs text-base-content/40">已配置的订阅源</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">已启用</div><div className="mt-2 text-3xl font-black tabular-nums text-success">{enabledCount}</div><div className="mt-2 text-xs text-base-content/40">参与运行时同步</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">节点总数</div><div className="mt-2 text-3xl font-black tabular-nums">{nodeCount}</div><div className="mt-2 text-xs text-base-content/40">累计保留成员</div></div>
          <div className={cn(surfaceClass, "p-5")}><div className="text-sm font-medium text-base-content/55">异常订阅</div><div className={cn("mt-2 text-3xl font-black tabular-nums", errorCount ? 'text-error' : 'text-base-content')}>{errorCount}</div><div className="mt-2 text-xs text-base-content/40">最近同步状态</div></div>
        </div>

        <section className={cn(surfaceClass, "p-5 lg:p-6")}>
          <div className="mb-5 flex flex-wrap items-center justify-between gap-3 border-b border-base-200 pb-4">
            <div><h3 className="text-lg font-bold">添加订阅源</h3><p className="mt-0.5 text-xs text-base-content/50">填写名称和订阅地址，添加后会立即在后台同步</p></div>
            <span className="badge badge-ghost">{subscriptions.length} 个订阅源</span>
          </div>
          <div className="grid items-end gap-4 lg:grid-cols-2">
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">订阅名称</legend><input aria-label="订阅名称" className={cn(`input input-md w-full`, controlClass)} placeholder="例如：主力节点" value={newName} onChange={(event) => setNewName(event.target.value)} /></fieldset>
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">订阅地址</legend><input aria-label="订阅地址" type="url" className={cn(`input input-md w-full font-mono text-sm`, controlClass)} placeholder="https://example.com/subscribe" value={newUrl} onChange={(event) => setNewUrl(event.target.value)} onKeyDown={(event) => event.key === 'Enter' && void addSubscription()} /></fieldset>
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">响应格式</legend><select aria-label="响应格式" className={cn('select select-md w-full', controlClass)} value={newFormat} onChange={(event) => setNewFormat(event.target.value as SubscriptionFormat)}><option value="auto">自动识别（推荐）</option><option value="clash">Clash / Mihomo YAML</option><option value="base64">Base64 URI 列表</option><option value="sing-box">Sing-box JSON</option></select></fieldset>
            <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">自定义 User-Agent</legend><input aria-label="自定义 User-Agent" className={cn('input input-md w-full font-mono text-sm', controlClass)} placeholder="留空时按格式自动选择" value={newUserAgent} onChange={(event) => setNewUserAgent(event.target.value)} /></fieldset>
            <div className="flex justify-end lg:col-span-2"><button className="btn btn-primary btn-md lg:min-w-28" disabled={!newName.trim() || !newUrl.trim() || busy} onClick={() => void addSubscription()}>{action === 'create' && <span className="loading loading-spinner loading-sm" />}添加订阅</button></div>
          </div>
        </section>

        <section className={cn(surfaceClass, "overflow-hidden")}>
          <div className="flex items-center justify-between border-b border-base-200 px-5 py-4 lg:px-6"><div><h3 className="text-lg font-bold">订阅源列表</h3><p className="mt-0.5 text-xs text-base-content/50">刷新采用增量合并，上游连续 2 次未出现的节点才会被移除</p></div><span className="badge badge-ghost">{subscriptions.length} 项</span></div>
          {subscriptions.length ? <div className="space-y-3 p-4 lg:p-6">{subscriptions.map((subscription) => (
            <article key={subscription.id} className={cn(`rounded-xl border bg-base-200/30 p-4`, subscription.enabled ? 'border-base-300' : 'border-base-200 opacity-70')}>
              {editing?.id === subscription.id ? (
                <div className="grid gap-3 sm:grid-cols-2">
                  <input className="input input-sm w-full" value={editName} onChange={(event) => setEditName(event.target.value)} />
                  <input className="input input-sm w-full font-mono text-xs" value={editUrl} onChange={(event) => setEditUrl(event.target.value)} />
                  <select className="select select-sm w-full" value={editFormat} onChange={(event) => setEditFormat(event.target.value as SubscriptionFormat)}><option value="auto">自动识别</option><option value="clash">Clash / Mihomo YAML</option><option value="base64">Base64 URI 列表</option><option value="sing-box">Sing-box JSON</option></select>
                  <input className="input input-sm w-full font-mono text-xs" placeholder="自定义 User-Agent（可选）" value={editUserAgent} onChange={(event) => setEditUserAgent(event.target.value)} />
                  <div className="flex justify-end gap-2 sm:col-span-2"><button className="btn btn-primary btn-sm" disabled={!editName.trim() || !editUrl.trim() || busy} onClick={() => void saveSubscription()}>保存</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => setEditing(null)}>取消</button></div>
                </div>
              ) : (
                <div className="flex flex-col gap-4 lg:flex-row lg:items-center">
                  <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><strong>{subscription.name}</strong><span className={cn("badge badge-sm", subscription.enabled ? 'badge-success' : 'badge-ghost')}>{subscription.enabled ? '已启用' : '已禁用'}</span>{isRefreshingRow(subscription) && <span className="badge badge-info badge-sm gap-1"><span className="loading loading-spinner loading-xs" />同步中</span>}<span className="badge badge-outline badge-sm">{subscription.node_count} 节点</span><span className="badge badge-ghost badge-sm">{subscriptionFormatLabel(subscription.format)}</span>{subscription.user_agent && <span className="badge badge-ghost badge-sm font-mono">UA: {subscription.user_agent}</span>}</div><code className="mt-1 block break-all text-xs text-base-content/55">{subscription.url}</code><div className="mt-2 flex flex-wrap gap-x-4 text-xs text-base-content/55"><span>最近成功：{subscription.last_success && !subscription.last_success.startsWith('0001-') ? new Date(subscription.last_success).toLocaleString() : '尚未成功'}</span>{subscription.last_error && <span className="break-all text-error">错误：{subscription.last_error}</span>}</div></div>
                  <div className="flex flex-wrap gap-2 lg:justify-end"><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => startEditing(subscription)}>编辑</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => void runAction(`toggle-${subscription.id}`, () => toggleSubscription(subscription.id, !subscription.enabled), subscription.enabled ? '订阅已禁用' : '订阅已启用')}>{action === `toggle-${subscription.id}` && <span className="loading loading-spinner loading-xs" />}{subscription.enabled ? '禁用' : '启用'}</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => void runAction(`refresh-${subscription.id}`, () => refreshOneSubscription(subscription.id), `${subscription.name} 刷新完成`)}>{action === `refresh-${subscription.id}` && <span className="loading loading-spinner loading-xs" />}刷新</button><button className="btn btn-ghost btn-sm text-primary" disabled={busy || (subscription.enabled && enabledCount === 1)} onClick={() => void runAction(`activate-${subscription.id}`, () => activateSubscription(subscription.id), `已独占启用 ${subscription.name}`)}>独占启用</button><button className="btn btn-ghost btn-sm text-error" disabled={busy} onClick={() => setDeleteTarget(subscription)}>删除</button></div>
                </div>
              )}
              {deleteTarget?.id === subscription.id && <div className="alert alert-warning mt-3 flex-col items-start sm:flex-row sm:items-center"><span>确认删除订阅“{subscription.name}”？其独占节点将保留为手动节点并继续运行。</span><div className="flex gap-2 sm:ml-auto"><button className="btn btn-error btn-sm" disabled={busy} onClick={() => void runAction(`delete-${subscription.id}`, () => deleteSubscription(subscription.id), '订阅已删除，节点已保留').then((deleted) => deleted && setDeleteTarget(null))}>确认删除</button><button className="btn btn-ghost btn-sm" disabled={busy} onClick={() => setDeleteTarget(null)}>取消</button></div></div>}
            </article>
          ))}</div> : <div className="m-4 rounded-xl border border-dashed border-base-300 bg-base-200/20 px-4 py-12 text-center lg:m-6"><p className="font-medium text-base-content/60">暂无订阅链接</p><p className="mt-1 text-sm text-base-content/40">在上方添加节点订阅地址</p></div>}
        </section>

        {/* ===== 订阅自动刷新（由「系统设置」迁移至此） ===== */}
        <section className={cn(surfaceClass, "p-5 lg:p-6")}>
          <div className="mb-5 flex flex-wrap items-center justify-between gap-3 border-b border-base-200 pb-4">
            <div className="flex items-center gap-3">
              <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border border-warning/20 bg-warning/10 text-warning"><Clock className="h-5 w-5" /></span>
              <div>
                <h3 className="text-lg font-bold">订阅自动刷新</h3>
                <p className="mt-0.5 text-xs text-base-content/50">定时同步订阅源，并在切换新节点前做健康检查；原「系统设置」中的同名配置已迁移至此</p>
              </div>
            </div>
            <span className={cn("badge", status?.enabled ? 'badge-success' : 'badge-ghost')}>{status?.enabled ? '自动刷新已开启' : '自动刷新未开启'}</span>
          </div>

          <label className="flex cursor-pointer items-center justify-between gap-4 rounded-xl border border-base-200 bg-base-200/30 p-4 transition-colors hover:border-base-300">
            <div>
              <span className="block font-semibold text-base-content/90">启用定时刷新</span>
              <p className="m-0 text-xs text-base-content/50">关闭后仅能手动触发「全量刷新」或单个订阅刷新</p>
            </div>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-md"
              checked={refreshSettings.sub_refresh_enabled}
              disabled={refreshLoading || savingRefresh}
              onChange={(event) => updateRefreshField('sub_refresh_enabled', event.target.checked)}
              aria-label="启用订阅定时刷新"
            />
          </label>

          {refreshSettings.sub_refresh_enabled && (
            <div className="mt-4 space-y-4 animate-in fade-in slide-in-from-top-2">
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">刷新间隔</legend><input aria-label="刷新间隔" className={cn('input input-md w-full', controlClass)} placeholder="1h" value={refreshSettings.sub_refresh_interval} onChange={(event) => updateRefreshField('sub_refresh_interval', event.target.value)} /></fieldset>
                <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">获取超时</legend><input aria-label="获取超时" className={cn('input input-md w-full', controlClass)} placeholder="30s" value={refreshSettings.sub_refresh_timeout} onChange={(event) => updateRefreshField('sub_refresh_timeout', event.target.value)} /></fieldset>
              </div>
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">健康检查超时</legend><input aria-label="健康检查超时" className={cn('input input-md w-full', controlClass)} placeholder="1m" value={refreshSettings.sub_refresh_health_check_timeout} onChange={(event) => updateRefreshField('sub_refresh_health_check_timeout', event.target.value)} /></fieldset>
                <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">排空超时</legend><input aria-label="排空超时" className={cn('input input-md w-full', controlClass)} placeholder="30s" value={refreshSettings.sub_refresh_drain_timeout} onChange={(event) => updateRefreshField('sub_refresh_drain_timeout', event.target.value)} /></fieldset>
              </div>
              <fieldset className="fieldset">
                <legend className="fieldset-legend font-semibold text-base-content/80">最少可用节点数</legend>
                <input aria-label="最少可用节点数" type="number" min={0} className={cn('input input-md w-full', controlClass)} value={refreshSettings.sub_refresh_min_available_nodes} onChange={(event) => updateRefreshField('sub_refresh_min_available_nodes', Math.max(0, parseInt(event.target.value) || 0))} />
                <p className="label mt-1 text-base-content/50">低于此值时不切换新节点（Go duration 格式：1h、30s、1m30s）</p>
              </fieldset>
            </div>
          )}

          <div className="mt-5 flex flex-wrap items-center justify-end gap-2">
            {refreshDirty && <span className="mr-auto text-xs font-medium text-warning">存在未保存的修改</span>}
            <button className="btn btn-ghost btn-sm" disabled={!refreshDirty || savingRefresh} onClick={() => setRefreshDraft(null)}>放弃修改</button>
            <button className={cn('btn btn-sm', refreshDirty ? 'btn-primary' : 'btn-ghost border border-base-300')} disabled={!refreshDirty || savingRefresh} onClick={() => void saveRefreshSettings()}>
              {savingRefresh ? <span className="loading loading-spinner loading-xs" /> : <Save className="h-4 w-4" />}
              保存自动刷新设置
            </button>
          </div>
        </section>

        {/* ===== 免登录 API ===== */}
        <section className={cn(surfaceClass, "p-5 lg:p-6")}>
          <div className="mb-5 flex flex-wrap items-center justify-between gap-3 border-b border-base-200 pb-4">
            <div className="flex items-center gap-3">
              <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl border border-primary/20 bg-primary/10 text-primary"><KeyRound className="h-5 w-5" /></span>
              <div>
                <h3 className="text-lg font-bold">免登录 API</h3>
                <p className="mt-0.5 text-xs text-base-content/50">生成 API Key 后，外部脚本无需登录即可添加订阅与导入节点</p>
              </div>
            </div>
            <span className="badge badge-ghost">{apiKeys.length} 个凭证</span>
          </div>

          <div className="space-y-2 rounded-xl border border-base-200 bg-base-200/30 p-4 text-sm">
            <p className="font-semibold text-base-content/90">鉴权方式</p>
            <p className="text-base-content/60">
              请求头 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">{'X-API-Key: <API_KEY>'}</code>；
              亦兼容 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">{'Authorization: Bearer <API_KEY>'}</code> 与查询参数 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">?api_key=</code>。
              凭证为 64 位随机十六进制串，仅存放于本地数据库，可随时在下方撤销。
            </p>
            <p className="font-semibold text-base-content/90 pt-1">凭证权限范围</p>
            <ul className="list-disc space-y-1 pl-5 text-base-content/60">
              <li>仅可调用 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">POST /api/public/subscriptions</code>（添加订阅）与 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">POST /api/public/import</code>（导入节点）两个写接口。</li>
              <li>不授予读取、修改、删除、刷新订阅的权限，也不授予节点、分组、标签、设置、调试等任何其他接口的访问权。</li>
              <li>不能代替登录会话：WebUI 的 <code className="rounded bg-base-300/60 px-1.5 py-0.5 font-mono text-xs">/api/*</code> 管理接口仍要求登录。</li>
            </ul>
          </div>

          <div className="mt-4 flex flex-col gap-3 sm:flex-row sm:items-end">
            <fieldset className="fieldset flex-1">
              <legend className="fieldset-legend font-semibold text-base-content/80">凭证名称（可选）</legend>
              <input aria-label="凭证名称（可选）" className={cn('input input-md w-full', controlClass)} placeholder="例如：CI 定时导入" value={newKeyName} onChange={(event) => setNewKeyName(event.target.value)} onKeyDown={(event) => event.key === 'Enter' && void generateAPIKey()} />
            </fieldset>
            <button className="btn btn-primary btn-md sm:min-w-32" disabled={keyAction === 'create'} onClick={() => void generateAPIKey()}>
              {keyAction === 'create' && <span className="loading loading-spinner loading-sm" />}
              生成 API Key
            </button>
          </div>

          {freshToken && (
            <div role="alert" className="alert alert-success alert-soft mt-4 flex-col items-start gap-2 sm:flex-row sm:items-center">
              <div className="min-w-0 flex-1">
                <p className="text-sm font-semibold">新凭证已生成，请立即保存</p>
                <code className="mt-1 block break-all font-mono text-xs">{freshToken}</code>
              </div>
              <div className="flex gap-2 sm:ml-auto">
                <button className="btn btn-success btn-sm gap-1" onClick={() => void copyText(freshToken, 'API Key')}><Copy className="h-3.5 w-3.5" />复制</button>
                <button className="btn btn-ghost btn-sm" onClick={() => setFreshToken('')}>关闭</button>
              </div>
            </div>
          )}

          {apiKeys.length ? (
            <div className="mt-4 space-y-2">
              {apiKeys.map((key) => (
                <div key={key.id} className="flex flex-col gap-3 rounded-xl border border-base-300 bg-base-200/30 p-3 sm:flex-row sm:items-center sm:gap-4">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <strong className="text-sm">{key.name || '未命名凭证'}</strong>
                      <code className="badge badge-ghost badge-sm font-mono">{maskToken(key.token)}</code>
                    </div>
                    <div className="mt-1 flex flex-wrap gap-x-4 text-xs text-base-content/55">
                      <span>创建于 {formatKeyTime(key.created_at)}</span>
                      <span>最近使用 {key.last_used_at ? formatKeyTime(key.last_used_at) : '尚未使用'}</span>
                    </div>
                  </div>
                  <div className="flex flex-wrap gap-2 sm:justify-end">
                    <button className="btn btn-ghost btn-sm gap-1" onClick={() => void copyText(key.token, 'API Key')}><Copy className="h-3.5 w-3.5" />复制</button>
                    <button className="btn btn-ghost btn-sm gap-1 text-error" disabled={keyAction === `delete-${key.id}`} onClick={() => setRevokeTarget(key)}>
                      {keyAction === `delete-${key.id}` ? <span className="loading loading-spinner loading-xs" /> : <Trash2 className="h-3.5 w-3.5" />}
                      撤销
                    </button>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="mt-4 rounded-xl border border-dashed border-base-300 bg-base-200/20 px-4 py-8 text-center">
              <p className="font-medium text-base-content/60">尚未生成任何 API Key</p>
              <p className="mt-1 text-sm text-base-content/40">生成后即可用于免登录添加订阅与导入节点</p>
            </div>
          )}

          {revokeTarget && (
            <div className="alert alert-warning mt-3 flex-col items-start sm:flex-row sm:items-center">
              <span>确认撤销凭证「{revokeTarget.name || '未命名凭证'}」？使用该 Key 的脚本将立即失效。</span>
              <div className="flex gap-2 sm:ml-auto">
                <button className="btn btn-error btn-sm" disabled={keyAction === `delete-${revokeTarget.id}`} onClick={() => void revokeAPIKey(revokeTarget)}>确认撤销</button>
                <button className="btn btn-ghost btn-sm" disabled={keyAction === `delete-${revokeTarget.id}`} onClick={() => setRevokeTarget(null)}>取消</button>
              </div>
            </div>
          )}

          <div className="mt-5 space-y-3">
            <p className="text-sm font-semibold text-base-content/80">调用示例</p>
            <div>
              <p className="mb-1 text-xs text-base-content/55">添加订阅（请求体字段与 <code className="font-mono">/api/subscriptions</code> 完全一致）</p>
              <pre className="overflow-x-auto rounded-xl border border-base-200 bg-base-200/40 p-3 font-mono text-xs leading-relaxed text-base-content/80">{curlSubscriptionExample}</pre>
            </div>
            <div>
              <p className="mb-1 text-xs text-base-content/55">导入节点（请求体字段与 <code className="font-mono">/api/import</code> 完全一致，支持 URI / Clash YAML / Base64 / sing-box JSON）</p>
              <pre className="overflow-x-auto rounded-xl border border-base-200 bg-base-200/40 p-3 font-mono text-xs leading-relaxed text-base-content/80">{curlImportExample}</pre>
            </div>
          </div>
        </section>
      </PageContent>
    </PageLayout>
  )
}

function formatKeyTime(value: string): string {
  if (!value || value.startsWith('0001-')) return '—'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString()
}

function refreshSummary(value: unknown): string | null {
  if (!value || typeof value !== 'object' || !('parsed' in value)) return null
  const result = value as Record<string, unknown>
  const count = (key: string) => typeof result[key] === 'number' ? result[key] : 0
  return `解析 ${count('parsed')} · 新增 ${count('created')} · 复用 ${count('updated')} · 重复跳过 ${count('duplicates_skipped')} · 无效 ${count('invalid')}`
}

function subscriptionFormatLabel(format: SubscriptionFormat | undefined): string {
  switch (format) {
    case 'clash': return 'Clash/Mihomo'
    case 'base64': return 'Base64'
    case 'sing-box': return 'Sing-box'
    default: return '自动识别'
  }
}
