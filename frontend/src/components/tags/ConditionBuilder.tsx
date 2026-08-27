import { Braces, Plus, Trash2 } from 'lucide-react'
import type { TagCondition, TagSchema } from '../../types'
import { cn } from '../../utils/cn'
import { controlClass } from '../ui/PageLayout'
import ConditionRow from './ConditionRow'
import { createLeafCondition } from './conditionDefaults'

interface ConditionBuilderProps {
  value: TagCondition
  schema: TagSchema
  onChange: (value: TagCondition) => void
}

function countNodes(condition: TagCondition): number {
  return 1 + (condition.children || []).reduce((sum, child) => sum + countNodes(child), 0)
}

export default function ConditionBuilder({ value, schema, onChange }: ConditionBuilderProps) {
  const root = value.match ? value : { match: 'all' as const, children: value.field ? [value] : [createLeafCondition(schema)] }
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3"><div><h4 className="text-sm font-bold">自动打标条件</h4><p className="mt-0.5 text-xs text-base-content/45">最多 {schema.limits.max_conditions} 个条件、嵌套 {schema.limits.max_depth} 层</p></div><span className="badge badge-ghost badge-sm font-mono">{countNodes(root)} / {schema.limits.max_conditions}</span></div>
      <ConditionGroup condition={root} depth={1} totalNodes={countNodes(root)} schema={schema} onChange={onChange} />
    </div>
  )
}

function ConditionGroup({ condition, depth, totalNodes, schema, onChange, onRemove }: {
  condition: TagCondition
  depth: number
  totalNodes: number
  schema: TagSchema
  onChange: (value: TagCondition) => void
  onRemove?: () => void
}) {
  const children = condition.children || []
  const canAddLeaf = totalNodes < schema.limits.max_conditions
  const canAddGroup = depth < schema.limits.max_depth - 1 && totalNodes + 2 <= schema.limits.max_conditions
  const updateChild = (index: number, child: TagCondition) => onChange({ ...condition, children: children.map((item, itemIndex) => itemIndex === index ? child : item) })
  const removeChild = (index: number) => onChange({ ...condition, children: children.filter((_, itemIndex) => itemIndex !== index) })

  return (
    <section className={cn('rounded-2xl border p-3 sm:p-4', depth === 1 ? 'border-primary/25 bg-primary/5' : 'border-base-300 bg-base-200/25')}>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Braces className="h-4 w-4 text-primary" />
        <span className="text-xs font-semibold text-base-content/55">条件组</span>
        <select className={cn('select select-xs w-32', controlClass)} value={condition.match || 'all'} onChange={(event) => onChange({ ...condition, match: event.target.value as TagCondition['match'] })} aria-label="条件组匹配方式"><option value="all">全部满足</option><option value="any">任一满足</option><option value="none">全部不满足</option></select>
        <span className="text-[10px] text-base-content/35">第 {depth} 层</span>
        {onRemove && <button type="button" className="btn btn-ghost btn-xs btn-square ml-auto text-error" onClick={onRemove} aria-label="删除条件组"><Trash2 className="h-3.5 w-3.5" /></button>}
      </div>
      <div className="space-y-2.5">
        {children.map((child, index) => child.match
          ? <ConditionGroup key={`group-${index}`} condition={child} depth={depth + 1} totalNodes={totalNodes} schema={schema} onChange={(next) => updateChild(index, next)} onRemove={() => removeChild(index)} />
          : <ConditionRow key={`leaf-${index}`} value={child} schema={schema} onChange={(next) => updateChild(index, next)} onRemove={() => removeChild(index)} />)}
        {children.length === 0 && <div className="rounded-xl border border-dashed border-warning/40 bg-warning/5 px-3 py-5 text-center text-xs text-warning-content">至少添加一个条件</div>}
      </div>
      <div className="mt-3 flex flex-wrap gap-2">
        <button type="button" className="btn btn-outline btn-xs gap-1" disabled={!canAddLeaf} onClick={() => onChange({ ...condition, children: [...children, createLeafCondition(schema)] })}><Plus className="h-3 w-3" />添加条件</button>
        <button type="button" className="btn btn-ghost btn-xs gap-1" disabled={!canAddGroup} onClick={() => onChange({ ...condition, children: [...children, { match: 'all', children: [createLeafCondition(schema)] }] })}><Braces className="h-3 w-3" />添加分组</button>
      </div>
    </section>
  )
}
