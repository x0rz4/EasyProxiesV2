import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Activity, CheckCircle2, ChevronDown, CircleAlert, Copy, KeyRound, Layers3, Link2, Network, Pencil,
  Plus, Power, RefreshCw, RotateCcw, Search, ShieldOff, Trash2, X, Crosshair,
} from 'lucide-react'
import { toast } from 'sonner'
import type { GroupMember, GroupNodeOption, GroupPool, GroupPoolPayload, GroupMemberStatus } from '../types'
import {
  activateGroupMember, createGroupPool, deleteGroupPool, listGroupPools, probeNode, removeGroupMember,
  resetGroupSubscriptionToken, restoreGroupMember, unexcludeGroupMember, updateGroupPool,
} from '../api/client'
import { cn } from '../utils/cn'
import { controlClass, PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'

const emptyPayload = (): GroupPoolPayload => ({
  name: '', bind_address: '0.0.0.0', bind_port: 0, protocol: 'mixed', username: '', password: '',
  dispatch_mode: 'fixed', regions: [], explicit_node_ids: [], failure_window_seconds: 300,
  excluded_node_ids: [],
  failure_threshold: 3, health_check_seconds: 60, enabled: true,
	subscription_enabled: true, subscription_mode: 'entry', external_host: '',
})

const statusStyle: Record<GroupMemberStatus, { label: string; badge: string; icon: typeof CheckCircle2 }> = {
  ALIVE: { label: '可用', badge: 'badge-success', icon: CheckCircle2 },
  SUSPECT: { label: '观察中', badge: 'badge-warning', icon: CircleAlert },
  EVICTED: { label: '已踢出', badge: 'badge-error', icon: ShieldOff },
}

const nodeStatusStyle: Record<GroupNodeOption['status'], { label: string; badge: string }> = {
  normal: { label: '可用', badge: 'badge-success' },
  unavailable: { label: '不可用', badge: 'badge-error' },
  blacklisted: { label: '已拉黑', badge: 'badge-warning' },
  pending: { label: '待检测', badge: 'badge-ghost' },
  disabled: { label: '已禁用', badge: 'badge-ghost' },
}

const dispatchModeLabel: Record<GroupPool['dispatch_mode'], string> = {
  fixed: '固定出口',
  lowest_latency: '延迟最低',
  random: '随机出口',
}

const dispatchModeHint: Record<GroupPoolPayload['dispatch_mode'], string> = {
  fixed: '保持当前健康出口；失效后按成员列表顺序切换到下一个可用节点。',
  lowest_latency: '保持当前健康出口；首次选择或失效后切换到延迟最低的可用节点。',
  random: '每个新连接在全部健康成员中随机选择出口。',
}

const runtimeStatusStyle: Record<GroupPool['runtime_status'], { label: string; badge: string }> = {
  starting: { label: '启动中', badge: 'badge-info' },
  ready: { label: '运行中', badge: 'badge-success' },
  reconfiguring: { label: '更新中', badge: 'badge-info' },
  stopped: { label: '已停用', badge: 'badge-ghost' },
  error: { label: '运行错误', badge: 'badge-error' },
}

async function copyTextWithFallback(value: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value)
      return true
    } catch { /* fall through for HTTP origins and denied permissions */ }
  }
  const textarea = document.createElement('textarea')
  textarea.value = value
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.left = '-9999px'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  textarea.setSelectionRange(0, value.length)
  try {
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    textarea.remove()
  }
}

function payloadFromGroup(group: GroupPool): GroupPoolPayload {
  return {
    name: group.name, bind_address: group.bind_address, bind_port: group.bind_port,
    protocol: group.protocol, username: group.username || '', password: group.password || '',
    dispatch_mode: group.dispatch_mode, regions: group.regions || [],
    explicit_node_ids: group.explicit_node_ids || [], excluded_node_ids: group.excluded_node_ids || [], failure_window_seconds: group.failure_window_seconds,
    failure_threshold: group.failure_threshold, health_check_seconds: group.health_check_seconds,
    enabled: group.enabled,
		subscription_enabled: group.subscription_enabled, subscription_mode: group.subscription_mode || 'entry',
		external_host: group.external_host || '',
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
  const [expandedGroups, setExpandedGroups] = useState<Set<number>>(() => new Set())
  const [removedDraftNodes, setRemovedDraftNodes] = useState<string[]>([])
  const [pendingAutoExcludedIDs, setPendingAutoExcludedIDs] = useState<Set<number>>(() => new Set())
  const [draftNodeCache, setDraftNodeCache] = useState<Record<number, GroupNodeOption>>({})
  const [probingNodeIDs, setProbingNodeIDs] = useState<Set<number>>(() => new Set())
  const [latencyOverrides, setLatencyOverrides] = useState<Record<number, number>>({})
  const [batchProbeProgress, setBatchProbeProgress] = useState<{ current: number; total: number } | null>(null)

  const filteredNodes = useMemo(() => {
    const keyword = nodeSearch.trim().toLowerCase()
    return nodes.filter((node) => node.selectable && !form.explicit_node_ids.includes(node.id))
      .filter((node) => !keyword || `${node.name} ${node.region || ''} ${node.country || ''}`.toLowerCase().includes(keyword))
  }, [nodes, nodeSearch, form.explicit_node_ids])

  const selectedNodeOptions = useMemo(() => {
    const byID = new Map(nodes.map((node) => [node.id, node]))
    return form.explicit_node_ids.map((id) => {
      const current = byID.get(id)
      if (current) return current
      const cached = draftNodeCache[id]
      return cached ? { ...cached, status: 'unavailable' as const, available: false, selectable: false, latency_ms: -1 } : undefined
    }).filter((node): node is GroupNodeOption => Boolean(node))
  }, [nodes, form.explicit_node_ids, draftNodeCache])

  const excludedDraftNodes = useMemo(() => {
    const nodeByID = new Map(nodes.map((node) => [node.id, node]))
    const runtimeByID = new Map((editing?.members || []).map((member) => [member.node_id, member]))
    return form.excluded_node_ids.map((id) => {
      const node = nodeByID.get(id)
      const member = runtimeByID.get(id)
      return {
        id,
        name: node?.name || member?.name || member?.tag || `节点 #${id}`,
        region: node?.region || member?.region || '',
        status: node?.status,
        pending: pendingAutoExcludedIDs.has(id),
      }
    })
  }, [nodes, editing?.members, form.excluded_node_ids, pendingAutoExcludedIDs])

  const openCreate = () => {
    setEditing(null); setForm(emptyPayload()); setRegionsText(''); setNodeSearch(''); setRemovedDraftNodes([]); setPendingAutoExcludedIDs(new Set()); setDraftNodeCache({}); setEditorOpen(true)
  }
  const openEdit = (group: GroupPool) => {
    const nodeByID = new Map(nodes.map((node) => [node.id, node]))
    const evictedIDs = group.members.filter((member) => member.status === 'EVICTED').map((member) => member.node_id)
    const evictedSet = new Set(evictedIDs)
    const excludedIDs = [...new Set([...(group.excluded_node_ids || []), ...evictedIDs])]
    const keptIDs = group.explicit_node_ids.filter((id) => nodeByID.get(id)?.selectable && !evictedSet.has(id))
    const removedNames = group.explicit_node_ids.filter((id) => !nodeByID.get(id)?.selectable).map((id) =>
      nodeByID.get(id)?.name || group.members.find((member) => member.node_id === id)?.name || `节点 #${id}`)
    const cache = Object.fromEntries(keptIDs.map((id) => [id, nodeByID.get(id)]).filter((entry): entry is [number, GroupNodeOption] => Boolean(entry[1])))
    setEditing(group); setForm({ ...payloadFromGroup(group), explicit_node_ids: keptIDs, excluded_node_ids: excludedIDs }); setRegionsText((group.regions || []).join(', '));
    setNodeSearch(''); setRemovedDraftNodes(removedNames); setPendingAutoExcludedIDs(new Set(evictedIDs.filter((id) => !group.excluded_node_ids.includes(id)))); setDraftNodeCache(cache); setEditorOpen(true)
  }
  const closeEditor = () => { setEditorOpen(false); setEditing(null); setRemovedDraftNodes([]); setPendingAutoExcludedIDs(new Set()); setBatchProbeProgress(null) }

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ['groupPools'] })
    await queryClient.invalidateQueries({ queryKey: ['nodes'] })
  }
  const save = async () => {
    const name = form.name.trim()
    if (!name) return
    setBusy('save')
    try {
      // Re-read immediately before saving so an eviction that happened while
      // the editor was open is persisted even if the polling cache is stale.
      const latestSnapshot = editing ? await listGroupPools() : data
      const latestGroup = editing ? latestSnapshot?.groups.find((group) => group.id === editing.id) : undefined
      const latestEvictedIDs = latestGroup?.members.filter((member) => member.status === 'EVICTED').map((member) => member.node_id) || []
      const excludedNodeIDs = [...new Set([...form.excluded_node_ids, ...latestEvictedIDs])]
      const excludedSet = new Set(excludedNodeIDs)
      const latestNodes = latestSnapshot?.nodes || nodes
      const selectableIDs = new Set(latestNodes.filter((node) => node.selectable).map((node) => node.id))
      const keptNodeIDs = form.explicit_node_ids.filter((id) => selectableIDs.has(id) && !excludedSet.has(id))
      const locallyRemoved = form.explicit_node_ids.length - keptNodeIDs.length
      const payload = {
        ...form,
        name,
        regions: regionsText.split(/[,，\s]+/).map((value) => value.trim().toLowerCase()).filter(Boolean),
        explicit_node_ids: keptNodeIDs,
        excluded_node_ids: excludedNodeIDs,
      }
      const result = editing ? await updateGroupPool(editing.id, payload) : await createGroupPool(payload)
      const removedCount = locallyRemoved + (result.removed_unavailable_node_ids?.length || 0)
      toast.success(editing ? '分组池已更新' : '分组池已创建并加载', removedCount > 0
        ? { description: `${removedCount} 个不可用手动节点未被保存` } : undefined)
      closeEditor(); await refresh()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存分组池失败')
    } finally { setBusy(null) }
  }

  const run = async (key: string, task: () => Promise<unknown>, message: string) => {
    setBusy(key)
    try {
      const result = await task()
      const reloadResult = result as { reloaded?: boolean; reload_error?: string } | undefined
      if (reloadResult?.reloaded === false) {
        toast.warning(message, { description: `配置已保存，但自动重载失败：${reloadResult.reload_error || '请手动重试'}` })
      } else {
        toast.success(message)
      }
      await refresh()
    }
    catch (error) { toast.error(error instanceof Error ? error.message : '操作失败') }
    finally { setBusy(null) }
  }

  const removeManualNode = (node: GroupNodeOption) => {
    setForm((current) => ({ ...current, explicit_node_ids: current.explicit_node_ids.filter((id) => id !== node.id) }))
    const regions = new Set(regionsText.split(/[,，\s]+/).map((value) => value.trim().toLowerCase()).filter(Boolean))
    if (node.region && regions.has(node.region.toLowerCase())) {
      toast.info('已取消手动指定', { description: `${node.name} 仍匹配地区规则，会继续自动入池` })
    }
  }

  const removeRuntimeMemberImmediately = async (member: GroupMember) => {
    if (!editing || !window.confirm(`确认立即将“${member.name || member.tag}”从分组“${editing.name}”移除？\n节点本身不会从节点管理中删除。`)) return
    const key = `remove-member-${editing.id}-${member.node_id}`
    setBusy(key)
    try {
      await removeGroupMember(editing.id, member.node_id)
      setForm((current) => ({ ...current,
        explicit_node_ids: current.explicit_node_ids.filter((id) => id !== member.node_id),
        excluded_node_ids: [...new Set([...current.excluded_node_ids, member.node_id])],
      }))
      setPendingAutoExcludedIDs((current) => { const next = new Set(current); next.delete(member.node_id); return next })
      setEditing((current) => current ? { ...current,
        members: current.members.filter((item) => item.node_id !== member.node_id),
        member_count: Math.max(0, current.member_count - 1),
        evicted_count: Math.max(0, current.evicted_count - (member.status === 'EVICTED' ? 1 : 0)),
        alive_count: Math.max(0, current.alive_count - (member.status === 'ALIVE' ? 1 : 0)),
        explicit_node_ids: current.explicit_node_ids.filter((id) => id !== member.node_id),
        excluded_node_ids: [...new Set([...current.excluded_node_ids, member.node_id])],
      } : null)
      toast.success('节点已从当前分组移除')
      await refresh()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '移除成员失败')
    } finally {
      setBusy(null)
    }
  }

  const probeOption = async (node: Pick<GroupNodeOption, 'id' | 'name' | 'tag'>, quiet = false) => {
    if (!node.tag) return false
    setProbingNodeIDs((current) => new Set(current).add(node.id))
    try {
      const result = await probeNode(node.tag)
      setLatencyOverrides((current) => ({ ...current, [node.id]: result.latency_ms }))
      if (!quiet) toast.success(`${node.name} 延迟 ${result.latency_ms} ms`)
      return true
    } catch (error) {
      const removed = form.explicit_node_ids.includes(node.id)
      setForm((current) => {
        if (!current.explicit_node_ids.includes(node.id)) return current
        return { ...current, explicit_node_ids: current.explicit_node_ids.filter((id) => id !== node.id) }
      })
      if (removed) setRemovedDraftNodes((current) => current.includes(node.name) ? current : [...current, node.name])
      if (!quiet) toast.error(error instanceof Error ? error.message : '延迟测试失败', removed
        ? { description: `${node.name} 已从当前编辑草稿移除` } : undefined)
      return false
    } finally {
      setProbingNodeIDs((current) => { const next = new Set(current); next.delete(node.id); return next })
      if (!quiet) {
        await queryClient.invalidateQueries({ queryKey: ['groupPools'] })
        await queryClient.invalidateQueries({ queryKey: ['nodes'] })
      }
    }
  }

  const probeSelectedNodes = async () => {
    const targets = selectedNodeOptions.filter((node) => node.tag)
    if (targets.length === 0) return toast.error('已选节点中没有可测试的运行节点')
    setBatchProbeProgress({ current: 0, total: targets.length })
    let succeeded = 0
    let failed = 0
    let completed = 0
    for (let index = 0; index < targets.length; index += 10) {
      const batch = targets.slice(index, index + 10)
      const results = await Promise.all(batch.map(async (node) => {
        const result = await probeOption(node, true)
        completed++
        setBatchProbeProgress({ current: completed, total: targets.length })
        return result
      }))
      succeeded += results.filter(Boolean).length
      failed += results.filter((result) => !result).length
    }
    setBatchProbeProgress(null)
    await queryClient.invalidateQueries({ queryKey: ['groupPools'] })
    await queryClient.invalidateQueries({ queryKey: ['nodes'] })
    toast.success(`批量测试完成：${succeeded} 成功，${failed} 失败`, failed > 0
      ? { description: '失败节点已从当前编辑草稿移除' } : undefined)
  }

  const activeGroups = groups.filter((group) => group.runtime_status === 'ready').length
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
              nodeOptions={nodes}
              expanded={expandedGroups.has(group.id)}
              onToggleExpanded={() => setExpandedGroups((current) => { const next = new Set(current); if (next.has(group.id)) next.delete(group.id); else next.add(group.id); return next })}
              onEdit={() => openEdit(group)} onDelete={() => setDeleteTarget(group)}
              onToggle={() => void run(`toggle-${group.id}`, () => updateGroupPool(group.id, { ...payloadFromGroup(group), enabled: !group.enabled }), group.enabled ? '分组已停用' : '分组已启用')}
				onResetToken={() => void run(`token-${group.id}`, () => resetGroupSubscriptionToken(group.id), '订阅 Token 已重置，旧链接已失效')}
              onRestore={(nodeId) => void run(`restore-${group.id}-${nodeId}`, () => restoreGroupMember(group.id, nodeId), '节点已恢复入池')}
              onActivate={(nodeId) => void run(`activate-${group.id}-${nodeId}`, () => activateGroupMember(group.id, nodeId), '当前出口已立即切换')}
              onUnexclude={(nodeId) => void run(`unexclude-${group.id}-${nodeId}`, () => unexcludeGroupMember(group.id, nodeId), '节点已取消排除')}
              onRemoveMember={(member) => {
                if (!window.confirm(`确认将“${member.name || member.tag}”从分组“${group.name}”移除？\n节点本身不会从节点管理中删除。`)) return
                void run(`remove-member-${group.id}-${member.node_id}`, () => removeGroupMember(group.id, member.node_id), '节点已从当前分组移除')
              }} />)}
          </section>
        )}
      </PageContent>

      {editorOpen && <div className="modal modal-open" role="dialog" aria-modal="true" aria-label={editing ? '编辑分组池' : '新建分组池'}>
        <div className="modal-box max-h-[92vh] w-11/12 max-w-5xl overflow-y-auto p-0">
          <div className="sticky top-0 z-10 flex items-center justify-between border-b border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl">
            <div><h3 className="text-lg font-bold">{editing ? '编辑分组池' : '新建分组池'}</h3><p className="mt-0.5 text-xs text-base-content/50">地区与手动节点取并集，订阅刷新后会自动重新匹配</p></div>
            <button className="btn btn-ghost btn-sm btn-square" onClick={closeEditor} aria-label="关闭"><X className="h-4 w-4" /></button>
          </div>
          <div className="space-y-6 p-5 sm:p-6">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="分组名称"><input autoFocus className={cn('input w-full', controlClass)} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="例如：香港 VIP 专线" /></Field>
              <Field label="监听端口" hint={`填 0 自动从 ${data?.port_range?.start ?? 10000}–${data?.port_range?.end ?? 19999} 分配`}><input type="number" min={0} max={65535} className={cn('input w-full font-mono', controlClass)} value={form.bind_port} onChange={(e) => setForm({ ...form, bind_port: Number(e.target.value) || 0 })} /></Field>
              <Field label="监听地址"><input className={cn('input w-full font-mono', controlClass)} value={form.bind_address} onChange={(e) => setForm({ ...form, bind_address: e.target.value })} /></Field>
              <Field label="入口协议"><select className={cn('select w-full', controlClass)} value={form.protocol} onChange={(e) => setForm({ ...form, protocol: e.target.value })}><option value="mixed">Mixed (HTTP + SOCKS5)</option><option value="http">HTTP</option><option value="socks5">SOCKS5</option></select></Field>
            </div>

            <Field label="出口调度模式" hint={dispatchModeHint[form.dispatch_mode]}>
              <select className={cn('select w-full', controlClass)} value={form.dispatch_mode} onChange={(event) => setForm({ ...form, dispatch_mode: event.target.value as GroupPoolPayload['dispatch_mode'] })}>
                <option value="fixed">固定出口模式</option>
                <option value="lowest_latency">延迟最低模式</option>
                <option value="random">随机出口模式</option>
              </select>
            </Field>

            <div className="grid gap-4 sm:grid-cols-3">
              <Field label="失败窗口（秒）"><input type="number" min={30} className={cn('input w-full', controlClass)} value={form.failure_window_seconds} onChange={(e) => setForm({ ...form, failure_window_seconds: Number(e.target.value) })} /></Field>
              <Field label="踢出阈值（次）"><input type="number" min={1} className={cn('input w-full', controlClass)} value={form.failure_threshold} onChange={(e) => setForm({ ...form, failure_threshold: Number(e.target.value) })} /></Field>
              <Field label="测活间隔（秒）"><input type="number" min={10} className={cn('input w-full', controlClass)} value={form.health_check_seconds} onChange={(e) => setForm({ ...form, health_check_seconds: Number(e.target.value) })} /></Field>
            </div>

            <Field label="地区自动入池" hint="ISO 地区码，用逗号或空格分隔，例如 hk, jp, us"><input className={cn('input w-full font-mono', controlClass)} value={regionsText} onChange={(e) => setRegionsText(e.target.value)} placeholder="hk, jp" /></Field>

			<div className="rounded-2xl border border-primary/20 bg-primary/5 p-4 sm:p-5">
				<div className="flex items-start gap-3"><div className="rounded-xl bg-primary/10 p-2 text-primary"><Link2 className="h-4 w-4" /></div><div className="min-w-0 flex-1"><div className="flex items-center justify-between gap-3"><div><h4 className="font-bold">对外订阅</h4><p className="mt-0.5 text-xs leading-5 text-base-content/55">独立 Token 鉴权；members 会暴露真实上游地址</p></div><input type="checkbox" className="toggle toggle-primary" checked={form.subscription_enabled} onChange={(e) => setForm({ ...form, subscription_enabled: e.target.checked })} aria-label="启用分组订阅" /></div>
				{form.subscription_enabled && <div className="mt-4 grid gap-4 sm:grid-cols-2"><Field label="默认输出模式"><select className={cn('select w-full', controlClass)} value={form.subscription_mode} onChange={(e) => setForm({ ...form, subscription_mode: e.target.value as 'members' | 'entry' })}><option value="entry">组入口（推荐中转）</option><option value="members">健康成员（暴露上游）</option></select></Field><Field label="外部主机覆盖" hint="留空使用系统 external_ip 或请求域名"><input className={cn('input w-full font-mono', controlClass)} value={form.external_host} onChange={(e) => setForm({ ...form, external_host: e.target.value })} placeholder="ep.example.com" /></Field></div>}
				</div></div>
			</div>

            <section aria-labelledby="member-editor-title">
              <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
                <div><h4 id="member-editor-title" className="font-bold">节点成员管理</h4><p className="mt-1 text-xs leading-5 text-base-content/55">只能新增节点管理中状态可用的节点；地区规则与手动节点取并集</p></div>
                {selectedNodeOptions.length > 0 && <button type="button" className="btn btn-outline btn-sm gap-2" onClick={() => void probeSelectedNodes()} disabled={Boolean(batchProbeProgress)}>
                  {batchProbeProgress ? <span className="loading loading-spinner loading-xs" /> : <Activity className="h-4 w-4" />}
                  {batchProbeProgress ? `${batchProbeProgress.current}/${batchProbeProgress.total}` : '测试全部已选'}
                </button>}
              </div>

              {removedDraftNodes.length > 0 && <div role="status" className="mt-3 flex items-start gap-2 rounded-xl border border-warning/30 bg-warning/8 px-3 py-2.5 text-xs leading-5 text-warning-content">
                <CircleAlert className="mt-0.5 h-4 w-4 shrink-0 text-warning" /><span><strong>已从编辑草稿移除 {removedDraftNodes.length} 个不可用节点：</strong> {removedDraftNodes.join('、')}。取消编辑不会修改原配置，保存后生效。</span>
              </div>}

              <div className="mt-4 grid gap-4 lg:grid-cols-2">
                <div className="rounded-2xl border border-base-300 bg-base-200/25 p-4">
                  <div><h5 className="text-sm font-bold">已手动指定</h5><p className="mt-0.5 text-xs text-base-content/50">当前草稿共 {selectedNodeOptions.length} 个节点</p></div>
                  <div className="mt-3 max-h-72 space-y-2 overflow-y-auto pr-1">
                    {selectedNodeOptions.map((node) => <NodeOptionRow key={node.id} node={node} latency={latencyOverrides[node.id] ?? node.latency_ms} probing={probingNodeIDs.has(node.id)}
                      action="remove" onAction={() => removeManualNode(node)} onProbe={() => void probeOption(node)} />)}
                    {selectedNodeOptions.length === 0 && <EmptyMemberList message="尚未手动指定节点" />}
                  </div>
                </div>

                <div className="rounded-2xl border border-base-300 bg-base-200/25 p-4">
                  <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"><div><h5 className="text-sm font-bold">可添加节点</h5><p className="mt-0.5 text-xs text-base-content/50">仅显示节点管理状态为“可用”的节点</p></div><label className="input input-sm flex w-full items-center gap-2 sm:w-56"><Search className="h-3.5 w-3.5 text-base-content/40" /><input className="min-w-0 grow" value={nodeSearch} onChange={(e) => setNodeSearch(e.target.value)} placeholder="搜索节点或地区" aria-label="搜索可添加节点" /></label></div>
                  <div className="mt-3 max-h-72 space-y-2 overflow-y-auto pr-1">
                    {filteredNodes.map((node) => <NodeOptionRow key={node.id} node={node} latency={latencyOverrides[node.id] ?? node.latency_ms} probing={probingNodeIDs.has(node.id)}
                      action="add" onAction={() => { setDraftNodeCache((current) => ({ ...current, [node.id]: node })); setForm((current) => ({ ...current, explicit_node_ids: [...current.explicit_node_ids, node.id], excluded_node_ids: current.excluded_node_ids.filter((id) => id !== node.id) })) }} onProbe={() => void probeOption(node)} />)}
                    {filteredNodes.length === 0 && <EmptyMemberList message={nodeSearch ? '没有匹配的可用节点' : '当前没有其他可用节点'} />}
                  </div>
                </div>
              </div>

              {editing && <div className="mt-4 rounded-2xl border border-base-300 bg-base-200/15 p-4">
                <div><h5 className="text-sm font-bold">当前运行成员</h5><p className="mt-0.5 text-xs text-base-content/50">来自已保存的地区与手动规则；修改将在保存后自动热更新生效</p></div>
                <div className="mt-3 grid max-h-80 gap-2 overflow-y-auto pr-1 md:grid-cols-2">
                  {editing.members.filter(member => !form.excluded_node_ids.includes(member.node_id)).map((member) => <RuntimeMemberRow key={member.node_id} member={member} manual={form.explicit_node_ids.includes(member.node_id)}
                    regional={Boolean(member.region && regionsText.split(/[,，\s]+/).some((region) => region.toLowerCase() === member.region?.toLowerCase()))}
                    latency={latencyOverrides[member.node_id] ?? member.latency_ms} probing={probingNodeIDs.has(member.node_id)} onProbe={() => void probeOption({ id: member.node_id, name: member.name || member.tag, tag: member.tag })} onExclude={() => void removeRuntimeMemberImmediately(member)} onActivate={() => { void run(`activate-${editing.id}-${member.node_id}`, () => activateGroupMember(editing.id, member.node_id), '当前出口已立即切换').then(() => { setEditing((current) => current ? { ...current, members: current.members.map(m => ({ ...m, is_active: m.node_id === member.node_id })) } : null) }) }} isActivateBusy={busy === `activate-${editing.id}-${member.node_id}` || editing.runtime_status !== 'ready'} isRemoveBusy={busy === `remove-member-${editing.id}-${member.node_id}`} />)}
                  {editing.members.filter(member => !form.excluded_node_ids.includes(member.node_id)).length === 0 && <div className="md:col-span-2"><EmptyMemberList message="当前没有运行成员" /></div>}
                </div>
              </div>}

              {editing && <div className="mt-4 rounded-2xl border border-error/20 bg-error/5 p-4">
                <div className="flex items-start justify-between gap-3"><div><h5 className="text-sm font-bold">已排除节点</h5><p className="mt-0.5 text-xs text-base-content/50">排除优先于地区规则；待保存项会在保存后从运行成员中移除</p></div><span className="badge badge-error badge-outline badge-sm">{excludedDraftNodes.length}</span></div>
                {pendingAutoExcludedIDs.size > 0 && <div className="alert alert-warning alert-soft mt-3 py-2 text-xs"><span>检测到 {pendingAutoExcludedIDs.size} 个已踢出节点，已自动加入待排除名单。</span></div>}
                <div className="mt-3 grid max-h-72 gap-2 overflow-y-auto pr-1 md:grid-cols-2">
                  {excludedDraftNodes.map((node) => <ExcludedDraftRow key={node.id} node={node} onUndo={node.pending ? undefined : () => setForm((current) => ({ ...current, excluded_node_ids: current.excluded_node_ids.filter((id) => id !== node.id) }))} />)}
                  {excludedDraftNodes.length === 0 && <div className="md:col-span-2"><EmptyMemberList message="当前没有排除节点" /></div>}
                </div>
              </div>}
            </section>

            <details className="collapse-arrow collapse rounded-2xl border border-base-300 bg-base-200/20"><summary className="collapse-title min-h-0 py-3 text-sm font-semibold">入口认证（可选）</summary><div className="collapse-content grid gap-4 sm:grid-cols-2"><Field label="用户名"><input className={cn('input w-full', controlClass)} value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></Field><Field label="密码"><input type="password" className={cn('input w-full', controlClass)} value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} /></Field></div></details>
          </div>
          <div className="sticky bottom-0 flex justify-end gap-2 border-t border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl"><button className="btn btn-ghost" onClick={closeEditor}>取消</button><button className="btn btn-primary min-w-28" disabled={!form.name.trim() || busy === 'save' || editing?.runtime_status === 'starting' || editing?.runtime_status === 'reconfiguring'} onClick={() => void save()}>{busy === 'save' && <span className="loading loading-spinner loading-sm" />}{editing ? '保存修改' : '创建并加载'}</button></div>
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

function LatencyValue({ value }: { value: number }) {
  if (value < 0) return <span className="font-mono text-[11px] text-base-content/35">未测试</span>
  const tone = value < 200 ? 'text-success' : value < 500 ? 'text-warning' : 'text-error'
  return <span className={cn('whitespace-nowrap font-mono text-[11px] font-semibold tabular-nums', tone)}>{value} ms</span>
}

function EmptyMemberList({ message }: { message: string }) {
  return <div className="rounded-xl border border-dashed border-base-300 px-3 py-8 text-center text-xs text-base-content/45">{message}</div>
}

function NodeOptionRow({ node, latency, probing, action, onAction, onProbe }: {
  node: GroupNodeOption
  latency: number
  probing: boolean
  action: 'add' | 'remove'
  onAction: () => void
  onProbe: () => void
}) {
  const style = nodeStatusStyle[node.status]
  return <div className="flex min-w-0 items-center gap-2 rounded-xl border border-base-200 bg-base-100/75 px-3 py-2.5 transition-colors hover:border-base-300">
    <div className="min-w-0 flex-1"><div className="flex min-w-0 items-center gap-2"><span className="truncate text-sm font-medium" title={node.name}>{node.name}</span>{node.region && <span className="shrink-0 text-[10px] font-bold uppercase text-base-content/45">{node.region}</span>}</div><div className="mt-1 flex items-center gap-2"><span className={cn('badge badge-xs', style.badge)}>{style.label}</span><LatencyValue value={latency} /></div></div>
    <button type="button" className="btn btn-ghost btn-xs btn-square text-primary" onClick={onProbe} disabled={!node.tag || probing} aria-label={`测试 ${node.name} 延迟`} title={node.tag ? '测试延迟' : '节点暂无运行时标识'}>{probing ? <span className="loading loading-spinner loading-xs" /> : <Activity className="h-3.5 w-3.5" />}</button>
    <button type="button" className={cn('btn btn-ghost btn-xs btn-square', action === 'remove' ? 'text-error' : 'text-success')} onClick={onAction} aria-label={`${action === 'remove' ? '移除' : '添加'} ${node.name}`} title={action === 'remove' ? '取消手动指定' : '添加到手动节点'}>{action === 'remove' ? <Trash2 className="h-3.5 w-3.5" /> : <Plus className="h-3.5 w-3.5" />}</button>
  </div>
}

function RuntimeMemberRow({ member, manual, regional, latency, probing, onProbe, onExclude, onActivate, isActivateBusy, isRemoveBusy }: {
  member: GroupMember
  manual: boolean
  regional: boolean
  latency: number
  probing: boolean
  onProbe: () => void
  onExclude: () => void
  onActivate?: () => void
  isActivateBusy?: boolean
  isRemoveBusy?: boolean
}) {
  const style = statusStyle[member.status]
  return <div className="flex min-w-0 items-center gap-2 rounded-xl border border-base-200 bg-base-100/70 px-3 py-2.5">
    <div className="min-w-0 flex-1"><div className="flex min-w-0 items-center gap-2"><span className="truncate text-sm font-medium">{member.name || member.tag}</span>{member.region && <span className="shrink-0 text-[10px] font-bold uppercase text-base-content/45">{member.region}</span>}</div><div className="mt-1 flex flex-wrap items-center gap-1.5"><span className={cn('badge badge-xs', style.badge)}>{style.label}</span>{manual && <span className="badge badge-outline badge-xs">手动</span>}{regional && <span className="badge badge-ghost badge-xs">地区</span>}{member.is_active && <span className="badge badge-success badge-outline badge-xs">当前出口</span>}<LatencyValue value={latency} /></div></div>
    {onActivate && member.status === 'ALIVE' && !member.is_active && (
      <button type="button" className="btn btn-ghost btn-xs btn-square text-primary" onClick={onActivate} disabled={isActivateBusy} title="强制设为当前出口" aria-label={`将 ${member.name || member.tag} 设为当前出口`}>
        <Crosshair className="h-3.5 w-3.5" />
      </button>
    )}
    <button type="button" className="btn btn-ghost btn-xs btn-square text-primary" onClick={onProbe} disabled={!member.tag || probing} aria-label={`测试 ${member.name || member.tag} 延迟`} title="测试延迟">{probing ? <span className="loading loading-spinner loading-xs" /> : <Activity className="h-3.5 w-3.5" />}</button>
    <button type="button" className="btn btn-ghost btn-xs btn-square text-error" onClick={onExclude} disabled={isRemoveBusy} aria-label={`移除节点 ${member.name || member.tag}`} title="立即从此分组移除">{isRemoveBusy ? <span className="loading loading-spinner loading-xs" /> : <Trash2 className="h-3.5 w-3.5" />}</button>
  </div>
}

function ExcludedDraftRow({ node, onUndo }: { node: { id: number; name: string; region: string; status?: GroupNodeOption['status']; pending: boolean }; onUndo?: () => void }) {
  const style = node.status ? nodeStatusStyle[node.status] : nodeStatusStyle.pending
  return <div className="flex min-w-0 items-center gap-2 rounded-xl border border-error/15 bg-base-100/80 px-3 py-2.5">
    <ShieldOff className="h-4 w-4 shrink-0 text-error" />
    <div className="min-w-0 flex-1"><div className="flex min-w-0 items-center gap-2"><span className="truncate text-sm font-medium">{node.name}</span>{node.region && <span className="shrink-0 text-[10px] font-bold uppercase text-base-content/45">{node.region}</span>}</div><div className="mt-1 flex items-center gap-1.5"><span className="badge badge-error badge-outline badge-xs">{node.pending ? '待保存排除' : '已排除'}</span><span className={cn('badge badge-xs', style.badge)}>{style.label}</span></div></div>
    {onUndo && <button type="button" className="btn btn-ghost btn-xs text-primary" onClick={onUndo} title="取消排除，保存后按地区或手动规则重新匹配"><RotateCcw className="h-3.5 w-3.5" />取消排除</button>}
  </div>
}

function GroupCard({ group, nodeOptions, busy, expanded, onToggleExpanded, onEdit, onDelete, onToggle, onResetToken, onRestore, onActivate, onUnexclude, onRemoveMember }: { group: GroupPool; nodeOptions: GroupNodeOption[]; busy: string | null; expanded: boolean; onToggleExpanded: () => void; onEdit: () => void; onDelete: () => void; onToggle: () => void; onResetToken: () => void; onRestore: (nodeId: number) => void; onActivate: (nodeId: number) => void; onUnexclude: (nodeId: number) => void; onRemoveMember: (member: GroupMember) => void }) {
  const active = group.members.find((member) => member.is_active)
	const nodeByID = new Map(nodeOptions.map((node) => [node.id, node]))
	const excludedNodes = (group.excluded_node_ids || []).map((id) => nodeByID.get(id) || { id, name: `节点 #${id}`, region: '', status: 'pending' as const })
	const runtimeStyle = runtimeStatusStyle[group.runtime_status] || runtimeStatusStyle.stopped
	const runtimeUpdating = group.runtime_status === 'starting' || group.runtime_status === 'reconfiguring'
	const runtimeReady = group.runtime_status === 'ready'
	const copySubscription = async (format: 'clash' | 'base64', mode: 'members' | 'entry') => {
		if (!group.subscription_token) { toast.error('当前分组没有可用的订阅 Token'); return }
		const query = new URLSearchParams({ token: group.subscription_token, format, mode })
		const link = `${window.location.origin}/sub/${group.id}?${query.toString()}`
		if (await copyTextWithFallback(link)) {
			toast.success(`已复制 ${format === 'clash' ? 'Clash' : 'Base64'} ${mode === 'entry' ? '入口' : '成员'}订阅`)
		} else {
			window.prompt('浏览器禁止自动复制，请手动复制订阅链接：', link)
			toast.info('已打开手动复制窗口')
		}
	}
  return <article className={cn(surfaceClass, 'overflow-hidden transition-colors', !group.enabled && 'opacity-70')}>
    <div className="border-b border-base-200 p-5">
      <div className="flex items-start gap-3"><div className={cn('flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl', runtimeReady ? 'bg-primary/10 text-primary' : 'bg-base-200 text-base-content/40')}><Layers3 className="h-5 w-5" /></div><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h3 className="truncate text-lg font-bold">{group.name}</h3><span className={cn('badge badge-sm', runtimeStyle.badge)}>{runtimeStyle.label}</span><span className="badge badge-outline badge-sm">{dispatchModeLabel[group.dispatch_mode]}</span></div><div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-base-content/55"><code className="font-semibold text-primary">{group.bind_address}:{group.bind_port}</code><span className="uppercase">{group.protocol}</span><span>{group.failure_window_seconds / 60} 分钟 / {group.failure_threshold} 次踢出</span></div>{group.runtime_error && <p className="mt-1 truncate text-xs text-error" title={group.runtime_error}>{group.runtime_error}</p>}</div><div className="dropdown dropdown-end"><button tabIndex={0} className="btn btn-ghost btn-sm btn-square" aria-label="分组操作" disabled={runtimeUpdating}><Pencil className="h-4 w-4" /></button><ul tabIndex={0} className="dropdown-content z-20 mt-1 w-36 rounded-box border border-base-300 bg-base-100 p-1.5 shadow-xl"><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-base-200" onClick={onEdit} disabled={runtimeUpdating}><Pencil className="h-4 w-4" />编辑</button></li><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-base-200" onClick={onToggle} disabled={runtimeUpdating}><Power className="h-4 w-4" />{group.enabled ? '停用' : '启用'}</button></li><li><button className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-2 text-sm text-error hover:bg-error/10" onClick={onDelete} disabled={runtimeUpdating}><Trash2 className="h-4 w-4" />删除</button></li></ul></div></div>
      <div className="mt-4 grid grid-cols-4 gap-2"><MiniMetric label="成员" value={group.member_count} /><MiniMetric label="健康" value={group.alive_count} tone="text-success" /><MiniMetric label="踢出" value={group.evicted_count} tone={group.evicted_count ? 'text-error' : ''} /><MiniMetric label="排除" value={excludedNodes.length} tone={excludedNodes.length ? 'text-error' : ''} /></div>
      {(group.regions?.length > 0 || group.explicit_node_ids?.length > 0) && <div className="mt-4 flex flex-wrap gap-1.5">{group.regions?.map((region) => <span key={region} className="badge badge-primary badge-outline badge-sm uppercase">{region}</span>)}{group.explicit_node_ids?.length > 0 && <span className="badge badge-ghost badge-sm">手动 {group.explicit_node_ids.length} 个</span>}</div>}
		<div className={cn('mt-4 rounded-xl border p-3', group.subscription_enabled ? 'border-info/20 bg-info/5' : 'border-base-200 bg-base-200/25')}>
			<div className="flex items-center gap-2"><Link2 className={cn('h-4 w-4', group.subscription_enabled ? 'text-info' : 'text-base-content/35')} /><span className="text-xs font-bold">订阅输出</span><span className={cn('badge badge-xs', group.subscription_enabled ? 'badge-info' : 'badge-ghost')}>{group.subscription_enabled ? '已启用' : '已关闭'}</span>{group.subscription_enabled && <span className="ml-auto flex items-center gap-1 font-mono text-[10px] text-base-content/40"><KeyRound className="h-3 w-3" />••••{group.subscription_token?.slice(-6)}</span>}</div>
			{group.subscription_enabled && <div className="mt-3 grid grid-cols-3 gap-1.5"><button className="btn btn-ghost btn-xs h-8 min-h-0 gap-1 border border-base-300 bg-base-100" onClick={() => void copySubscription('clash', 'entry')}><Copy className="h-3 w-3" />入口</button><button className="btn btn-ghost btn-xs h-8 min-h-0 gap-1 border border-base-300 bg-base-100" onClick={() => void copySubscription('clash', 'members')}><Copy className="h-3 w-3" />成员</button><button className="btn btn-ghost btn-xs h-8 min-h-0 gap-1 border border-base-300 bg-base-100" onClick={() => void copySubscription('base64', 'members')}><Copy className="h-3 w-3" />Base64</button></div>}
			{group.subscription_enabled && <button className="mt-2 flex cursor-pointer items-center gap-1 text-[11px] text-base-content/45 transition-colors hover:text-error" disabled={busy === `token-${group.id}`} onClick={onResetToken}><RotateCcw className={cn('h-3 w-3', busy === `token-${group.id}` && 'animate-spin')} />重置 Token（旧链接立即失效）</button>}
		</div>
    </div>
    <div className="p-4 sm:p-5">
      {group.dispatch_mode !== 'random' && <div className={cn('mb-3 flex items-center gap-3 rounded-xl border px-3 py-2.5', active ? 'border-success/25 bg-success/5' : 'border-warning/25 bg-warning/5')}><Activity className={cn('h-4 w-4 shrink-0', active ? 'text-success' : 'text-warning')} /><div className="min-w-0 flex-1"><p className="text-[11px] font-semibold uppercase tracking-wider text-base-content/45">当前主出口</p><p className="truncate text-sm font-semibold">{active?.name || '等待首个连接选择'}</p></div>{active?.latency_ms && active.latency_ms > 0 ? <span className="text-xs font-mono text-base-content/55">{active.latency_ms} ms</span> : null}</div>}
      <div className="space-y-2">{(expanded ? group.members : group.members.slice(0, 8)).map((member) => { const style = statusStyle[member.status]; const StatusIcon = style.icon; return <div key={member.node_id} className="flex min-w-0 items-center gap-2 rounded-xl border border-base-200 bg-base-200/20 px-3 py-2.5"><StatusIcon className={cn('h-4 w-4 shrink-0', member.status === 'ALIVE' ? 'text-success' : member.status === 'SUSPECT' ? 'text-warning' : 'text-error')} /><div className="min-w-0 flex-1"><div className="flex items-center gap-2"><span className="truncate text-sm font-medium">{member.name || member.tag}</span>{member.region && <span className="text-[10px] font-bold uppercase text-base-content/40">{member.region}</span>}</div>{member.last_error && <p className="mt-0.5 truncate text-[11px] text-error/75" title={member.last_error}>{member.last_error}</p>}</div><span className={cn('badge badge-sm', style.badge)}>{style.label}</span>{member.latency_ms > 0 && <span className="hidden w-14 text-right font-mono text-xs text-base-content/45 sm:block">{member.latency_ms} ms</span>}<div className="flex shrink-0 items-center gap-1">{member.status === 'ALIVE' && !member.is_active && <button className="btn btn-ghost btn-xs btn-square text-primary" disabled={!runtimeReady || busy === `activate-${group.id}-${member.node_id}`} onClick={() => onActivate(member.node_id)} title={group.dispatch_mode === 'random' ? '立即切换；random 模式下后续连接仍会随机选择' : '强制设为当前出口'} aria-label={`将 ${member.name || member.tag} 设为当前出口`}><Crosshair className="h-3.5 w-3.5" /></button>}{member.status === 'EVICTED' && <button className="btn btn-ghost btn-xs gap-1 text-primary" disabled={!runtimeReady || busy === `restore-${group.id}-${member.node_id}`} onClick={() => onRestore(member.node_id)} title="恢复入池"><RotateCcw className="h-3 w-3" /><span className="hidden sm:inline">恢复</span></button>}<button className="btn btn-ghost btn-xs btn-square text-error" disabled={!runtimeReady || busy === `remove-member-${group.id}-${member.node_id}`} onClick={() => onRemoveMember(member)} title="从此分组移除" aria-label={`从分组移除 ${member.name || member.tag}`}><Trash2 className="h-3.5 w-3.5" /></button></div></div> })}</div>
      {group.member_count === 0 && <div className="rounded-xl border border-dashed border-warning/40 bg-warning/5 px-4 py-8 text-center"><CircleAlert className="mx-auto h-6 w-6 text-warning" /><p className="mt-2 text-sm font-medium">当前没有匹配的有效节点</p><p className="mt-1 text-xs text-base-content/45">检查地区码、手动成员或节点启用状态</p></div>}
      {excludedNodes.length > 0 && <div className="mt-4 rounded-xl border border-error/15 bg-error/5 p-3"><div className="mb-2 flex items-center justify-between"><div className="flex items-center gap-2"><ShieldOff className="h-4 w-4 text-error" /><span className="text-xs font-bold">已排除节点</span></div><span className="badge badge-error badge-outline badge-xs">{excludedNodes.length}</span></div><div className="space-y-1.5">{excludedNodes.map((node) => <div key={node.id} className="flex min-w-0 items-center gap-2 rounded-lg bg-base-100/75 px-2.5 py-2"><div className="min-w-0 flex-1"><div className="flex min-w-0 items-center gap-2"><span className="truncate text-xs font-medium">{node.name}</span>{node.region && <span className="text-[10px] font-bold uppercase text-base-content/40">{node.region}</span>}</div></div><button className="btn btn-ghost btn-xs text-primary" disabled={busy === `unexclude-${group.id}-${node.id}`} onClick={() => onUnexclude(node.id)} title="仅取消排除；原健康踢出状态不会清除">{busy === `unexclude-${group.id}-${node.id}` ? <span className="loading loading-spinner loading-xs" /> : <RotateCcw className="h-3.5 w-3.5" />}取消排除</button></div>)}</div><p className="mt-2 text-[10px] leading-4 text-base-content/45">取消排除只恢复成员规则；如节点仍显示已踢出，请再执行“恢复”。</p></div>}
      {group.member_count > 8 && <button type="button" className="mt-3 flex min-h-9 w-full cursor-pointer items-center justify-center gap-1.5 rounded-lg text-xs font-medium text-base-content/55 transition-colors hover:bg-base-200/60 hover:text-primary focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary" onClick={onToggleExpanded} aria-expanded={expanded}>
        <ChevronDown className={cn('h-3.5 w-3.5 transition-transform duration-200', expanded && 'rotate-180')} />{expanded ? '收起成员' : `展开其余 ${group.member_count - 8} 个成员`}
      </button>}
    </div>
  </article>
}

function MiniMetric({ label, value, tone = '' }: { label: string; value: number; tone?: string }) {
  return <div className="rounded-xl bg-base-200/55 px-3 py-2 text-center"><div className={cn('text-lg font-black tabular-nums', tone)}>{value}</div><div className="text-[11px] text-base-content/45">{label}</div></div>
}
