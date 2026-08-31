import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Activity, Gauge, Play, Save, Square, Wrench } from 'lucide-react'
import { toast } from 'sonner'
import { fetchProbeSettings, fetchProbeStatus, probeAllNodes, updateProbeSettings } from '../api/client'
import type { ProbeOperationsSettings, ProbeSSEProgress } from '../types'
import { cn } from '../utils/cn'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'

const EMPTY_SETTINGS: ProbeOperationsSettings = {
  probe_target: '',
  startup_availability_policy: 'optimistic',
  health_check_interval: '2h0m0s',
  probe_concurrency: 0,
  startup_probe_timeout: '5s',
  routine_probe_timeout: '10s',
  probe_dial_timeout: '3s',
  probe_response_timeout: '2s',
  routine_probe_retries: 1,
}

interface ProbeProgressState {
  total: number
  current: number
  success: number
  failed: number
  percent: number
}

export default function OperationsPanel() {
  const queryClient = useQueryClient()
  const abortRef = useRef<AbortController | null>(null)
  const cachedSettings = queryClient.getQueryData<ProbeOperationsSettings>(['probeSettings'])
  const [form, setForm] = useState<ProbeOperationsSettings>(() => cachedSettings ?? EMPTY_SETTINGS)
  const [saving, setSaving] = useState(false)
  const [probing, setProbing] = useState(false)
  const [progress, setProgress] = useState<ProbeProgressState | null>(null)
  const [recentResults, setRecentResults] = useState<ProbeSSEProgress[]>([])

  const settingsQuery = useQuery({
    queryKey: ['probeSettings'],
    queryFn: async () => {
      const settings = await fetchProbeSettings()
      setForm((current) => JSON.stringify(current) === JSON.stringify(cachedSettings ?? EMPTY_SETTINGS) ? settings : current)
      return settings
    },
  })
  const statusQuery = useQuery({
    queryKey: ['probeStatus'],
    queryFn: fetchProbeStatus,
    refetchInterval: (query) => query.state.data?.round.in_flight || query.state.data?.converging || probing ? 1000 : 5000,
  })

  useEffect(() => () => abortRef.current?.abort(), [])

  const status = statusQuery.data
  const roundBusy = probing || Boolean(status?.round.in_flight)
  const dirty = Boolean(settingsQuery.data && JSON.stringify(form) !== JSON.stringify(settingsQuery.data))

  const update = <K extends keyof ProbeOperationsSettings>(key: K, value: ProbeOperationsSettings[K]) => {
    setForm((current) => ({ ...current, [key]: value }))
  }

  const saveSettings = async () => {
    if (!Number.isInteger(form.probe_concurrency) || form.probe_concurrency < 0 || form.probe_concurrency > 512) {
      toast.error('探测并发必须为 0（自动）或 1-512')
      return
    }
    if (!Number.isInteger(form.routine_probe_retries) || form.routine_probe_retries < 0 || form.routine_probe_retries > 2) {
      toast.error('失败重试次数必须为 0-2')
      return
    }
    setSaving(true)
    try {
      const saved = await updateProbeSettings(form)
      setForm(saved)
      queryClient.setQueryData(['probeSettings'], saved)
      await queryClient.invalidateQueries({ queryKey: ['probeStatus'] })
      toast.success('探测参数已保存并热应用', { description: '正在运行的轮次继续使用启动时的策略快照' })
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存探测参数失败')
    } finally {
      setSaving(false)
    }
  }

  const finishProbe = () => {
    abortRef.current = null
    setProbing(false)
    void queryClient.invalidateQueries({ queryKey: ['probeStatus'] })
    void queryClient.invalidateQueries({ queryKey: ['nodes'] })
  }

  const startProbe = () => {
    if (roundBusy) return
    setProbing(true)
    setProgress(null)
    setRecentResults([])
    abortRef.current = probeAllNodes((event) => {
      if (event.type === 'start') {
        setProgress({ total: event.total, current: 0, success: 0, failed: 0, percent: 0 })
      } else if (event.type === 'progress') {
        setRecentResults((current) => [event, ...current].slice(0, 8))
        setProgress((current) => ({
          total: event.total,
          current: event.current,
          success: (current?.success ?? 0) + (event.status === 'success' ? 1 : 0),
          failed: (current?.failed ?? 0) + (event.status === 'error' ? 1 : 0),
          percent: event.progress,
        }))
      } else {
        setProgress({ total: event.total, current: event.total, success: event.success, failed: event.failed, percent: 100 })
        toast.success('全量探测完成', { description: `成功 ${event.success}，失败 ${event.failed}` })
        finishProbe()
      }
    }, (error) => {
      toast.error(error.message)
      finishProbe()
    })
    void queryClient.invalidateQueries({ queryKey: ['probeStatus'] })
  }

  const cancelProbe = () => {
    abortRef.current?.abort()
    abortRef.current = null
    setProbing(false)
    toast.info('已取消手动全量探测')
    void queryClient.invalidateQueries({ queryKey: ['probeStatus'] })
  }

  if (settingsQuery.isLoading || statusQuery.isLoading) {
    return <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>
  }

  if (settingsQuery.error || statusQuery.error) {
    const error = settingsQuery.error || statusQuery.error
    return <div role="alert" className="alert alert-error m-6"><span>{error instanceof Error ? error.message : '运维数据加载失败'}</span></div>
  }

  const liveRound = status?.round
  const visibleProgress = progress ?? (liveRound?.in_flight ? {
    total: liveRound.total,
    current: liveRound.completed,
    success: liveRound.success,
    failed: liveRound.failed,
    percent: liveRound.total ? liveRound.completed / liveRound.total * 100 : 0,
  } : null)

  return (
    <PageLayout>
      <PageHeader
        title="运维管理"
        description="统一管理启动、周期与手动探测策略；参数保存后立即作用于下一轮"
        icon={<Wrench className="h-5 w-5" />}
        actions={roundBusy ? (
          probing ? <button className="btn btn-error btn-sm gap-2 lg:btn-md" onClick={cancelProbe}><Square className="h-4 w-4" />取消探测</button> : <span className="badge badge-warning gap-2 py-3"><span className="loading loading-spinner loading-xs" />探测进行中</span>
        ) : <button className="btn btn-primary btn-sm gap-2 shadow-sm lg:btn-md" onClick={startProbe}><Play className="h-4 w-4" />立即全量探测</button>}
      />

      <PageContent>
        <section aria-label="探测运行概览" className="grid grid-cols-2 gap-4 xl:grid-cols-5">
          <Metric label="节点规模" value={status?.node_count ?? 0} detail="本轮可调度节点" tone="text-primary" />
          <Metric label="首次探测剩余" value={status?.initial_pending ?? 0} detail={status?.waiting_for_manual ? '后台收敛等待手动探测结束' : status?.converging ? `后台收敛中 · 队列等待 ${status.queued} · 已调度 ${status.scheduled}` : status?.initial_pending ? '等待首次探测调度' : '首次探测已收敛'} tone={status?.initial_pending ? 'text-warning' : 'text-success'} />
          <Metric label="有效 Worker" value={status?.effective_concurrency ?? 0} detail={status?.concurrency_mode === 'auto' ? '自动并发模式' : `固定值 ${status?.configured_concurrency ?? 0}`} tone="text-info" />
          <Metric label="启动最坏估算" value={status?.estimated_startup_worst_case || '0s'} detail="启动策略 · 失败等待 500ms 后重试一次" tone="text-success" />
          <Metric label="Routine 最坏估算" value={status?.estimated_routine_worst_case || '0s'} detail="周期与手动总预算" tone="text-warning" />
        </section>

        <section className={cn(surfaceClass, 'overflow-hidden')}>
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-base-200 px-5 py-4 lg:px-6">
            <div><h3 className="flex items-center gap-2 text-lg font-bold"><Gauge className="h-5 w-5 text-primary" />探测调度参数</h3><p className="mt-1 text-xs text-base-content/55">时长使用 Go duration 格式，例如 5s、30m、2h；保存无需重启或重载 sing-box</p></div>
            <button className="btn btn-primary btn-sm gap-2" disabled={!dirty || saving} onClick={() => void saveSettings()}>{saving ? <span className="loading loading-spinner loading-xs" /> : <Save className="h-4 w-4" />}保存参数</button>
          </div>
          <div className="grid gap-4 p-5 md:grid-cols-2 xl:grid-cols-3 lg:p-6">
            <Field id="probe-target" label="探测目标" hint="支持 HTTP/HTTPS；未填路径时使用 /generate_204" className="md:col-span-2 xl:col-span-3"><input id="probe-target" className={cn('input w-full font-mono text-sm', controlClass)} value={form.probe_target} onChange={(event) => update('probe_target', event.target.value)} placeholder="https://cp.cloudflare.com/generate_204" /></Field>
            <Field id="startup-policy" label="启动准入策略" hint="乐观模式先提供服务，最终失败后立即移出"><select id="startup-policy" className={cn('select w-full', controlClass)} value={form.startup_availability_policy} onChange={(event) => update('startup_availability_policy', event.target.value as 'optimistic' | 'strict')}><option value="optimistic">optimistic · 待复检节点临时可用</option><option value="strict">strict · 本轮成功后才可用</option></select></Field>
            <Field id="health-interval" label="健康检查间隔" hint="周期调度只探测已到期节点"><input id="health-interval" className={cn('input w-full font-mono', controlClass)} value={form.health_check_interval} onChange={(event) => update('health_check_interval', event.target.value)} /></Field>
            <Field id="probe-concurrency" label="批量并发" hint="0=自动：min(128, max(32, ceil(节点数/10)))"><input id="probe-concurrency" type="number" min={0} max={512} className={cn('input w-full tabular-nums', controlClass)} value={form.probe_concurrency} onChange={(event) => update('probe_concurrency', Number(event.target.value))} /></Field>
            <Field id="routine-retries" label="Routine 失败重试" hint="允许 0-2；首次探测固定重试一次"><input id="routine-retries" type="number" min={0} max={2} className={cn('input w-full tabular-nums', controlClass)} value={form.routine_probe_retries} onChange={(event) => update('routine_probe_retries', Number(event.target.value))} /></Field>
            <Field id="startup-timeout" label="启动单次尝试预算" hint="每次尝试独立计时，首次失败 500ms 后再试一次"><input id="startup-timeout" className={cn('input w-full font-mono', controlClass)} value={form.startup_probe_timeout} onChange={(event) => update('startup_probe_timeout', event.target.value)} /></Field>
            <Field id="routine-timeout" label="Routine 单节点总预算" hint="包含所有失败重试，默认 10s"><input id="routine-timeout" className={cn('input w-full font-mono', controlClass)} value={form.routine_probe_timeout} onChange={(event) => update('routine_probe_timeout', event.target.value)} /></Field>
            <Field id="dial-timeout" label="拨号阶段上限" hint="代理连接与出站握手的独立 deadline"><input id="dial-timeout" className={cn('input w-full font-mono', controlClass)} value={form.probe_dial_timeout} onChange={(event) => update('probe_dial_timeout', event.target.value)} /></Field>
            <Field id="response-timeout" label="响应阶段上限" hint="覆盖 TLS、HTTP 写入和响应头读取"><input id="response-timeout" className={cn('input w-full font-mono', controlClass)} value={form.probe_response_timeout} onChange={(event) => update('probe_response_timeout', event.target.value)} /></Field>
          </div>
        </section>

        <section className={cn(surfaceClass, 'overflow-hidden')}>
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-base-200 px-5 py-4 lg:px-6">
            <div><h3 className="flex items-center gap-2 text-lg font-bold"><Activity className="h-5 w-5 text-primary" />全量探测执行</h3><p className="mt-1 text-xs text-base-content/55">同一时刻只运行一轮；手动探测复用 Routine 策略，断开或取消会终止本轮</p></div>
            {liveRound?.kind && <span className={cn('badge', liveRound.in_flight ? 'badge-warning' : 'badge-ghost')}>{roundLabel(liveRound.kind)} · {liveRound.in_flight ? '运行中' : '最近一轮'}</span>}
          </div>
          <div className="space-y-5 p-5 lg:p-6">
            {visibleProgress ? <>
              <div className="grid gap-3 sm:grid-cols-4"><SmallMetric label="已完成" value={`${visibleProgress.current}/${visibleProgress.total}`} /><SmallMetric label="成功" value={visibleProgress.success} tone="text-success" /><SmallMetric label="失败" value={visibleProgress.failed} tone="text-error" /><SmallMetric label="进度" value={`${visibleProgress.percent.toFixed(1)}%`} tone="text-primary" /></div>
              {liveRound?.in_flight && <div className="flex flex-wrap gap-2 text-xs"><span className="badge badge-info badge-outline">第 {liveRound.attempt || 1} 次尝试</span><span className="badge badge-outline">活跃 Worker {liveRound.active_workers}</span>{liveRound.hard_timeouts > 0 && <span className="badge badge-warning badge-outline">硬超时 {liveRound.hard_timeouts}</span>}{liveRound.detached_probes > 0 && <span className="badge badge-error badge-outline">遗留调用 {liveRound.detached_probes}</span>}{liveRound.last_progress_at && <span className="badge badge-ghost" title={new Date(liveRound.last_progress_at).toLocaleString()}>最后进度 {new Date(liveRound.last_progress_at).toLocaleTimeString()}</span>}</div>}
              <progress className="progress progress-primary h-2 w-full" value={visibleProgress.percent} max="100" aria-label="全量探测进度" />
            </> : <div className="rounded-xl border border-dashed border-base-300 bg-base-200/20 px-5 py-8 text-center"><p className="font-medium text-base-content/65">当前没有运行中的全量探测</p><p className="mt-1 text-sm text-base-content/45">可从页面顶部启动，进度和最终结果会实时显示在这里</p></div>}

            {recentResults.length > 0 && <div className="overflow-x-auto"><table className="table table-sm"><thead><tr><th>节点</th><th>状态</th><th>延迟</th><th>错误</th></tr></thead><tbody>{recentResults.map((result) => <tr key={`${result.tag}-${result.current}`}><td><div className="max-w-56 truncate font-medium">{result.name || result.tag}</div><div className="max-w-56 truncate font-mono text-[11px] text-base-content/45">{result.tag}</div></td><td><span className={cn('badge badge-sm', result.status === 'success' ? 'badge-success' : 'badge-error')}>{result.status === 'success' ? '成功' : '失败'}</span></td><td className="font-mono tabular-nums">{result.latency >= 0 ? `${result.latency} ms` : '—'}</td><td className="max-w-md truncate text-xs text-error" title={result.error}>{result.error || '—'}</td></tr>)}</tbody></table></div>}
          </div>
        </section>
      </PageContent>
    </PageLayout>
  )
}

function Field({ id, label, hint, className, children }: { id: string; label: string; hint: string; className?: string; children: ReactNode }) {
  return <fieldset className={cn('fieldset', className)}><label htmlFor={id} className="fieldset-legend font-semibold text-base-content/80">{label}</label>{children}<p className="label mt-1 text-xs text-base-content/50">{hint}</p></fieldset>
}

function Metric({ label, value, detail, tone }: { label: string; value: string | number; detail: string; tone: string }) {
  return <div className={cn(surfaceClass, 'p-5')}><div className="text-sm font-medium text-base-content/55">{label}</div><div className={cn('mt-2 text-2xl font-black tabular-nums sm:text-3xl', tone)}>{value}</div><div className="mt-2 text-xs text-base-content/45">{detail}</div></div>
}

function SmallMetric({ label, value, tone = '' }: { label: string; value: string | number; tone?: string }) {
  return <div className="rounded-xl border border-base-200 bg-base-200/30 p-3"><div className="text-xs text-base-content/50">{label}</div><div className={cn('mt-1 text-lg font-bold tabular-nums', tone)}>{value}</div></div>
}

function roundLabel(kind: string): string {
  if (kind === 'startup') return '启动探测'
  if (kind === 'manual') return '手动探测'
  return '周期探测'
}
