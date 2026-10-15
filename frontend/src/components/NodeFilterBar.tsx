import { useState } from 'react'
import { toast } from 'sonner'
import { deleteNodeFilter, saveNodeFilter } from '../api/client'
import type { useNodeFilter } from '../hooks/useNodeFilter'
import type { NodeFacet } from '../types'

export function RegionSelect({ value, onChange, facets = [], label = '地区' }: {
  value: string[]; onChange: (value: string[]) => void; facets?: NodeFacet[]; label?: string
}) {
  const options = new Map(facets.filter(item => item.value !== 'unknown').map(item => [item.value, item.label]))
  for (const code of ['hk', 'tw', 'jp', 'kr', 'us', 'gb', 'cn', 'sg', ...value]) {
    if (!options.has(code)) options.set(code, code.toUpperCase())
  }
  options.set('other', '其他已知地区')
  return <label className="flex flex-col gap-1 text-xs">{label}
    <select multiple aria-label={label} className="select h-28 min-w-40" value={value}
      onChange={event => onChange(Array.from(event.target.selectedOptions, item => item.value))}>
      {[...options].map(([code, name]) => <option key={code} value={code}>{name}</option>)}
    </select>
  </label>
}

export default function NodeFilterBar({ filter }: { filter: ReturnType<typeof useNodeFilter> }) {
  const { spec, setSpec, page, setPage, facets, query, saved } = filter
  const [name, setName] = useState('')
  const [selected, setSelected] = useState<number>(0)
  const [busy, setBusy] = useState(false)
  const previous = saved.data?.filters.find(item => item.id === selected)
  const persist = async (remove = false) => {
    setBusy(true)
    try {
      if (remove && previous) {
        await deleteNodeFilter(previous.id)
        setSelected(0)
      } else {
        const result = await saveNodeFilter(name.trim(), spec, previous)
        setSelected(result.id)
      }
      await saved.refetch()
    } catch (error) { toast.error(error instanceof Error ? error.message : '保存筛选失败') }
    finally { setBusy(false) }
  }
  return <section className="panel-card space-y-3 p-4" aria-label="节点筛选">
    <div className="flex flex-wrap items-end gap-3">
      <label className="flex flex-col gap-1 text-xs">节点名称<input className="input" value={spec.text || ''} onChange={e => setSpec({ ...spec, text: e.target.value })} placeholder="搜索名称" /></label>
      <RegionSelect value={spec.regions || []} facets={facets.data?.regions} onChange={regions => setSpec({ ...spec, regions })} />
      <label className="flex items-center gap-2 text-xs"><input type="checkbox" className="checkbox checkbox-sm" checked={!!spec.include_unknown_region} onChange={e => setSpec({ ...spec, include_unknown_region: e.target.checked })} />包含未知地区</label>
      {(['statuses', 'sources', 'protocols', 'tags'] as const).map(key => {
        const field = { statuses: 'status', sources: 'sources', protocols: 'protocols', tags: 'tag_whitelist' }[key] as 'status' | 'sources' | 'protocols' | 'tag_whitelist'
        return <label key={key} className="flex flex-col gap-1 text-xs">{{ statuses: '状态', sources: '来源', protocols: '协议', tags: '标签' }[key]}
          <select multiple className="select h-28" value={(spec[field] || []).map(String)} onChange={event => {
            const values = Array.from(event.target.selectedOptions, item => item.value)
            setSpec({ ...spec, [field]: field === 'tag_whitelist' ? values.map(Number) : values })
          }}>
            {(facets.data?.[key] || []).map(item => <option key={item.value} value={item.value}>{item.label} ({item.count})</option>)}
          </select>
        </label>
      })}
      <button className="btn btn-sm" onClick={() => { setSpec({ version: 2 }); setSelected(0) }}>清除筛选</button>
    </div>
    <div className="flex flex-wrap items-center gap-2">
      <select aria-label="已保存筛选" className="select select-sm" value={selected} onChange={event => {
        const item = saved.data?.filters.find(value => value.id === Number(event.target.value))
        setSelected(item?.id || 0)
        if (item) { setSpec(item.spec); setName(item.name) }
      }}><option value={0}>新筛选</option>{saved.data?.filters.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
      <input aria-label="筛选名称" className="input input-sm" value={name} onChange={event => setName(event.target.value)} placeholder="筛选名称" maxLength={100} />
      <button className="btn btn-sm" disabled={busy || !name.trim()} onClick={() => void persist()}>{previous ? '更新筛选' : '保存筛选'}</button>
      {previous && <button className="btn btn-sm btn-ghost" disabled={busy} onClick={() => void persist(true)}>删除筛选</button>}
    </div>
    {(query.error || facets.error) && <p role="alert" className="text-sm text-error">{(query.error || facets.error)?.message}</p>}
    <div className="flex items-center gap-3 text-xs">
      <span>{query.isFetching ? '查询中…' : `共 ${query.data?.total || 0} 个节点`} · 选择操作针对当前页</span>
      <button className="btn btn-xs" disabled={!page.offset || query.isFetching} onClick={() => setPage({ ...page, offset: Math.max(0, page.offset - page.limit) })}>上一页</button>
      <span>第 {Math.floor(page.offset / page.limit) + 1} 页</span>
      <button className="btn btn-xs" disabled={query.isFetching || !query.data || page.offset + page.limit >= query.data.total} onClick={() => setPage({ ...page, offset: page.offset + page.limit })}>下一页</button>
    </div>
  </section>
}
