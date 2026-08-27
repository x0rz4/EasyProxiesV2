import { useMemo, useState } from 'react'
import { Check, Search, X } from 'lucide-react'
import type { Tag } from '../../types'
import { cn } from '../../utils/cn'
import { controlClass } from '../ui/PageLayout'
import TagBadge from './TagBadge'

interface TagMultiSelectProps {
  tags: Tag[]
  value: number[]
  onChange: (value: number[]) => void
  placeholder?: string
  disabledIds?: number[]
  compact?: boolean
}

export default function TagMultiSelect({
  tags,
  value,
  onChange,
  placeholder = '搜索并选择标签',
  disabledIds = [],
  compact = false,
}: TagMultiSelectProps) {
  const [search, setSearch] = useState('')
  const disabled = useMemo(() => new Set(disabledIds), [disabledIds])
  const selected = useMemo(() => new Set(value), [value])
  const visible = useMemo(() => {
    const keyword = search.trim().toLowerCase()
    return tags.filter((tag) => !keyword || `${tag.name} ${tag.description}`.toLowerCase().includes(keyword))
  }, [search, tags])

  const toggle = (tagId: number) => {
    if (disabled.has(tagId) && !selected.has(tagId)) return
    onChange(selected.has(tagId) ? value.filter((id) => id !== tagId) : [...value, tagId])
  }

  return (
    <div className={cn('rounded-xl border border-base-300 bg-base-100/75', compact ? 'p-2.5' : 'p-3')}>
      <div className="flex min-h-7 flex-wrap items-center gap-1.5">
        {value.map((tagId) => {
          const tag = tags.find((candidate) => candidate.id === tagId)
          if (!tag) return <span key={tagId} className="badge badge-ghost badge-sm">#{tagId}</span>
          return (
            <span key={tag.id} className="inline-flex items-center gap-0.5">
              <TagBadge tag={tag} compact />
              <button type="button" className="btn btn-ghost btn-xs btn-circle h-5 min-h-0 w-5" onClick={() => toggle(tag.id)} aria-label={`移除标签 ${tag.name}`}>
                <X className="h-3 w-3" />
              </button>
            </span>
          )
        })}
        {value.length === 0 && <span className="text-xs text-base-content/40">尚未选择</span>}
      </div>
      <label className={cn('input input-sm mt-2 flex w-full items-center gap-2', controlClass)}>
        <Search className="h-3.5 w-3.5 text-base-content/40" />
        <input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={placeholder} aria-label={placeholder} className="min-w-0 grow" />
      </label>
      <div className={cn('mt-2 grid gap-1 overflow-y-auto pr-1', compact ? 'max-h-32' : 'max-h-44')}>
        {visible.map((tag) => {
          const checked = selected.has(tag.id)
          const unavailable = disabled.has(tag.id)
          return (
            <button
              key={tag.id}
              type="button"
              className={cn(
                'flex min-h-9 cursor-pointer items-center gap-2 rounded-lg px-2.5 text-left transition-colors hover:bg-base-200 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary',
                checked && 'bg-primary/8', unavailable && 'cursor-not-allowed opacity-40',
              )}
              onClick={() => toggle(tag.id)}
              disabled={unavailable}
              aria-pressed={checked}
            >
              <span className={cn('flex h-4 w-4 shrink-0 items-center justify-center rounded border', checked ? 'border-primary bg-primary text-primary-content' : 'border-base-300')}>
                {checked && <Check className="h-3 w-3" />}
              </span>
              <TagBadge tag={tag} compact />
              <span className="ml-auto truncate text-[10px] text-base-content/40">{tag.description}</span>
            </button>
          )
        })}
        {visible.length === 0 && <div className="py-5 text-center text-xs text-base-content/40">没有匹配标签</div>}
      </div>
    </div>
  )
}
