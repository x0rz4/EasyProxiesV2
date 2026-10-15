import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchGroupHealEvents } from '../api/client'
import type { GroupHealEvent, GroupPool } from '../types'

const statusLabels: Record<GroupHealEvent['status'], string> = {
  dry_run: '模拟', planned: '准备执行', succeeded: '已完成', failed: '失败', deferred: '暂缓', alert: '容量不足',
}
const actionLabels: Record<string, string> = {
  promote_standby: '调整活动成员', probe_quarantined: '复检剔除节点', refresh_subscription: '刷新订阅', capacity_alert: '容量告警',
}

export default function GroupHealLog({ group }: { group: GroupPool }) {
  const [open, setOpen] = useState(false)
  const events = useQuery({
    queryKey: ['groupHealEvents', group.id],
    queryFn: ({ signal }) => fetchGroupHealEvents(group.id, signal),
    enabled: open,
    refetchInterval: open ? 10_000 : false,
  })
  return <details className="mt-3 rounded-xl border border-base-300 p-3 text-xs" onToggle={e => setOpen(e.currentTarget.open)}>
    <summary className="cursor-pointer font-medium">自愈记录 · {group.target_size ? `最低健康 ${group.min_healthy} / 容量 ${group.target_size} · ${group.healing_enabled ? '执行' : 'dry-run'}` : '容量管理关闭'}</summary>
    {events.error && <p className="mt-2 text-error">读取失败：{events.error.message}</p>}
    {events.isLoading && <p className="mt-2">正在读取…</p>}
    {events.data?.events.length === 0 && <p className="mt-2 text-base-content/50">暂无动作记录</p>}
    <ul className="mt-2 max-h-64 space-y-2 overflow-auto">{events.data?.events.map(event => <li key={event.id} className="border-t border-base-200 pt-2">
      <div>{new Date(event.created_at).toLocaleString()} · {statusLabels[event.status]} · {actionLabels[event.action.kind] ?? event.action.kind}</div>
      <p className="mt-1 text-base-content/60">{event.action.reason}</p>
      {!!event.action.node_ids?.length && <p>节点：{event.action.node_ids.join(', ')}</p>}
      {!!event.action.removed_ids?.length && <p>转为备用：{event.action.removed_ids.join(', ')}</p>}
      {!!event.action.subscription_ids?.length && <p>订阅：{event.action.subscription_ids.join(', ')}</p>}
    </li>)}</ul>
  </details>
}
