import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Check, Search, X } from 'lucide-react'
import { deleteNodeFilter, saveNodeFilter } from '../api/client'
import type { useNodeFilter } from '../hooks/useNodeFilter'
import type { NodeFacet } from '../types'
import { cn } from '../utils/cn'

export interface MultiSelectOption {
  value: string
  label: string
  count?: number
}

interface MultiSelectProps {
  label: string
  value: string[]
  onChange: (value: string[]) => void
  options: MultiSelectOption[]
  allowCustom?: boolean
  searchHint?: string
  width?: string
}

/**
 * 可搜索的多选面板。
 * 原生 `<select multiple>` 在 DaisyUI 下会被 `.select{display:inline-flex}` 影响，
 * 选项被当作 flex 项逐字竖排，因此这里用 chips + 复选列表替代。
 */
export function MultiSelect({
  label, value, onChange, options, allowCustom = false, searchHint, width = 'w-52',
}: MultiSelectProps) {
  const [keyword, setKeyword] = useState('')
  const merged = useMemo(() => {
    const map = new Map(options.map(item => [item.value, item]))
    for (const item of value) if (!map.has(item)) map.set(item, { value: item, label: item })
    return [...map.values()]
  }, [options, value])
  const selected = useMemo(() => new Set(value), [value])
  const selectedOptions = merged.filter(item => selected.has(item.value))
  const visible = useMemo(() => {
    const query = keyword.trim().toLowerCase()
    if (!query) return merged
    return merged.filter(item => item.label.toLowerCase().includes(query) || item.value.toLowerCase().includes(query))
  }, [keyword, merged])

  const toggle = (target: string) => {
    onChange(selected.has(target) ? value.filter(item => item !== target) : [...value, target])
  }
  const commitKeyword = () => {
    const query = keyword.trim()
    if (!query) return
    const hit = visible[0]
    if (hit) toggle(hit.value)
    else if (allowCustom) onChange([...new Set([...value, query])])
    setKeyword('')
  }

  return (
    <div className={cn('flex flex-col gap-1 text-xs', width)}>
      <span className="font-medium text-base-content/70">{label}</span>
      <div className="flex flex-col overflow-hidden rounded-xl border border-base-300 bg-base-100 transition-colors focus-within:border-primary/50">
        <div className="flex max-h-14 min-h-7 flex-wrap content-start items-center gap-1 overflow-y-auto px-1.5 py-1.5">
          {selectedOptions.length === 0
            ? <span className="text-[11px] text-base-content/35">未选择</span>
            : selectedOptions.map(item => (
              <span key={item.value} className="inline-flex max-w-full items-center gap-0.5 rounded-md bg-primary/10 py-0.5 pl-1.5 pr-0.5 text-[11px] font-medium text-primary">
                <span className="truncate">{item.label}</span>
                <button type="button" className="rounded p-0.5 transition-colors hover:bg-primary/20" onClick={() => toggle(item.value)} aria-label={`移除 ${item.label}`}>
                  <X className="h-3 w-3" />
                </button>
              </span>
            ))}
        </div>
        <label className="flex items-center gap-1.5 border-t border-base-200 px-2 py-1">
          <Search className="h-3.5 w-3.5 shrink-0 text-base-content/35" />
          <input
            className="min-w-0 grow bg-transparent text-xs outline-none placeholder:text-base-content/35"
            value={keyword}
            onChange={event => setKeyword(event.target.value)}
            onKeyDown={event => {
              if (event.key === 'Enter') {
                event.preventDefault()
                commitKeyword()
              }
            }}
            placeholder={searchHint || (allowCustom ? '搜索，回车添加' : '搜索')}
            aria-label={`搜索${label}`}
          />
        </label>
        <div className="max-h-32 overflow-y-auto border-t border-base-200 p-1">
          {visible.map(item => {
            const checked = selected.has(item.value)
            return (
              <button
                key={item.value}
                type="button"
                onClick={() => toggle(item.value)}
                aria-pressed={checked}
                className={cn('flex w-full items-center gap-2 rounded-lg px-1.5 py-1 text-left text-xs transition-colors hover:bg-base-200', checked && 'bg-primary/10')}
              >
                <span className={cn('flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded border', checked ? 'border-primary bg-primary text-primary-content' : 'border-base-300')}>
                  {checked && <Check className="h-2.5 w-2.5" strokeWidth={3} />}
                </span>
                <span className="min-w-0 flex-1 truncate">{item.label}</span>
                {item.count !== undefined && <span className="shrink-0 text-[10px] tabular-nums text-base-content/40">{item.count}</span>}
              </button>
            )
          })}
          {visible.length === 0 && <div className="px-2 py-3 text-center text-[11px] text-base-content/40">无匹配项</div>}
        </div>
      </div>
    </div>
  )
}

export function RegionSelect({ value, onChange, facets = [], label = '地区' }: {
  value: string[]; onChange: (value: string[]) => void; facets?: NodeFacet[]; label?: string
}) {
  const options = useMemo(() => {
    const map = new Map(facets.filter(item => item.value !== 'unknown').map(item => [item.value, item.label]))
    for (const code of ['hk', 'tw', 'jp', 'kr', 'us', 'gb', 'cn', 'sg', ...value]) {
      if (!map.has(code)) map.set(code, code.toUpperCase())
    }
    map.set('other', '其他已知地区')
    return [...map].map(([itemValue, itemLabel]) => ({ value: itemValue, label: itemLabel }))
  }, [facets, value])

  return <MultiSelect label={label} value={value} onChange={onChange} options={options}
    allowCustom searchHint="搜索，回车添加地区码" />
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
  const facetOptions = (key: 'statuses' | 'sources' | 'protocols' | 'tags') => (facets.data?.[key] || []).map(item => ({ value: item.value, label: item.label, count: item.count }))
  return <section className="panel-card space-y-3 p-4" aria-label="节点筛选">
    <div className="flex flex-wrap items-start gap-3">
      <label className="flex w-40 flex-col gap-1 text-xs">
        <span className="font-medium text-base-content/70">节点名称</span>
        <input className="input input-sm" value={spec.text || ''} onChange={e => setSpec({ ...spec, text: e.target.value })} placeholder="搜索名称" />
      </label>
      <RegionSelect value={spec.regions || []} facets={facets.data?.regions} onChange={regions => setSpec({ ...spec, regions })} />
      <RegionSelect label="排除地区" value={spec.exclude_regions || []} facets={facets.data?.regions} onChange={exclude_regions => setSpec({ ...spec, exclude_regions })} />
      <MultiSelect label="状态" value={spec.status || []} options={facetOptions('statuses')} onChange={status => setSpec({ ...spec, status })} />
      <MultiSelect label="来源" value={(spec.sources || [])} options={facetOptions('sources')} onChange={sources => setSpec({ ...spec, sources })} />
      <MultiSelect label="协议" value={(spec.protocols || [])} options={facetOptions('protocols')} onChange={protocols => setSpec({ ...spec, protocols })} />
      <MultiSelect label="标签" value={(spec.tag_whitelist || []).map(String)} options={facetOptions('tags')} onChange={values => setSpec({ ...spec, tag_whitelist: values.map(Number) })} />
      <MultiSelect label="排除标签" value={(spec.tag_blacklist || []).map(String)} options={facetOptions('tags')} onChange={values => setSpec({ ...spec, tag_blacklist: values.map(Number) })} />
      <label className="flex w-32 flex-col gap-1 text-xs">
        <span className="font-medium text-base-content/70">最大延迟（ms）</span>
        <input className="input input-sm" type="number" min={0} value={spec.max_latency_ms ?? ''} onChange={event => setSpec({ ...spec, max_latency_ms: event.target.value === '' ? undefined : Number(event.target.value) })} />
      </label>
      <label className="flex w-28 flex-col gap-1 self-start text-xs">
        <span className="font-medium text-base-content/70">标签匹配</span>
        <select className="select select-sm" value={spec.tag_match || 'any'} onChange={event => setSpec({ ...spec, tag_match: event.target.value as 'any' | 'all' })}><option value="any">任一标签</option><option value="all">全部标签</option></select>
      </label>
      <label className="flex w-44 flex-col gap-1 text-xs">
        <span className="font-medium text-base-content/70">诊断筛选</span>
        <select className="select select-sm" value={spec.diagnostic || ''} onChange={event => setSpec({ ...spec, diagnostic: event.target.value })}><option value="">全部诊断结果</option>{facets.data?.diagnostics.map(item => <option key={item.value} value={item.value}>{item.label} ({item.count})</option>)}</select>
      </label>
      <label className="flex cursor-pointer items-center gap-2 self-end rounded-lg border border-base-300 px-3 py-1.5 text-xs">
        <input type="checkbox" className="checkbox checkbox-sm" checked={!!spec.include_unknown_region} onChange={e => setSpec({ ...spec, include_unknown_region: e.target.checked })} />包含未知地区
      </label>
      <button className="btn btn-sm self-end" onClick={() => { setSpec({ version: 2 }); setSelected(0) }}>清除筛选</button>
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
      <span>{query.isFetching ? '查询中…' : `共 ${query.data?.total || 0} 个节点`} · 全选作用于当前页；已选节点跨页保留</span>
      <button className="btn btn-xs" disabled={!page.offset || query.isFetching} onClick={() => setPage({ ...page, offset: Math.max(0, page.offset - page.limit) })}>上一页</button>
      <span>第 {Math.floor(page.offset / page.limit) + 1} 页</span>
      <button className="btn btn-xs" disabled={query.isFetching || !query.data || page.offset + page.limit >= query.data.total} onClick={() => setPage({ ...page, offset: page.offset + page.limit })}>下一页</button>
    </div>
  </section>
}
