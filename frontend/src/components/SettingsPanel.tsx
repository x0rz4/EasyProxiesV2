import { useEffect, useState } from 'react'
import type { SettingsData } from '../types'
import {
  fetchSettings,
  triggerReload,
  updateSettings,
  fetchGeoipStatus,
  downloadGeoipDatabase,
  updateGeoipDatabase,
} from '../api/client'
import { PageContent, PageHeader, PageLayout } from './ui/PageLayout'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  Settings, RefreshCw, Save, Check, AlertTriangle, Globe, Wifi, Network, Layers, Monitor, Map as MapIcon, RefreshCcw
} from 'lucide-react'

const settingResultLabels: Record<string, string> = {
  runtime_config: '运行时配置',
  management_server: '管理服务',
  management_password: 'WebUI 密码',
  management_probe_target: '探测目标',
  management_startup_availability_policy: '启动准入策略',
  management_health_check_interval: '健康检查间隔',
  external_ip: '外部 IP',
  sub_refresh_enabled: '订阅自动刷新',
}

const formatSettingResults = (items: string[]) =>
  items.map(item => settingResultLabels[item] || item).join('、')

function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 2)} ${units[i]}`
}

function formatTimestamp(ts: string): string {
  if (!ts) return ''
  try {
    return new Date(ts).toLocaleString()
  } catch {
    return ts
  }
}

const defaultSettings: SettingsData = {
  log_level: 'info',
  external_ip: '',
  skip_cert_verify: false,

  listener_enabled: false,
  listener_address: '0.0.0.0',
  listener_port: 2323,
  listener_protocol: 'http',
  listener_username: '',
  listener_password: '',

  multi_port_enabled: false,
  multi_port_address: '0.0.0.0',
  multi_port_base_port: 24000,
  multi_port_protocol: 'http',
  multi_port_username: '',
  multi_port_password: '',

  pool_mode: 'sequential',
  pool_failure_threshold: 3,
  pool_blacklist_duration: '24h0m0s',

  management_enabled: true,
  management_listen: '0.0.0.0:9090',
  management_probe_target: '',
  management_startup_availability_policy: 'optimistic',
  management_password: '',
  management_health_check_interval: '2h0m0s',

  sub_refresh_enabled: false,
  sub_refresh_interval: '1h0m0s',
  sub_refresh_timeout: '30s',
  sub_refresh_health_check_timeout: '1m0s',
  sub_refresh_drain_timeout: '30s',
  sub_refresh_min_available_nodes: 1,

  geoip_enabled: false,
  geoip_database_path: './GeoLite2-Country.mmdb',
  geoip_auto_update_enabled: true,
  geoip_auto_update_interval: '24h0m0s',

  subscriptions: [],
}

export default function SettingsPanel() {
  const queryClient = useQueryClient()
  const cachedSettings = queryClient.getQueryData<SettingsData>(['settings'])
  const initialSettings = { ...defaultSettings, ...cachedSettings }
  const [settings, setSettings] = useState<SettingsData>(() => initialSettings)
  const [savedSettings, setSavedSettings] = useState<SettingsData>(() => initialSettings)
  const [saving, setSaving] = useState(false)
  const [reloading, setReloading] = useState(false)
  const [reloadWarning, setReloadWarning] = useState('')
  const [needReload, setNeedReload] = useState(false)
  const [needRestart, setNeedRestart] = useState(false)
  const [applied, setApplied] = useState<string[]>([])
  const [pending, setPending] = useState<string[]>([])
  const [isDirty, setIsDirty] = useState(false)
  const { isLoading: loading } = useQuery({
    queryKey: ['settings'],
    queryFn: async () => {
      const fetched = await fetchSettings()
      const merged = { ...defaultSettings, ...fetched }
      setSettings(prev => isDirty ? prev : merged)
      setSavedSettings(merged)
      return fetched
    },
  })

  // GeoIP database management state
  const { data: geoipStatus, refetch: refetchGeoip } = useQuery({ queryKey: ['geoipStatus'], queryFn: fetchGeoipStatus })
  const [geoipLoading, setGeoipLoading] = useState(false)

  const handleGeoipDownload = async () => {
    setGeoipLoading(true)
    try {
      const res = await downloadGeoipDatabase()
      if (res.reload_error) toast.warning(`IP 库下载完成，但区域回填失败：${res.reload_error}`)
      else toast.success(res.reloaded ? 'IP 库下载完成，节点区域已回填' : (res.message || 'IP 库下载完成'))
      void refetchGeoip()
      void queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '下载 IP 库失败')
    } finally {
      setGeoipLoading(false)
    }
  }

  const handleGeoipUpdate = async () => {
    setGeoipLoading(true)
    try {
      const res = await updateGeoipDatabase()
      if (res.reload_error) toast.warning(`IP 库更新完成，但区域回填失败：${res.reload_error}`)
      else toast.success(res.reloaded ? 'IP 库更新完成，节点区域已回填' : (res.message || 'IP 库更新完成'))
      if (res.reload_hint) {
        setNeedReload(true)
        if (!pending.includes('runtime_config')) {
          setPending([...pending, 'runtime_config'])
        }
      }
      void refetchGeoip()
      void queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '更新 IP 库失败')
    } finally {
      setGeoipLoading(false)
    }
  }

  const handleSave = async () => {
    setSaving(true)
    setReloadWarning('')
    try {
      // Subscription CRUD is persisted separately; preserve the last value read
      // from /api/settings so this form cannot overwrite it with UI state.
      const payload = { ...settings, subscriptions: savedSettings.subscriptions }
      const res = await updateSettings(payload)
      toast.success(res.reloaded ? '设置已保存并自动重载' : (res.message || '设置已保存'))
      setReloadWarning(res.reload_error ? `设置已保存，但自动重载失败：${res.reload_error}` : '')
      setSavedSettings(payload)
      setIsDirty(false)
      setNeedReload(Boolean(res.need_reload))
      setNeedRestart(Boolean(res.need_restart))
      setApplied(res.applied)
      setPending(res.pending)
      queryClient.invalidateQueries({ queryKey: ['settings'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const handleReload = async () => {
    setReloading(true)
    setReloadWarning('')
    try {
      const res = await triggerReload()
      toast.success(res.message || '重载成功')
      setNeedReload(false)
      setPending(items => items.filter(item => item !== 'runtime_config'))
      queryClient.invalidateQueries({ queryKey: ['configNodes'] })
      queryClient.invalidateQueries({ queryKey: ['nodes'] })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '重载失败')
    } finally {
      setReloading(false)
    }
  }

  const updateField = <K extends keyof SettingsData>(key: K, value: SettingsData[K]) => {
    setSettings(s => {
      const updated = { ...s, [key]: value }
      setIsDirty(JSON.stringify(updated) !== JSON.stringify(savedSettings))
      return updated
    })
  }

  // Warn before leaving with unsaved changes
  useEffect(() => {
    const handleBeforeUnload = (e: BeforeUnloadEvent) => {
      if (isDirty) {
        e.preventDefault()
        e.returnValue = ''
      }
    }
    window.addEventListener('beforeunload', handleBeforeUnload)
    return () => window.removeEventListener('beforeunload', handleBeforeUnload)
  }, [isDirty])

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <span className="loading loading-spinner loading-lg text-primary"></span>
      </div>
    )
  }

  return (
    <PageLayout>
      <PageHeader
        title="系统设置"
        description="管理系统所有配置项，修改后需保存生效"
        icon={<Settings className="h-5 w-5" />}
        actions={<>
            {needReload && (
              <button
                className="btn btn-warning btn-sm gap-2 shadow-sm animate-pulse lg:btn-md"
                onClick={handleReload}
                disabled={reloading}
                title="重载配置"
                aria-label="重载配置"
              >
                {reloading ? <span className="loading loading-spinner loading-sm"></span> : <RefreshCw className="h-4 w-4" />}
                <span className="hidden sm:inline">重载配置</span>
              </button>
            )}
            <button
              className={`btn btn-sm lg:btn-md gap-2 shadow-sm ${isDirty ? 'btn-primary' : 'btn-ghost border border-base-300'}`}
              onClick={handleSave}
              disabled={saving || !isDirty}
              title={isDirty ? '保存设置' : '设置已保存'}
              aria-label={isDirty ? '保存设置' : '设置已保存'}
            >
              {saving ? <span className="loading loading-spinner loading-sm"></span> : isDirty ? (
                <>
                  <Save className="h-4 w-4" />
                  <span className="hidden sm:inline">保存设置</span>
                </>
              ) : <><Check className="h-4 w-4" /><span className="hidden sm:inline">已保存</span></>}
            </button>
          </>}
      />

      <PageContent>
        {/* Alerts */}
      {reloadWarning && (
        <div role="alert" className="alert alert-warning alert-soft text-sm">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>{reloadWarning}</span>
        </div>
      )}
      {needRestart && (
        <div role="alert" className="alert alert-warning alert-soft text-sm">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>管理服务启停或监听地址需重启进程。</span>
        </div>
      )}
      {needReload && (
        <div role="alert" className="alert alert-warning alert-soft text-sm">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <div>
            <span>配置已保存，部分运行时配置尚未生效，请点击「重载配置」。</span>
          </div>
        </div>
      )}
      {(applied.length > 0 || pending.length > 0) && (
        <div className="text-xs text-base-content/60 px-1 space-y-1">
          {applied.length > 0 && <p>已应用：{formatSettingResults(applied)}</p>}
          {pending.length > 0 && <p>待生效：{formatSettingResults(pending)}</p>}
        </div>
      )}

      {/* Settings Cards Grid */}
      <div className="grid items-start gap-5 lg:grid-cols-2 2xl:grid-cols-3">

        {/* ===== 全局设置 ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-info/10 flex items-center justify-center text-info shrink-0">
              <Globe className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">全局设置</h3>
              <p className="text-xs text-base-content/50 font-medium">系统基础运行参数</p>
            </div>
          </div>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">日志级别</legend>
            <select
              className="select select-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.log_level}
              onChange={(e) => updateField('log_level', e.target.value)}
            >
              <option value="debug">debug</option>
              <option value="info">info</option>
              <option value="warn">warn</option>
              <option value="error">error</option>
            </select>
          </fieldset>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">外部 IP 地址</legend>
            <input
              type="text"
              placeholder="例如: 1.2.3.4"
              className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.external_ip}
              onChange={(e) => updateField('external_ip', e.target.value)}
            />
            <p className="label text-base-content/50 mt-1">用于导出时替换 0.0.0.0</p>
          </fieldset>

          <label className="flex items-center justify-between cursor-pointer gap-4 bg-base-200/30 p-4 rounded-xl border border-base-200 hover:border-base-300 transition-colors">
            <div>
              <span className="font-semibold text-base-content/90 block mb-0.5">跳过 SSL 证书验证</span>
              <p className="text-xs text-base-content/50 m-0">全局跳过上游代理的 SSL 证书验证</p>
            </div>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-md"
              checked={settings.skip_cert_verify}
              onChange={(e) => updateField('skip_cert_verify', e.target.checked)}
            />
          </label>
        </div>

        {/* ===== 监听配置 (Pool 入口) ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-success/10 flex items-center justify-center text-success shrink-0">
              <Wifi className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">监听配置 (Pool)</h3>
              <p className="text-xs text-base-content/50 font-medium">代理池入口网络参数</p>
            </div>
            <input
              type="checkbox"
              className="toggle toggle-success toggle-md ml-auto"
              checked={settings.listener_enabled}
              onChange={(e) => updateField('listener_enabled', e.target.checked)}
              aria-label="启用 Pool 入口"
            />
          </div>

          {!settings.listener_enabled && <p className="text-xs text-base-content/50">已关闭，不创建 Pool 监听端口。</p>}

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">监听地址</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                value={settings.listener_address}
                onChange={(e) => updateField('listener_address', e.target.value)}
                disabled={!settings.listener_enabled}
              />
            </fieldset>
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">监听端口</legend>
              <input
                type="number"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                value={settings.listener_port}
                onChange={(e) => updateField('listener_port', parseInt(e.target.value) || 0)}
                min={1}
                max={65535}
                disabled={!settings.listener_enabled}
              />
            </fieldset>
          </div>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">监听协议</legend>
            <select
              className="select select-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.listener_protocol}
              onChange={(e) => updateField('listener_protocol', e.target.value)}
              disabled={!settings.listener_enabled}
            >
              <option value="http">http</option>
              <option value="socks5">socks5</option>
              <option value="mixed">mixed (HTTP + SOCKS5)</option>
            </select>
            <p className="label text-base-content/50 mt-1">mixed 表示同端口同时支持 HTTP 与 SOCKS5</p>
          </fieldset>

          <div className="grid grid-cols-1 gap-4 pt-2 sm:grid-cols-2">
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">代理用户名</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                placeholder="可选，留空表示无验证"
                value={settings.listener_username}
                onChange={(e) => updateField('listener_username', e.target.value)}
                disabled={!settings.listener_enabled}
              />
            </fieldset>
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">代理密码</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                placeholder="可选，留空表示无验证"
                value={settings.listener_password}
                onChange={(e) => updateField('listener_password', e.target.value)}
                disabled={!settings.listener_enabled}
              />
            </fieldset>
          </div>
        </div>

        {/* ===== 多端口配置 ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-secondary/10 flex items-center justify-center text-secondary shrink-0">
              <Network className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">多端口配置</h3>
              <p className="text-xs text-base-content/50 font-medium">每个节点独立入口网络参数</p>
            </div>
            <input
              type="checkbox"
              className="toggle toggle-secondary toggle-md ml-auto"
              checked={settings.multi_port_enabled}
              onChange={(e) => updateField('multi_port_enabled', e.target.checked)}
              aria-label="启用每节点多端口入口"
            />
          </div>

          {!settings.multi_port_enabled && <p className="text-xs text-base-content/50">已关闭，不为节点创建独立监听端口。</p>}

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">监听地址</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                value={settings.multi_port_address}
                onChange={(e) => updateField('multi_port_address', e.target.value)}
                disabled={!settings.multi_port_enabled}
              />
            </fieldset>
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">起始端口</legend>
              <input
                type="number"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                value={settings.multi_port_base_port}
                onChange={(e) => updateField('multi_port_base_port', parseInt(e.target.value) || 0)}
                min={1}
                max={65535}
                disabled={!settings.multi_port_enabled}
              />
            </fieldset>
          </div>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">监听协议</legend>
            <select
              className="select select-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.multi_port_protocol}
              onChange={(e) => updateField('multi_port_protocol', e.target.value)}
              disabled={!settings.multi_port_enabled}
            >
              <option value="http">http</option>
              <option value="socks5">socks5</option>
              <option value="mixed">mixed (HTTP + SOCKS5)</option>
            </select>
            <p className="label text-base-content/50 mt-1">应用于 multi-port / hybrid 的每个节点入口</p>
          </fieldset>

          <div className="grid grid-cols-1 gap-4 pt-2 sm:grid-cols-2">
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">默认用户名</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                placeholder="可选"
                value={settings.multi_port_username}
                onChange={(e) => updateField('multi_port_username', e.target.value)}
                disabled={!settings.multi_port_enabled}
              />
            </fieldset>
            <fieldset className="fieldset">
              <legend className="fieldset-legend font-semibold text-base-content/80">默认密码</legend>
              <input
                type="text"
                className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                placeholder="可选"
                value={settings.multi_port_password}
                onChange={(e) => updateField('multi_port_password', e.target.value)}
                disabled={!settings.multi_port_enabled}
              />
            </fieldset>
          </div>
        </div>

        {/* ===== 代理池配置 ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-accent/10 flex items-center justify-center text-accent shrink-0">
              <Layers className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">代理池调度</h3>
              <p className="text-xs text-base-content/50 font-medium">节点选择与高可用策略</p>
            </div>
          </div>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">调度模式</legend>
            <select
              className="select select-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.pool_mode}
              onChange={(e) => updateField('pool_mode', e.target.value)}
            >
              <option value="sequential">sequential - 顺序轮询</option>
              <option value="random">random - 随机选择</option>
              <option value="balance">balance - 最小连接数负载均衡</option>
            </select>
          </fieldset>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">失败阈值</legend>
            <input
              type="number"
              className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              value={settings.pool_failure_threshold}
              onChange={(e) => updateField('pool_failure_threshold', parseInt(e.target.value) || 1)}
              min={1}
            />
            <p className="label text-base-content/50 mt-1">连续失败多少次后加入黑名单</p>
          </fieldset>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">黑名单持续时间</legend>
            <input
              type="text"
              className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              placeholder="例如: 24h, 1h30m"
              value={settings.pool_blacklist_duration}
              onChange={(e) => updateField('pool_blacklist_duration', e.target.value)}
            />
            <p className="label text-base-content/50 mt-1">Go duration 格式: 24h, 1h30m, 30m 等</p>
          </fieldset>
        </div>

        {/* ===== 管理面板 ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-primary/10 flex items-center justify-center text-primary shrink-0">
              <Monitor className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">管理面板</h3>
              <p className="text-xs text-base-content/50 font-medium">Web 界面与访问控制；探测参数已迁至运维管理</p>
            </div>
          </div>

          <label className="flex items-center justify-between cursor-pointer gap-4 bg-base-200/30 p-4 rounded-xl border border-base-200 hover:border-base-300 transition-colors">
            <span className="font-semibold text-base-content/90">启用管理面板</span>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-md"
              checked={settings.management_enabled}
              onChange={(e) => updateField('management_enabled', e.target.checked)}
            />
          </label>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">监听地址</legend>
            <input
              type="text"
              className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              placeholder="0.0.0.0:9090"
              value={settings.management_listen}
              onChange={(e) => updateField('management_listen', e.target.value)}
            />
          </fieldset>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">WebUI 密码</legend>
            <input
              type="text"
              className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
              placeholder="为空则无需密码"
              value={settings.management_password}
              onChange={(e) => updateField('management_password', e.target.value)}
            />
            <p className="label text-base-content/50 mt-1">为空则不需要登录密码</p>
          </fieldset>

          <fieldset className="fieldset">
            <legend className="fieldset-legend font-semibold text-base-content/80">启动准入策略</legend>
            <select className="select select-md w-full bg-base-200/50" value={settings.management_startup_availability_policy} onChange={(e) => updateField('management_startup_availability_policy', e.target.value as 'optimistic' | 'strict')}>
              <option value="optimistic">optimistic · 待复检节点先运行</option>
              <option value="strict">strict · 本轮成功后运行</option>
            </select>
          </fieldset>
        </div>

        {/* ===== GeoIP ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-info/10 flex items-center justify-center text-info shrink-0">
              <MapIcon className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">GeoIP 地域分区</h3>
              <p className="text-xs text-base-content/50 font-medium">节点地域解析与自动更新</p>
            </div>
          </div>

          <label className="flex items-center justify-between cursor-pointer gap-4 bg-base-200/30 p-4 rounded-xl border border-base-200 hover:border-base-300 transition-colors">
            <div>
              <span className="font-semibold text-base-content/90 block mb-0.5">启用 GeoIP</span>
              <p className="text-xs text-base-content/50 m-0">按地域自动分组节点</p>
            </div>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-md"
              checked={settings.geoip_enabled}
              onChange={(e) => updateField('geoip_enabled', e.target.checked)}
            />
          </label>

          {/* IP 库是节点落地地区识别的基础，与区域路由开关无关。 */}
          <div className="bg-base-200/30 p-4 rounded-xl border border-base-200 space-y-3">
            <div className="flex items-center justify-between gap-3">
              <span className="font-semibold text-base-content/80 text-sm">IP 库状态</span>
              {geoipStatus?.database?.exists ? (
                <span className="badge badge-success badge-sm gap-1">已下载</span>
              ) : (
                <span className="badge badge-ghost badge-sm gap-1">未下载</span>
              )}
            </div>

            {geoipStatus?.database && (
              <div className="text-xs text-base-content/60 space-y-1 font-mono">
                {geoipStatus.database.exists ? (
                  <>
                    <div>大小：{formatBytes(geoipStatus.database.size_bytes)}</div>
                    {geoipStatus.database.modified_at && (
                      <div>更新时间：{formatTimestamp(geoipStatus.database.modified_at)}</div>
                    )}
                  </>
                ) : (
                  <div>数据库尚未下载，节点区域识别已暂停</div>
                )}
                <div className="truncate" title={geoipStatus.database.source_url || geoipStatus.database.download_url}>
                  当前来源：{geoipStatus.database.source_url || geoipStatus.database.download_url}
                </div>
                {geoipStatus.database.fallback_url && (
                  <div className="truncate" title={geoipStatus.database.fallback_url}>备用来源：{geoipStatus.database.fallback_url}</div>
                )}
              </div>
            )}

            <div className="flex flex-wrap gap-2 pt-1">
              <button type="button" className="btn btn-sm btn-outline" disabled={geoipLoading} onClick={handleGeoipDownload}>
                {geoipLoading ? <span className="loading loading-spinner loading-xs"></span> : null}
                下载 IP 库
              </button>
              <button type="button" className="btn btn-sm btn-primary" disabled={geoipLoading} onClick={handleGeoipUpdate}>
                {geoipLoading ? <span className="loading loading-spinner loading-xs"></span> : null}
                更新 IP 库
              </button>
              <button type="button" className="btn btn-sm btn-ghost" disabled={geoipLoading} onClick={() => void refetchGeoip()}>
                刷新状态
              </button>
            </div>
            <p className="text-xs text-base-content/40 m-0">下载或更新成功后会自动回填节点区域，无需手动重载。</p>
          </div>

          {settings.geoip_enabled && (
            <div className="space-y-4 pt-2 animate-in fade-in slide-in-from-top-2">
              <fieldset className="fieldset">
                <legend className="fieldset-legend font-semibold text-base-content/80">数据库路径</legend>
                <input
                  type="text"
                  className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                  value={settings.geoip_database_path}
                  onChange={(e) => updateField('geoip_database_path', e.target.value)}
                />
              </fieldset>
              <label className="flex items-center justify-between cursor-pointer gap-4 bg-base-200/30 p-4 rounded-xl border border-base-200 hover:border-base-300 transition-colors">
                <span className="font-semibold text-base-content/90">自动更新数据库</span>
                <input
                  type="checkbox"
                  className="toggle toggle-primary toggle-md"
                  checked={settings.geoip_auto_update_enabled}
                  onChange={(e) => updateField('geoip_auto_update_enabled', e.target.checked)}
                />
              </label>

              {settings.geoip_auto_update_enabled && (
                <fieldset className="fieldset animate-in fade-in slide-in-from-top-2">
                  <legend className="fieldset-legend font-semibold text-base-content/80">更新间隔</legend>
                  <input
                    type="text"
                    className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                    placeholder="24h"
                    value={settings.geoip_auto_update_interval}
                    onChange={(e) => updateField('geoip_auto_update_interval', e.target.value)}
                  />
                </fieldset>
              )}
            </div>
          )}
        </div>

        {/* ===== 订阅刷新 ===== */}
        <div className="panel-card space-y-5 p-5 transition-shadow hover:shadow-md lg:p-6">
          <div className="flex items-center gap-3 mb-2 border-b border-base-200 pb-4">
            <div className="w-10 h-10 rounded-xl bg-warning/10 flex items-center justify-center text-warning shrink-0">
              <RefreshCcw className="h-5 w-5" />
            </div>
            <div>
              <h3 className="font-bold text-lg text-base-content">订阅自动刷新</h3>
              <p className="text-xs text-base-content/50 font-medium">配置订阅定时更新及健康检查</p>
            </div>
          </div>

          <label className="flex items-center justify-between cursor-pointer gap-4 bg-base-200/30 p-4 rounded-xl border border-base-200 hover:border-base-300 transition-colors">
            <span className="font-semibold text-base-content/90">启用定时刷新</span>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-md"
              checked={settings.sub_refresh_enabled}
              onChange={(e) => updateField('sub_refresh_enabled', e.target.checked)}
            />
          </label>

          {settings.sub_refresh_enabled && (
            <div className="space-y-4 pt-2 animate-in fade-in slide-in-from-top-2">
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">刷新间隔</legend>
                  <input
                    type="text"
                    className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                    placeholder="1h"
                    value={settings.sub_refresh_interval}
                    onChange={(e) => updateField('sub_refresh_interval', e.target.value)}
                  />
                </fieldset>
                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">获取超时</legend>
                  <input
                    type="text"
                    className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                    placeholder="30s"
                    value={settings.sub_refresh_timeout}
                    onChange={(e) => updateField('sub_refresh_timeout', e.target.value)}
                  />
                </fieldset>
              </div>

              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">健康检查超时</legend>
                  <input
                    type="text"
                    className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                    placeholder="1m"
                    value={settings.sub_refresh_health_check_timeout}
                    onChange={(e) => updateField('sub_refresh_health_check_timeout', e.target.value)}
                  />
                </fieldset>
                <fieldset className="fieldset">
                  <legend className="fieldset-legend font-semibold text-base-content/80">排空超时</legend>
                  <input
                    type="text"
                    className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                    placeholder="30s"
                    value={settings.sub_refresh_drain_timeout}
                    onChange={(e) => updateField('sub_refresh_drain_timeout', e.target.value)}
                  />
                </fieldset>
              </div>

              <fieldset className="fieldset">
                <legend className="fieldset-legend font-semibold text-base-content/80">最少可用节点数</legend>
                <input
                  type="number"
                  className="input input-md w-full bg-base-200/50 focus:bg-base-100 transition-colors focus:border-primary/50"
                  value={settings.sub_refresh_min_available_nodes}
                  onChange={(e) => updateField('sub_refresh_min_available_nodes', parseInt(e.target.value) || 1)}
                  min={0}
                />
                <p className="label text-base-content/50 mt-1">低于此值时不切换新节点</p>
              </fieldset>
            </div>
          )}
        </div>

      </div>

      </PageContent>

    </PageLayout>
  )
}
