import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { GitCompareArrows, Plus, RefreshCw, Sparkles, Tags as TagsIcon, Users, WandSparkles } from 'lucide-react'
import { toast } from 'sonner'
import { createTag, deleteTag, fetchConfigNodes, fetchTagSchema, fetchTags, recomputeTags, seedTagTemplates, setTagAuto, updateTag } from '../api/client'
import type { Tag, TagPayload } from '../types'
import { cn } from '../utils/cn'
import { PageContent, PageHeader, PageLayout, surfaceClass } from './ui/PageLayout'
import MutexGroupManager from './tags/MutexGroupManager'
import TagEditorDrawer from './tags/TagEditorDrawer'
import TagList from './tags/TagList'

export default function TagsPanel() {
  const queryClient = useQueryClient()
  const { data, isLoading, isFetching, refetch } = useQuery({ queryKey: ['tags'], queryFn: fetchTags })
  const { data: schema } = useQuery({ queryKey: ['tagSchema'], queryFn: fetchTagSchema, staleTime: Infinity })
  const { data: configData } = useQuery({ queryKey: ['configNodes'], queryFn: () => fetchConfigNodes() })
  const [editing, setEditing] = useState<Tag | null | undefined>(undefined)
  const [mutexOpen, setMutexOpen] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const tags = data?.tags || []
  const mutexGroups = data?.mutex_groups || []
  const taggedNodes = useMemo(
    () => configData?.nodes.filter((node) => (node.tags?.length || 0) > 0).length || 0,
    [configData?.nodes],
  )
  const automaticTags = tags.filter((tag) => tag.auto_enabled).length

  const refreshAffected = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['tags'] }),
      queryClient.invalidateQueries({ queryKey: ['tagAssignments'] }),
      queryClient.invalidateQueries({ queryKey: ['configNodes'] }),
      queryClient.invalidateQueries({ queryKey: ['nodes'] }),
      queryClient.invalidateQueries({ queryKey: ['groupPools'] }),
    ])
  }
  const run = async (key: string, task: () => Promise<unknown>, message: string) => {
    setBusy(key)
    try { await task(); toast.success(message); await refreshAffected() }
    catch (error) { toast.error(error instanceof Error ? error.message : '操作失败') }
    finally { setBusy(null) }
  }
  const save = async (payload: TagPayload) => {
    setBusy('save')
    try {
      if (editing) await updateTag(editing.id, payload)
      else await createTag(payload)
      toast.success(editing ? '标签已更新' : '标签已创建')
      setEditing(undefined)
      await refreshAffected()
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存标签失败')
    } finally { setBusy(null) }
  }
  const remove = (tag: Tag) => {
    const used = tag.used_by_groups || []
    const message = used.length
      ? `标签“${tag.name}”正被 ${used.length} 个分组引用。强制删除会同步清理这些分组的白/黑名单，确认继续？`
      : `删除标签“${tag.name}”？节点上的人工与自动分配都会一并移除。`
    if (!window.confirm(message)) return
    void run(`delete-${tag.id}`, () => deleteTag(tag.id, used.length > 0), '标签已删除')
  }
  const seed = async () => {
    setBusy('seed')
    try {
      const result = await seedTagTemplates()
      toast.success('内置模板处理完成', { description: `创建 ${result.created.length}，跳过 ${result.skipped.length}，冲突 ${result.conflicts.length}` })
      await refreshAffected()
    } catch (error) { toast.error(error instanceof Error ? error.message : '种入模板失败') }
    finally { setBusy(null) }
  }

  if (isLoading) return <div className="flex h-64 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>

  return (
    <PageLayout>
      <PageHeader title="节点标签" description="用人工标签和可审计规则组织节点，并驱动分组成员筛选" icon={<TagsIcon className="h-5 w-5" />} actions={<div className="flex gap-2"><button className="btn btn-ghost btn-sm btn-square" onClick={() => void refetch()} disabled={isFetching} aria-label="刷新标签"><RefreshCw className={cn('h-4 w-4', isFetching && 'animate-spin')} /></button><button className="btn btn-ghost btn-sm gap-2 border border-base-300" disabled={busy === 'recompute'} onClick={() => void run('recompute', () => recomputeTags(), '标签已全量重算')}>{busy === 'recompute' ? <span className="loading loading-spinner loading-xs" /> : <WandSparkles className="h-4 w-4" />}<span className="hidden lg:inline">重算全部</span></button><button className="btn btn-ghost btn-sm gap-2 border border-base-300" disabled={busy === 'seed'} onClick={() => void seed()}>{busy === 'seed' ? <span className="loading loading-spinner loading-xs" /> : <Sparkles className="h-4 w-4" />}<span className="hidden lg:inline">种入内置模板</span></button><button className="btn btn-primary btn-sm gap-2" onClick={() => setEditing(null)}><Plus className="h-4 w-4" /><span className="hidden sm:inline">新建标签</span></button></div>} />
      <PageContent>
        <section className="grid grid-cols-2 gap-4 xl:grid-cols-4"><SummaryCard label="标签数" value={tags.length} hint="人工与规则标签" icon={<TagsIcon className="h-5 w-5" />} tone="text-primary" /><SummaryCard label="自动标签" value={automaticTags} hint="当前启用规则" icon={<WandSparkles className="h-5 w-5" />} tone="text-success" /><SummaryCard label="互斥组" value={mutexGroups.length} hint="优先级裁决范围" icon={<GitCompareArrows className="h-5 w-5" />} tone="text-info" /><SummaryCard label="已打标节点" value={taggedNodes} hint="至少拥有一个标签" icon={<Users className="h-5 w-5" />} tone="text-warning" /></section>
        <div className="flex justify-end"><button className="btn btn-outline btn-sm gap-2" onClick={() => setMutexOpen(true)}><GitCompareArrows className="h-4 w-4" />管理互斥组</button></div>
        {tags.length === 0 ? <section className={cn(surfaceClass, 'flex min-h-80 flex-col items-center justify-center border-dashed px-6 text-center')}><div className="flex h-16 w-16 items-center justify-center rounded-2xl bg-primary/10 text-primary"><TagsIcon className="h-8 w-8" /></div><h3 className="mt-5 text-xl font-bold">创建第一个节点标签</h3><p className="mt-2 max-w-md text-sm leading-6 text-base-content/55">可以先种入内置模板，也可以创建人工标签并逐个分配给节点。</p><div className="mt-6 flex gap-2"><button className="btn btn-outline gap-2" onClick={() => void seed()}><Sparkles className="h-4 w-4" />种入内置模板</button><button className="btn btn-primary gap-2" onClick={() => setEditing(null)}><Plus className="h-4 w-4" />新建标签</button></div></section> : <TagList tags={tags} mutexGroups={mutexGroups} busy={busy} onEdit={(tag) => setEditing(tag)} onDelete={remove} onToggleAuto={(tag) => void run(`auto-${tag.id}`, () => setTagAuto(tag.id, !tag.auto_enabled), tag.auto_enabled ? '已关闭自动打标' : '已启用自动打标')} onBatchAuto={(ids, enabled) => void run('batch-auto', () => Promise.all(ids.map((id) => setTagAuto(id, enabled))), enabled ? '所选标签已启用自动打标' : '所选标签已关闭自动打标')} />}
      </PageContent>
      {editing !== undefined && schema && <TagEditorDrawer key={editing?.id || 'new'} tag={editing} mutexGroups={mutexGroups} schema={schema} saving={busy === 'save'} onClose={() => setEditing(undefined)} onSave={save} />}
      {mutexOpen && <MutexGroupManager onClose={() => setMutexOpen(false)} />}
    </PageLayout>
  )
}

function SummaryCard({ label, value, hint, icon, tone }: { label: string; value: number; hint: string; icon: ReactNode; tone: string }) {
  return <article className={cn(surfaceClass, 'p-4 sm:p-5')}><div className="flex items-center justify-between"><span className="text-sm font-medium text-base-content/55">{label}</span><span className={cn('rounded-xl bg-base-200 p-2', tone)}>{icon}</span></div><div className={cn('mt-2 text-3xl font-black tabular-nums', tone)}>{value}</div><p className="mt-1 text-xs text-base-content/45">{hint}</p></article>
}
