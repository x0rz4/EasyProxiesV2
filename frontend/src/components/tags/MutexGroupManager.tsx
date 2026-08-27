import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Save, Trash2, X } from 'lucide-react'
import { toast } from 'sonner'
import { createTagMutexGroup, deleteTagMutexGroup, fetchTagMutexGroups, updateTagMutexGroup } from '../../api/client'
import type { TagMutexGroup } from '../../types'
import { cn } from '../../utils/cn'
import { controlClass } from '../ui/PageLayout'

interface MutexGroupManagerProps {
  onClose: () => void
}

export default function MutexGroupManager({ onClose }: MutexGroupManagerProps) {
  const queryClient = useQueryClient()
  const inputRef = useRef<HTMLInputElement>(null)
  const { data, isLoading } = useQuery({ queryKey: ['tagMutexGroups'], queryFn: fetchTagMutexGroups })
  const [editing, setEditing] = useState<TagMutexGroup | null>(null)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const groups = data?.mutex_groups || []

  const reset = () => { setEditing(null); setName(''); setDescription(''); window.setTimeout(() => inputRef.current?.focus(), 0) }
  const edit = (group: TagMutexGroup) => { setEditing(group); setName(group.name); setDescription(group.description); window.setTimeout(() => inputRef.current?.focus(), 0) }
  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['tagMutexGroups'] }),
      queryClient.invalidateQueries({ queryKey: ['tags'] }),
      queryClient.invalidateQueries({ queryKey: ['tagAssignments'] }),
      queryClient.invalidateQueries({ queryKey: ['configNodes'] }),
      queryClient.invalidateQueries({ queryKey: ['nodes'] }),
      queryClient.invalidateQueries({ queryKey: ['groupPools'] }),
    ])
  }
  const save = async () => {
    if (!name.trim()) return
    setBusy('save')
    try {
      if (editing) await updateTagMutexGroup(editing.id, { name: name.trim(), description: description.trim() })
      else await createTagMutexGroup({ name: name.trim(), description: description.trim() })
      toast.success(editing ? '互斥组已更新' : '互斥组已创建')
      reset(); await refresh()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存互斥组失败')
    } finally { setBusy(null) }
  }
  const remove = async (group: TagMutexGroup) => {
    if (!window.confirm(`删除互斥组“${group.name}”？\n只会解除成员标签之间的互斥关系，不会删除标签。`)) return
    setBusy(`delete-${group.id}`)
    try {
      await deleteTagMutexGroup(group.id)
      toast.success('互斥组已删除', { description: '成员标签已解除互斥，标签本身仍然保留。' })
      if (editing?.id === group.id) reset()
      await refresh()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '删除互斥组失败')
    } finally { setBusy(null) }
  }

  return (
    <div className="modal modal-open" role="dialog" aria-modal="true" aria-label="互斥组管理">
      <div className="modal-box max-h-[90vh] w-11/12 max-w-3xl overflow-y-auto p-0">
        <header className="sticky top-0 z-10 flex items-center justify-between border-b border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl"><div><h3 className="text-lg font-bold">互斥组管理</h3><p className="mt-0.5 text-xs text-base-content/50">一个节点在同一互斥组内最多应用一个标签</p></div><button className="btn btn-ghost btn-sm btn-square" onClick={onClose} aria-label="关闭"><X className="h-4 w-4" /></button></header>
        <div className="grid gap-5 p-5 md:grid-cols-[minmax(0,1fr)_minmax(17rem,0.75fr)]">
          <section><h4 className="text-sm font-bold">已有互斥组</h4><div className="mt-3 space-y-2">{isLoading ? <div className="py-12 text-center"><span className="loading loading-spinner text-primary" /></div> : groups.map((group) => <article key={group.id} className="flex items-start gap-3 rounded-xl border border-base-300 bg-base-200/20 p-3"><div className="min-w-0 flex-1"><div className="font-semibold">{group.name}</div><p className="mt-1 text-xs leading-5 text-base-content/50">{group.description || '暂无说明'}</p></div><button className="btn btn-ghost btn-xs btn-square text-info" onClick={() => edit(group)} aria-label={`编辑 ${group.name}`}><Pencil className="h-3.5 w-3.5" /></button><button className="btn btn-ghost btn-xs btn-square text-error" disabled={busy === `delete-${group.id}`} onClick={() => void remove(group)} aria-label={`删除 ${group.name}`}>{busy === `delete-${group.id}` ? <span className="loading loading-spinner loading-xs" /> : <Trash2 className="h-3.5 w-3.5" />}</button></article>)}{!isLoading && groups.length === 0 && <div className="rounded-xl border border-dashed border-base-300 py-10 text-center text-xs text-base-content/40">暂无互斥组</div>}</div></section>
          <section className="rounded-2xl border border-primary/20 bg-primary/5 p-4"><h4 className="text-sm font-bold">{editing ? '编辑互斥组' : '新建互斥组'}</h4><label className="mt-3 block text-xs font-semibold">名称<input ref={inputRef} autoFocus className={cn('input mt-1.5 w-full', controlClass)} value={name} onChange={(event) => setName(event.target.value)} maxLength={64} /></label><label className="mt-3 block text-xs font-semibold">说明<textarea className={cn('textarea mt-1.5 min-h-24 w-full', controlClass)} value={description} onChange={(event) => setDescription(event.target.value)} /></label><div className="mt-4 flex justify-end gap-2">{editing && <button className="btn btn-ghost btn-sm" onClick={reset}>取消编辑</button>}<button className="btn btn-primary btn-sm gap-2" disabled={!name.trim() || busy === 'save'} onClick={() => void save()}>{busy === 'save' ? <span className="loading loading-spinner loading-xs" /> : editing ? <Save className="h-3.5 w-3.5" /> : <Plus className="h-3.5 w-3.5" />}{editing ? '保存' : '创建'}</button></div></section>
        </div>
      </div>
      <button className="modal-backdrop" onClick={onClose} aria-label="关闭互斥组管理" />
    </div>
  )
}
