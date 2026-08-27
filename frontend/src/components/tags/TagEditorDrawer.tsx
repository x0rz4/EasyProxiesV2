import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Eye, Save, X } from 'lucide-react'
import type { Tag, TagCondition, TagMutexGroup, TagPayload, TagPreviewResponse, TagSchema } from '../../types'
import { previewTagRule } from '../../api/client'
import { cn } from '../../utils/cn'
import { controlClass } from '../ui/PageLayout'
import ConditionBuilder from './ConditionBuilder'
import { createDefaultCondition } from './conditionDefaults'
import TagPreviewPanel from './TagPreviewPanel'

interface TagEditorDrawerProps {
  tag: Tag | null
  mutexGroups: TagMutexGroup[]
  schema: TagSchema
  saving: boolean
  onClose: () => void
  onSave: (payload: TagPayload) => Promise<void>
}

function useDebouncedValue<T>(value: T, delay: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delay)
    return () => window.clearTimeout(timer)
  }, [delay, value])
  return debounced
}

export default function TagEditorDrawer({ tag, mutexGroups, schema, saving, onClose, onSave }: TagEditorDrawerProps) {
  const firstInput = useRef<HTMLInputElement>(null)
  const [form, setForm] = useState<TagPayload>(() => ({
    name: tag?.name || '',
    color: tag?.color || '#6366f1',
    description: tag?.description || '',
    mutex_group_id: tag?.mutex_group_id || 0,
    priority: tag?.priority || 0,
    auto_enabled: tag?.auto_enabled || false,
    rule: tag?.rule || createDefaultCondition(schema),
  }))
  const [preview, setPreview] = useState<TagPreviewResponse | null>(null)
  const [previewError, setPreviewError] = useState('')
  const [previewLoading, setPreviewLoading] = useState(false)
  const debouncedRule = useDebouncedValue(form.rule as TagCondition, 400)

  useEffect(() => {
    const previousFocus = document.activeElement as HTMLElement | null
    firstInput.current?.focus()
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKeyDown)
    return () => { window.removeEventListener('keydown', onKeyDown); previousFocus?.focus() }
  }, [onClose])

  useEffect(() => {
    const controller = new AbortController()
    const loadPreview = async () => {
      setPreviewLoading(true)
      setPreviewError('')
      try {
        const result = await previewTagRule({
          rule: debouncedRule,
          tag_id: tag?.id,
          mutex_group_id: form.mutex_group_id || 0,
          priority: form.priority || 0,
          limit: 30,
        }, controller.signal)
        setPreview(result)
      } catch (error: unknown) {
        if (error instanceof DOMException && error.name === 'AbortError') return
        setPreviewError(error instanceof Error ? error.message : '规则预览失败')
      } finally {
        if (!controller.signal.aborted) setPreviewLoading(false)
      }
    }
    void loadPreview()
    return () => controller.abort()
  }, [debouncedRule, form.mutex_group_id, form.priority, tag?.id])

  const rule = form.rule as TagCondition
  const canSave = form.name.trim().length > 0 && (!form.auto_enabled || Boolean(rule.field || rule.children?.length))

  return (
    <div className="modal modal-open" role="dialog" aria-modal="true" aria-label={tag ? '编辑标签' : '新建标签'}>
      <div className="modal-box max-h-[94vh] w-11/12 max-w-7xl overflow-hidden p-0">
        <header className="flex items-center justify-between border-b border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl">
          <div><h3 className="text-lg font-bold">{tag ? '编辑标签' : '新建标签'}</h3><p className="mt-0.5 text-xs text-base-content/50">人工标签立即生效；自动标签由规则重算维护</p></div>
          <button type="button" className="btn btn-ghost btn-sm btn-square" onClick={onClose} aria-label="关闭"><X className="h-4 w-4" /></button>
        </header>
        <div className="grid max-h-[calc(94vh-8.5rem)] overflow-y-auto xl:grid-cols-[minmax(0,1.45fr)_minmax(21rem,0.75fr)] xl:overflow-hidden">
          <div className="space-y-5 p-5 sm:p-6 xl:overflow-y-auto">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="标签名称" hint="1–64 个字符，支持 emoji"><input ref={firstInput} className={cn('input w-full', controlClass)} value={form.name} maxLength={64} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="例如：香港低延迟" /></Field>
              <Field label="标签颜色"><div className="flex gap-2"><input type="color" className="h-10 w-14 cursor-pointer rounded-lg border border-base-300 bg-base-100 p-1" value={form.color || '#6366f1'} onChange={(event) => setForm({ ...form, color: event.target.value })} aria-label="标签颜色" /><input className={cn('input w-full font-mono', controlClass)} value={form.color || ''} onChange={(event) => setForm({ ...form, color: event.target.value })} /></div></Field>
              <Field label="互斥组" hint="同一节点在一个互斥组内最多保留一个标签"><select className={cn('select w-full', controlClass)} value={form.mutex_group_id || 0} onChange={(event) => setForm({ ...form, mutex_group_id: Number(event.target.value) })}><option value={0}>不加入互斥组</option>{mutexGroups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select></Field>
              <Field label="优先级" hint="互斥组内数值越高越优先（0–1000）"><input type="number" min={0} max={1000} className={cn('input w-full', controlClass)} value={form.priority || 0} onChange={(event) => setForm({ ...form, priority: Math.max(0, Math.min(1000, Number(event.target.value) || 0)) })} /></Field>
            </div>
            <Field label="说明"><textarea className={cn('textarea min-h-20 w-full', controlClass)} value={form.description || ''} onChange={(event) => setForm({ ...form, description: event.target.value })} placeholder="说明这个标签的用途与维护方式" /></Field>
            <label className="flex cursor-pointer items-start gap-3 rounded-xl border border-primary/20 bg-primary/5 p-4"><input type="checkbox" className="toggle toggle-primary mt-0.5" checked={Boolean(form.auto_enabled)} onChange={(event) => setForm({ ...form, auto_enabled: event.target.checked })} /><span><strong className="block text-sm">启用自动打标</strong><span className="mt-0.5 block text-xs leading-5 text-base-content/50">关闭时保留规则但不自动分配；人工标签不受影响。</span></span></label>
            <ConditionBuilder value={rule} schema={schema} onChange={(next) => setForm({ ...form, rule: next })} />
          </div>
          <aside className="border-t border-base-300 bg-base-200/20 p-5 sm:p-6 xl:overflow-y-auto xl:border-l xl:border-t-0"><div className="mb-4 flex items-center gap-2"><Eye className="h-4 w-4 text-primary" /><div><h4 className="text-sm font-bold">规则预览</h4><p className="text-[10px] text-base-content/45">停止输入 400ms 后自动试跑</p></div></div><TagPreviewPanel preview={preview} loading={previewLoading} error={previewError} /></aside>
        </div>
        <footer className="flex items-center justify-between gap-3 border-t border-base-300 bg-base-100/95 px-5 py-4 backdrop-blur-xl"><span className="hidden text-xs text-base-content/45 sm:block">规则版本只在条件真正变化时递增</span><div className="ml-auto flex gap-2"><button type="button" className="btn btn-ghost" onClick={onClose}>取消</button><button type="button" className="btn btn-primary min-w-28 gap-2" disabled={!canSave || saving} onClick={() => void onSave({ ...form, name: form.name.trim() })}>{saving ? <span className="loading loading-spinner loading-sm" /> : <Save className="h-4 w-4" />}{tag ? '保存修改' : '创建标签'}</button></div></footer>
      </div>
      <button className="modal-backdrop" onClick={onClose} aria-label="关闭标签编辑器" />
    </div>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return <fieldset className="fieldset"><legend className="fieldset-legend font-semibold text-base-content/80">{label}</legend>{children}{hint && <p className="mt-1 text-xs text-base-content/45">{hint}</p>}</fieldset>
}
