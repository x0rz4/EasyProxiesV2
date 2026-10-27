import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Tags as TagsIcon, X } from 'lucide-react'
import { toast } from 'sonner'
import { fetchTagAssignments, fetchTags, setNodeManualTags } from '../../api/client'
import { cn } from '../../utils/cn'
import TagBadge from './TagBadge'

interface NodeTagPickerProps {
  nodeId?: number
  currentTagNames?: string[]
}

export default function NodeTagPicker({ nodeId, currentTagNames = [] }: NodeTagPickerProps) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<number[] | null>(null)
  const [saving, setSaving] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const outside = (event: PointerEvent) => { if (!root.current?.contains(event.target as Node)) { setOpen(false); setDraft(null) } }
    const escape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.stopPropagation(); setOpen(false); setDraft(null); root.current?.querySelector('button')?.focus() }
    }
    document.addEventListener('pointerdown', outside)
    root.current?.addEventListener('keydown', escape)
    const element = root.current
    return () => { document.removeEventListener('pointerdown', outside); element?.removeEventListener('keydown', escape) }
  }, [open])
  const { data: tagsData } = useQuery({ queryKey: ['tags'], queryFn: fetchTags })
  const assignments = useQuery({ queryKey: ['tagAssignments', nodeId], queryFn: ({ signal }) => fetchTagAssignments(nodeId ? [nodeId] : [], signal), enabled: open && Boolean(nodeId) })
  const assignmentsData = assignments.data
  const tags = useMemo(() => tagsData?.tags || [], [tagsData?.tags])
  const assignment = assignmentsData?.assignments.find((item) => item.node_id === nodeId)
  const manual = assignment?.manual_tag_ids || []
  const automatic = assignment?.auto_tag_ids || []
  const selected = draft ?? manual
  const visibleTags = useMemo(() => {
    const byName = new Map(tags.map((tag) => [tag.name, tag]))
    return currentTagNames.map((name) => byName.get(name)).filter((tag) => tag != null)
  }, [currentTagNames, tags])

  if (!nodeId) {
    return <div className="mt-1 flex flex-wrap gap-1">{visibleTags.map((tag) => <TagBadge key={tag.id} tag={tag} compact />)}</div>
  }

  const save = async () => {
    setSaving(true)
    try {
      await setNodeManualTags(nodeId, selected)
      toast.success('人工标签已更新')
      setDraft(null)
      setOpen(false)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['tagAssignments'] }),
        queryClient.invalidateQueries({ queryKey: ['configNodes'] }),
        queryClient.invalidateQueries({ queryKey: ['nodes'] }),
        queryClient.invalidateQueries({ queryKey: ['tags'] }),
        queryClient.invalidateQueries({ queryKey: ['groupPools'] }),
        queryClient.invalidateQueries({ queryKey: ['nodeQuery'] }),
        queryClient.invalidateQueries({ queryKey: ['nodeFacets'] }),
      ])
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '更新人工标签失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div ref={root} className="relative mt-1.5">
      <button
        type="button"
        className="flex min-h-7 cursor-pointer flex-wrap items-center gap-1 rounded-lg text-left transition-colors hover:bg-base-200/70 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary"
        onClick={() => { setDraft(null); setOpen((value) => !value) }}
        aria-expanded={open}
        aria-label={`编辑节点 ${nodeId} 的人工标签`}
      >
        {visibleTags.slice(0, 3).map((tag) => <TagBadge key={tag.id} tag={tag} compact />)}
        {visibleTags.length > 3 && <span className="badge badge-ghost badge-xs">+{visibleTags.length - 3}</span>}
        {visibleTags.length === 0 && <span className="inline-flex items-center gap-1 text-[10px] text-base-content/40"><TagsIcon className="h-3 w-3" />添加标签</span>}
      </button>
      {open && (
        <div className="absolute left-0 z-40 mt-1 w-72 rounded-xl border border-base-300 bg-base-100 p-3 shadow-xl" role="dialog" aria-label="编辑节点人工标签">
          <div className="flex items-center justify-between"><div><div className="text-xs font-bold">人工标签</div><div className="text-[10px] text-base-content/45">自动标签只读，由规则维护</div></div><button className="btn btn-ghost btn-xs btn-circle" onClick={() => { setOpen(false); setDraft(null) }} aria-label="关闭"><X className="h-3.5 w-3.5" /></button></div>
          <div className="mt-3 max-h-52 space-y-1 overflow-y-auto">
            {assignments.isPending && <p role="status" className="text-xs">正在加载人工标签…</p>}
            {assignments.isError && <p role="alert" className="text-xs text-error">加载标签失败，请关闭后重试。</p>}
            {tags.map((tag) => {
              const checked = selected.includes(tag.id)
              const isAuto = automatic.includes(tag.id)
              return <label key={tag.id} className={cn('flex cursor-pointer items-center gap-2 rounded-lg px-2 py-2 hover:bg-base-200', isAuto && 'bg-info/5')}><input type="checkbox" className="checkbox checkbox-primary checkbox-xs" disabled={!assignments.isSuccess || assignments.isFetching || saving} checked={checked} onChange={() => setDraft(checked ? selected.filter((id) => id !== tag.id) : [...selected, tag.id])} /><TagBadge tag={tag} compact /><span className="ml-auto text-[10px] text-base-content/40">{isAuto ? '自动' : ''}</span></label>
            })}
            {tags.length === 0 && <div className="py-5 text-center text-xs text-base-content/40">请先创建标签</div>}
          </div>
          <div className="mt-3 flex justify-end gap-2 border-t border-base-200 pt-3"><button className="btn btn-ghost btn-xs" onClick={() => { setOpen(false); setDraft(null) }}>取消</button><button className="btn btn-primary btn-xs gap-1" disabled={saving || !assignments.isSuccess || assignments.isFetching} onClick={() => void save()}>{saving ? <span className="loading loading-spinner loading-xs" /> : <Check className="h-3 w-3" />}保存</button></div>
        </div>
      )}
    </div>
  )
}
