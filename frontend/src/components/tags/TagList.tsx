import { useMemo, useState } from 'react'
import { ArrowDown, ArrowUp, ArrowUpDown, Pencil, Search, Trash2 } from 'lucide-react'
import type { Tag, TagMutexGroup } from '../../types'
import { cn } from '../../utils/cn'
import { controlClass, surfaceClass } from '../ui/PageLayout'
import TagBadge from './TagBadge'

type SortKey = 'name' | 'priority' | 'nodes' | 'auto'
type SortDirection = 'asc' | 'desc'

interface TagListProps {
  tags: Tag[]
  mutexGroups: TagMutexGroup[]
  busy: string | null
  onEdit: (tag: Tag) => void
  onDelete: (tag: Tag) => void
  onToggleAuto: (tag: Tag) => void
  onBatchAuto: (ids: number[], enabled: boolean) => void
}

export default function TagList({ tags, mutexGroups, busy, onEdit, onDelete, onToggleAuto, onBatchAuto }: TagListProps) {
  const [search, setSearch] = useState('')
  const [sortKey, setSortKey] = useState<SortKey>('priority')
  const [sortDirection, setSortDirection] = useState<SortDirection>('desc')
  const [selected, setSelected] = useState<Set<number>>(() => new Set())
  const groupByID = useMemo(() => new Map(mutexGroups.map((group) => [group.id, group])), [mutexGroups])
  const visible = useMemo(() => {
    const keyword = search.trim().toLowerCase()
    const rows = tags.filter((tag) => !keyword || `${tag.name} ${tag.description} ${tag.builtin_key}`.toLowerCase().includes(keyword))
    return [...rows].sort((a, b) => {
      let result = 0
      if (sortKey === 'name') result = a.name.localeCompare(b.name)
      if (sortKey === 'priority') result = a.priority - b.priority
      if (sortKey === 'nodes') result = a.node_count - b.node_count
      if (sortKey === 'auto') result = Number(a.auto_enabled) - Number(b.auto_enabled)
      return sortDirection === 'asc' ? result : -result
    })
  }, [search, sortDirection, sortKey, tags])
  const selectedIDs = [...selected]
  const automatableSelectedIDs = selectedIDs.filter((id) => tags.some((tag) => tag.id === id && Boolean(tag.rule)))
  const allVisibleSelected = visible.length > 0 && visible.every((tag) => selected.has(tag.id))
  const sort = (key: SortKey) => { if (sortKey === key) setSortDirection((current) => current === 'asc' ? 'desc' : 'asc'); else { setSortKey(key); setSortDirection('asc') } }

  return (
    <section className={cn(surfaceClass, 'overflow-hidden')}>
      <div className="flex flex-col gap-3 border-b border-base-200 p-4 sm:flex-row sm:items-center sm:justify-between"><label className={cn('input input-sm flex w-full items-center gap-2 sm:max-w-sm', controlClass)}><Search className="h-3.5 w-3.5 text-base-content/40" /><input className="min-w-0 grow" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索名称、说明或内置键" aria-label="搜索标签" /></label><div className="text-xs text-base-content/45">显示 {visible.length} / {tags.length} 个标签</div></div>
      {selected.size > 0 && <div className="flex flex-wrap items-center gap-2 border-b border-primary/15 bg-primary/5 px-4 py-2.5"><span className="text-xs font-semibold text-primary">已选择 {selected.size} 个</span><button className="btn btn-outline btn-xs" disabled={automatableSelectedIDs.length === 0 || busy === 'batch-auto'} onClick={() => onBatchAuto(automatableSelectedIDs, true)} title="没有规则的人工标签会保持关闭">启用自动</button><button className="btn btn-ghost btn-xs" disabled={busy === 'batch-auto'} onClick={() => onBatchAuto(selectedIDs, false)}>关闭自动</button><button className="btn btn-ghost btn-xs ml-auto" onClick={() => setSelected(new Set())}>清除选择</button></div>}
      <div className="overflow-x-auto"><table className="table table-zebra"><thead><tr><th className="w-10"><input type="checkbox" className="checkbox checkbox-sm" checked={allVisibleSelected} onChange={() => setSelected((current) => { const next = new Set(current); if (allVisibleSelected) visible.forEach((tag) => next.delete(tag.id)); else visible.forEach((tag) => next.add(tag.id)); return next })} aria-label="选择全部可见标签" /></th><SortableHeader label="名称" active={sortKey === 'name'} direction={sortDirection} onClick={() => sort('name')} /><th>互斥组</th><SortableHeader label="优先级" active={sortKey === 'priority'} direction={sortDirection} onClick={() => sort('priority')} /><SortableHeader label="自动" active={sortKey === 'auto'} direction={sortDirection} onClick={() => sort('auto')} /><SortableHeader label="节点数" active={sortKey === 'nodes'} direction={sortDirection} onClick={() => sort('nodes')} /><th>分组引用</th><th className="text-right">操作</th></tr></thead><tbody>{visible.map((tag) => <tr key={tag.id}><td><input type="checkbox" className="checkbox checkbox-sm" checked={selected.has(tag.id)} onChange={() => setSelected((current) => { const next = new Set(current); if (next.has(tag.id)) next.delete(tag.id); else next.add(tag.id); return next })} aria-label={`选择 ${tag.name}`} /></td><td><div className="flex items-center gap-2"><TagBadge tag={tag} /><div className="min-w-0"><div className="max-w-64 truncate text-xs text-base-content/50" title={tag.description}>{tag.description || '暂无说明'}</div>{tag.builtin_key && <code className="text-[10px] text-base-content/35">{tag.builtin_key}</code>}</div></div></td><td>{tag.mutex_group_id ? <span className="badge badge-outline badge-sm">{groupByID.get(tag.mutex_group_id)?.name || `#${tag.mutex_group_id}`}</span> : <span className="text-xs text-base-content/30">—</span>}</td><td className="font-mono font-semibold">{tag.priority}</td><td><input type="checkbox" className="toggle toggle-primary toggle-sm" checked={tag.auto_enabled} disabled={busy === `auto-${tag.id}` || (!tag.rule && !tag.auto_enabled)} onChange={() => onToggleAuto(tag)} aria-label={`${tag.auto_enabled ? '关闭' : '启用'} ${tag.name} 自动打标`} /></td><td><div className="flex gap-1.5"><span className="badge badge-ghost badge-sm">总 {tag.node_count}</span><span className="badge badge-outline badge-sm">手 {tag.manual_count}</span><span className="badge badge-outline badge-sm">自 {tag.auto_count}</span></div></td><td>{tag.used_by_groups?.length ? <span className="badge badge-warning badge-outline badge-sm">{tag.used_by_groups.length} 个分组</span> : <span className="text-xs text-base-content/30">未引用</span>}</td><td><div className="flex justify-end gap-1"><button className="btn btn-ghost btn-sm btn-square text-info" onClick={() => onEdit(tag)} aria-label={`编辑 ${tag.name}`}><Pencil className="h-4 w-4" /></button><button className="btn btn-ghost btn-sm btn-square text-error" disabled={busy === `delete-${tag.id}`} onClick={() => onDelete(tag)} aria-label={`删除 ${tag.name}`}>{busy === `delete-${tag.id}` ? <span className="loading loading-spinner loading-xs" /> : <Trash2 className="h-4 w-4" />}</button></div></td></tr>)}</tbody></table></div>
      {visible.length === 0 && <div className="flex min-h-60 flex-col items-center justify-center text-center"><Search className="h-7 w-7 text-base-content/20" /><p className="mt-2 text-sm font-medium text-base-content/50">没有匹配的标签</p></div>}
    </section>
  )
}

function SortableHeader({ label, active, direction, onClick }: { label: string; active: boolean; direction: SortDirection; onClick: () => void }) {
  const Icon = !active ? ArrowUpDown : direction === 'asc' ? ArrowUp : ArrowDown
  return <th><button type="button" className="flex cursor-pointer items-center gap-1 transition-colors hover:text-primary" onClick={onClick}>{label}<Icon className={cn('h-3 w-3', !active && 'opacity-30')} /></button></th>
}
