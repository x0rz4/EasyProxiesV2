import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Activity, CheckCircle2, CircleAlert, Dices, Layers3, Network, Pencil,
  Plus, Power, RefreshCw, RotateCcw, Server, ShieldOff, Trash2, X,
} from 'lucide-react'
import { toast } from 'sonner'
import type { GroupPool, GroupPoolPayload, GroupMemberStatus } from '../types'
import {
  createGroupPool, deleteGroupPool, listGroupPools, restoreGroupMember, updateGroupPool,
} from '../api/client'
import { cn } from '../utils/cn'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'

const emptyPayload = (): GroupPoolPayload => ({
  name: '', bind_address: '0.0.0.0', bind_port: 0, protocol: 'mixed', username: '', password: '',
  dispatch_mode: 'fixed', regions: [], explicit_node_ids: [], failure_window_seconds: 300,
  failure_threshold: 3, health_check_seconds: 60, enabled: true,
})

const statusStyle: Record<GroupMemberStatus, { label: string; badge: string; icon: typeof CheckCircle2 }> = {
  ALIVE: { label: '可用', badge: 'badge-success', icon: CheckCircle2 },
  SUSPECT: { label: '观察中', badge: 'badge-warning', icon: CircleAlert },
  EVICTED: { label: '已踢出', badge: 'badge-error', icon: ShieldOff },
}

function payloadFromGroup(group: GroupPool): GroupPoolPayload {
  return {
    name: group.name, bind_address: group.bind_address, bind_port: group.bind_port,
    protocol: group.protocol, username: group.username || '', password: group.password || '',
    dispatch_mode: group.dispatch_mode, regions: group.regions || [],
    explicit_node_ids: group.explicit_node_ids || [], failure_window_seconds: group.failure_window_seconds,
    failure_threshold: group.failure_threshold, health_check_seconds: group.health_check_seconds,
    enabled: group.enabled,
  }
}

export default function GroupPoolsPanel() {
  const queryClient = useQueryClient()
  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ['groupPools'], queryFn: listGroupPools, refetchInterval: 10_000,
	})
	const groups = data?.groups || []
	const nodes = useMemo(() => data?.nodes || [], [data?.nodes])
  const [editorOpen, setEditorOpen] = useState(false)
  const [editing, setEditing] = useState<GroupPool | null>(null)
  const [form, setForm] = useState<GroupPoolPayload>(emptyPayload)
  const [regionsText, setRegionsText] = useState('')
  const [nodeSearch, setNodeSearch] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<GroupPool | null>(null)
  const [busy, setBusy] = useState<string | null>(null)

  const filteredNodes = useMemo(() => {
    const keyword = nodeSearch.trim().toLowerCase()
    if (!keyword) return nodes
    return nodes.filter((node) => `${node.name} ${node.region || ''} ${node.country || ''}`.toLowerCase().includes(keyword))
  }, [nodes, nodeSearch])

  const openCreate = () => {
    setEditing(null); setForm(emptyPayload()); setRegionsText(''); setNodeSearch(''); setEditorOpen(true)
  }
  const openEdit = (group: GroupPool) => {
    setEditing(group); setForm(payloadFromGroup(group)); setRegionsText((group.regions || []).join(', '));
    setNodeSearch(''); setEditorOpen(true)
  }
  const closeEditor = () => { setEditorOpen(false); setEditing(null) }

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ['groupPools'] })
    await queryClient.invalidateQueries({ queryKey: ['nodes'] })
  }
  const save = async () => {
    const payload = {
      ...form,
      name: form.name.trim(),
      regions: regionsText.split(/[,，\s]+/).map((value) => value.trim().toLowerCase()).filter(Boolean),
    }
    if (!payload.name) return
    setBusy('save')
    try {
      if (editing) await updateGroupPool(editing.id, payload)
      else await createGroupPool(payload)
      toast.success(editing ? '分组池已更新' : '分组池已创建并加载')
      closeEditor(); await refresh()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存分组池失败')
    } finally { setBusy(null) }
  }

  const run = async (key: string, task: () => Promise<unknown>, message: string) => {
    setBusy(key)
    try { await task(); toast.success(message); await refresh() }
    catch (error) { toast.error(error instanceof Error ? error.message : '操作失败') }
    finally { setBusy(null) }
  }

  const activeGroups = groups.filter((group) => group.enabled).length
  const totalMembers = groups.reduce((sum, group) => sum + group.member_count, 0)
  const evictedMembers = groups.reduce((sum, group) => sum + group.evicted_count, 0)

  if (isLoading) return <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>

  return (
    <PageLayout>
      <PageHeader title="分组池" description="一组节点一个独立端口，按地区或手动成员动态编排出口" icon={<Layers3 className="h-5 w-5" />}
        actions={<div className="flex gap-2">
          <button className="btn btn-ghost btn-sm btn-square" onClick={() => void refetch()} disabled={isFetching} aria-label="刷新分组池" title="刷新">
            <RefreshCw className={cn('h-4 w-4', isFetching && 'animate-spin')} />
          </button>
          <button className="btn btn-primary btn-sm gap-2 lg:btn-md" onClick={openCreate}><Plus className="h-4 w-4" /><span className="hidden sm:inline">新建分组</span></button>
        </div>} />

      <PageContent>
        <section className="grid grid-cols-2 gap-4 xl:grid-cols-4">
          <SummaryCard label="分组总数" value={groups.length} hint="独立入口池" icon={<Layers3 className="h-5 w-5" />} tone="text-primary" />
          <SummaryCard label="运行中" value={activeGroups} hint="已启用监听" icon={<Power className="h-5 w-5" />} tone="text-success" />
          <SummaryCard label="池内成员" value={totalMembers} hint="动态匹配节点" icon={<Network className="h-5 w-5" />} tone="text-info" />
          <SummaryCard label="永久踢出" value={evictedMembers} hint="等待手动恢复" icon={<ShieldOff className="h-5 w-5" />} tone={evictedMembers ? 'text-error' : 'text-base-content'} />
        </section>

        {groups.length === 0 ? (
          <section className={cn(surfaceClass, 'flex min-h-80 flex-col items-center justify-center border-dashed px-6 text-center')}>
            <div className="flex h-16 w-16 items-center justify-center rounded-2xl bg-primary/10 text-primary"><Layers3 className="h-8 w-8" /></div>
            <h3 className="mt-5 text-xl font-bold">创建第一个分组池</h3>
            <p className="mt-2 max-w-md text-sm leading-6 text-base-content/55">选择地区或指定节点，系统会分配独立端口，并在出口失败时自动热替换。</p>
            <button className="btn btn-primary mt-6 gap-2" onClick={openCreate}><Plus className="h-4 w-4" />新建分组</button>
          </section>
        ) : (
          <section className="grid items-start gap-5 xl:grid-cols-2">
            {groups.map((group) => <GroupCard key={group.id} group={group} busy={busy}
              onEdit={() => openEdit(group)} onDelete={() => setDeleteTarget(group)}
              onToggle={() => void run(`toggle-${group.id}`, () => updateGroupPool(group.id, { ...payloadFromGroup(group), enabled: !group.enabled }), group.enabled ? '分组已停用' : '分组已启用')}
              onRestore={(nodeId) => void run(`restore-${group.id}-${nodeId}`, () => restoreGroupMember(group.id, nodeId), '节点已恢复入池')} />)}
          </section>
        )}
      </PageContent>

      {editorOpen && <div className="modal modal-open" role="dialog" aria-modal="true" aria-label={editing ? '编辑分组池' : '新建分组池'}>
        <div className="modal-box max-h-[92vh] w-11/12 max-w-3xl overflow-y-auto p-0">
          <div className="sticky top-0 z-10 flex items-center justify-between border-b border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl">
            <div><h3 className="text-lg font-bold">{editing ? '编辑分组池' : '新建分组池'}</h3><p className="mt-0.5 text-xs text-base-content/50">地区与手动节点取并集，订阅刷新后会自动重新匹配</p></div>
            <button className="btn btn-ghost btn-sm btn-square" onClick={closeEditor} aria-label="关闭"><X className="h-4 w-4" /></button>
          </div>
          <div className="space-y-6 p-5 sm:p-6">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="分组名称"><input autoFocus className={cn('input w-full', controlClass)} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="例如：香港 VIP 专线" /></Field>
              <Field label="监听端口" hint="填 0 自动从 10000–19999 分配"><input type="number" min={0} max={65535} className={cn('input w-full font-mono', controlClass)} value={form.bind_port} onChange={(e) => setForm({ ...form, bind_port: Number(e.target.value) || 0 })} /></Field>
              <Field label="监听地址"><input className={cn('input w-full font-mono', controlClass)} value={form.bind_address} onChange={(e) => setForm({ ...form, bind_address: e.target.value })} /></Field>
              <Field label="入口协议"><select className={cn('select w-full', controlClass)} value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}><option value="mixed">Mixed (HTTP + SOCKS5)</option><option value="http">HTTP</option><option value="socks5">SOCKS5</option></select></Field>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <button type="button" className={cn('cursor-pointer rounded-2xl border p-4 text-left transition-colors', form.dispatch_mode === 'fixed' ? 'border-primary bg-primary/10' : 'border-base-300 hover:bg-base-200/50')} onClick={() => setForm({ ...form, dispatch_mode: 'fixed' })}>
                <div className="flex items-center gap-2 font-bold"><Server className="h-4 w-4" />固定出口</div><p className="mt-2 text-xs leading-5 text-base-content/55">保持当前主出口；失效或被踢出时自动切到下一可用节点。</p>
              </button>
              <button type="button" className={cn('cursor-pointer rounded-2xl border p-4 text-left transition-colors', form.dispatch_mode === 'random' ? 'border-primary bg-primary/10' : 'border-base-300 hover:bg-base-200/50')} onClick={() => setForm({ ...form, dispatch_mode: 'random' })}>
                <div className="flex items-center gap-2 font-bold"><Dices className="h-4 w-4" />随机出口</div><p className="mt-2 text-xs leading-5 text-base-content/55">每个新连接在全部健康成员中随机选择出口。</p>
              </button>
            </div>

            <div className="grid gap-4 sm:grid-cols-3">
              <Field label="失败窗口（秒）"><input type="number" min={30} className={cn('input w-full', controlClass)} value={form.failure_window_seconds} onChange={(e) => setForm({ ...form, failure_window_seconds: Number(e.target.value) })} /></Field>
              <Field label="踢出阈值（次）"><input type="number" min={1} className={cn('input w-full', controlClass)} value={form.failure_threshold} onChange={(e) => setForm({ ...form, failure_threshold: Number(e.target.value) })} /></Field>
              <Field label="测活间隔（秒）"><input type="number" min={10} className={cn('input w-full', controlClass)} value={form.health_check_seconds} onChange={(e) => setForm({ ...form, health_check_seconds: Number(e.target.value) })} /></Field>
            </div>

            <Field label="地区自动入池" hint="ISO 地区码，用逗号或空格分隔，例如 hk, jp, us"><input className={cn('input w-full font-mono', controlClass)} value={regionsText} onChange={(e) => setRegionsText(e.target.value)} placeholder="hk, jp" /></Field>

            <div className="rounded-2xl border border-base-300 bg-base-200/25 p-4">
              <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"><div><h4 className="font-bold">手动指定节点</h4><p className="mt-0.5 text-xs text-base-content/50">已选择 {form.explicit_node_ids.length} 个；可与地区规则同时使用</p></div><input className="input input-sm w-full sm:w-64" value={nodeSearch} onChange={(e) => setNodeSearch(e.target.value)} placeholder="搜索节点或地区" /></div>
              <div className="mt-3 max-h-52 space-y-1 overflow-y-auto pr-1">
                {filteredNodes.map((node) => <label key={node.id} className="flex cursor-pointer items-center gap-3 rounded-xl px-3 py-2.5 transition-colors hover:bg-base-200">
                  <input type="checkbox" className="checkbox checkbox-primary checkbox-sm" checked={form.explicit_node_ids.includes(node.id)} onChange={() => setForm({ ...form, explicit_node_ids: form.explicit_node_ids.includes(node.id) ? form.explicit_node_ids.filter((id) => id !== node.id) : [...form.explicit_node_ids, node.id] })} />
                  <span className="min-w-0 flex-1 truncate text-sm font-medium">{node.name}</span>{node.region && <span className="badge badge-ghost badge-sm uppercase">{node.region}</span>}
                </label>)}
                {filteredNodes.length === 0 && <p className="py-8 text-center text-sm text-base-content/45">没有匹配的节点</p>}
              </div>
            </div>

            <details className="collapse-arrow collapse rounded-2xl border border-base-300 bg-base-200/20"><summary className="collapse-title min-h-0 py-3 text-sm font-semibold">入口认证（可选）</summary><div className="collapse-content grid gap-4 sm:grid-cols-2"><Field label="用户名"><input className={cn('input w-full', controlClass)} value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></Field><Field label="密码"><input type="password" className={cn('input w-full', controlClass)} value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} /></Field></div></details>
          </div>
          <div className="sticky bottom-0 flex justify-end gap-2 border-t border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl"><button className="btn btn-ghost" onClick={closeEditor}>取消</button><button className="btn btn-primary min-w-28" disabled={!form.name.trim() || busy === 'save'} onClick={() => void save()}>{busy === 'save' && <span className="loading loading-spinner loading-sm" />}{editing ? '保存修改' : '创建并加载'}</button></div>
        </div><button className="modal-backdrop" onClick={closeEditor} aria-label="关闭对话框" />
      </div>}

      {deleteTarget && <div className="modal modal-open" role="dialog" aria-modal="true"><div className="modal-box max-w-md"><h3 className="text-lg font-bold">删除“{deleteTarget.name}”？</h3><p className="mt-3 text-sm leading-6 text-base-content/60">独立端口将立即停止监听，分组状态与失败历史也会一并删除。</p><div className="modal-action"><button className="btn btn-ghost" onClick={() => setDeleteTarget(null)}>取消</button><button className="btn btn-error" disabled={busy === `delete-${deleteTarget.id}`} onClick={() => void run(`delete-${deleteTarget.id}`, () => deleteGroupPool(deleteTarget.id), '分组池已删除').then(() => setDeleteTarget(null))}>{busy === `delete-${deleteTarget.id}` && <span className="loading loading-spinner loading-sm" />}确认删除</button></div></div><button className="modal-backdrop" onClick={() => setDeleteTarget(null)} aria-label="关闭对话框" /></div>}
    </PageLayout>
  )
}

function SummaryCard({ label, value, hint, icon, tone }: { label: string; value: number; hint: string; icon: React.ReactNode; tone: string }) {
  return <article className={cn(surfaceClass, 'p-4 sm:p-5')}><div className="flex items-center justify-between"><span className="text-sm font-medium text-base-content/55">{label}</span><span className={cn('rounded-xl bg-base-200 p-2', tone)}>{icon}</span></div><div className={cn('mt-2 text-3xl font-black tabular-nums', tone)}>{value}</div><p className="mt-1 text-xs text-base-content/45">{hint}</p></article>
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">{label}</legend>{children}{hint && <p className="mt-1 text-xs text-base-content/45">{hint}</p>}</fieldset>
}

function GroupCard({ group, busy, onEdit, onDelete, onToggle, onRestore }: { group: GroupPool; busy: string | null; onEdit: () => void; onDelete: () => void; onToggle: () => void; onRestore: (nodeId: number) => void }) {
  const active = group.members.find((member) => member.is_active)
  return <article className={cn(surfaceClass, 'overflow-hidden transition-colors', !group.enabled && 'opacity-70')}>
    <div className="border-b border-base-200 p-5">
      <div className="flex items-start gap-3"><div className={cn('flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl', group.enabled ? 'bg-primary/10 text-primary' : 'bg-base-200 text-base-content/40')}><Layers3 className="h-5 w-5" /></div><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h3 className="truncate text-lg font-bold">{group.name}</h3><span className={cn('badge badge-sm', group.enabled ? 'badge-success' : 'badge-ghost')}>{group.enabled ? '运行中' : '已停用'}</span><span className="badge badge-outline badge-sm">{group.dispatch_mode === 'fixed' ? '固定出口' : '随机出口'}</span></div><div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-base-content/55"><code className="font-semibold text-primary">{group.bind_address}:{group.bind_port}</code><span className="uppercase">{group.protocol}</span><span>{group.failure_window_seconds / 60} 分钟 / {group.failure_threshold} 次踢出</span></div></div><div className="dropdown dropdown-end"><button tabIndex={0} className="btn btn-ghost btn-sm btn-square" aria-label="分组操作"><Pencil className="h-4 w-4" /></button><ul tabIndex={0} className="dropdown-content z-20 mt-1 w-36 rounded-box border border-base-300 bg-base-100 p-1.5 shadow-xl"><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-base-200" onClick={onEdit}><Pencil className="h-4 w-4" />编辑</button></li><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-base-200" onClick={onToggle}><Power className="h-4 w-4" />{group.enabled ? '停用' : '启用'}</button></li><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm text-error hover:bg-error/10" onClick={onDelete}><Trash2 className="h-4 w-4" />删除</button></li></ul></div></div>
      <div className="mt-4 grid grid-cols-3 gap-2"><MiniMetric label="成员" value={group.member_count} /><MiniMetric label="健康" value={group.alive_count} tone="text-success" /><MiniMetric label="踢出" value={group.evicted_count} tone={group.evicted_count ? 'text-error' : ''} /></div>
      {(group.regions?.length > 0 || group.explicit_node_ids?.length > 0) && <div className="mt-4 flex flex-wrap gap-1.5">{group.regions?.map((region) => <span key={region} className="badge badge-primary badge-outline badge-sm uppercase">{region}</span>)}{group.explicit_node_ids?.length > 0 && <span className="badge badge-ghost badge-sm">手动 {group.explicit_node_ids.length} 个</span>}</div>}
    </div>
    <div className="p-4 sm:p-5">
      {group.dispatch_mode === 'fixed' && <div className={cn('mb-3 flex items-center gap-3 rounded-xl border px-3 py-2.5', active ? 'border-success/25 bg-success/5' : 'border-warning/25 bg-warning/5')}><Activity className={cn('h-4 w-4 shrink-0', active ? 'text-success' : 'text-warning')} /><div className="min-w-0 flex-1"><p className="text-[11px] font-semibold uppercase tracking-wider text-base-content/45">当前主出口</p><p className="truncate text-sm font-semibold">{active?.name || '等待首个连接选择'}</p></div>{active?.latency_ms && active.latency_ms > 0 ? <span className="text-xs font-mono text-base-content/55">{active.latency_ms} ms</span> : null}</div>}
      <div className="space-y-2">{group.members.slice(0, 8).map((member) => { const style = statusStyle[member.status]; const StatusIcon = style.icon; return <div key={member.node_id} className="flex items-center gap-3 rounded-xl border border-base-200 bg-base-200/20 px-3 py-2.5"><StatusIcon className={cn('h-4 w-4 shrink-0', member.status === 'ALIVE' ? 'text-success' : member.status === 'SUSPECT' ? 'text-warning' : 'text-error')} /><div className="min-w-0 flex-1"><div className="flex items-center gap-2"><span className="truncate text-sm font-medium">{member.name || member.tag}</span>{member.region && <span className="text-[10px] font-bold uppercase text-base-content/40">{member.region}</span>}</div>{member.last_error && <p className="mt-0.5 truncate text-[11px] text-error/75" title={member.last_error}>{member.last_error}</p>}</div><span className={cn('badge badge-sm', style.badge)}>{style.label}</span>{member.latency_ms > 0 && <span className="hidden w-14 text-right font-mono text-xs text-base-content/45 sm:block">{member.latency_ms} ms</span>}{member.status === 'EVICTED' && <button className="btn btn-ghost btn-xs gap-1 text-primary" disabled={busy === `restore-${group.id}-${member.node_id}`} onClick={() => onRestore(member.node_id)} title="恢复入池"><RotateCcw className="h-3 w-3" /><span className="hidden sm:inline">恢复</span></button>}</div> })}</div>
      {group.member_count === 0 && <div className="rounded-xl border border-dashed border-warning/40 bg-warning/5 px-4 py-8 text-center"><CircleAlert className="mx-auto h-6 w-6 text-warning" /><p className="mt-2 text-sm font-medium">当前没有匹配的有效节点</p><p className="mt-1 text-xs text-base-content/45">检查地区码、手动成员或节点启用状态</p></div>}
      {group.member_count > 8 && <p className="mt-3 text-center text-xs text-base-content/45">还有 {group.member_count - 8} 个成员未展开</p>}
    </div>
  </article>
}

function MiniMetric({ label, value, tone = '' }: { label: string; value: number; tone?: string }) {
  return <div className="rounded-xl bg-base-200/55 px-3 py-2 text-center"><div className={cn('text-lg font-black tabular-nums', tone)}>{value}</div><div className="text-[11px] text-base-content/45">{label}</div></div>
}
