import { useId, useMemo, useState } from 'react'
import { Trash2 } from 'lucide-react'
import type { TagCondition, TagFieldDef, TagSchema } from '../../types'
import { cn } from '../../utils/cn'
import { controlClass } from '../ui/PageLayout'

interface ConditionRowProps {
  value: TagCondition
  schema: TagSchema
  onChange: (value: TagCondition) => void
  onRemove: () => void
}

function defaultValue(field: TagFieldDef): unknown {
  if (field.kind === 'bool') return true
  if (field.kind === 'int') return 0
  return ''
}

function valueForInput(value: unknown): string {
  if (typeof value === 'string' || typeof value === 'number') return String(value)
  if (typeof value === 'boolean') return value ? 'true' : 'false'
  return ''
}

function parseValue(field: TagFieldDef, raw: string): unknown {
  if (field.kind === 'int') return Number(raw) || 0
  if (field.kind === 'bool') return raw === 'true'
  return raw
}

export default function ConditionRow({ value, schema, onChange, onRemove }: ConditionRowProps) {
  const field = schema.fields.find((item) => item.name === value.field) || schema.fields[0]
  const operator = schema.operators.find((item) => item.value === value.op)
  const arity = operator?.value_arity || 'one'
  const enumSet = field?.enum_key ? schema.enums[field.enum_key] : undefined

  const changeField = (name: string) => {
    const nextField = schema.fields.find((item) => item.name === name)
    if (!nextField) return
    const op = nextField.operators[0] || 'eq'
    const nextArity = schema.operators.find((item) => item.value === op)?.value_arity || 'one'
    const next: TagCondition = { field: name, op, negate: value.negate }
    if (nextArity === 'one') next.value = defaultValue(nextField)
    if (nextArity === 'many') next.values = []
    if (nextArity === 'two') next.values = [defaultValue(nextField), defaultValue(nextField)]
    onChange(next)
  }

  const changeOperator = (op: string) => {
    if (!field) return
    const nextArity = schema.operators.find((item) => item.value === op)?.value_arity || 'one'
    const next: TagCondition = { field: field.name, op, negate: value.negate, max_age_seconds: value.max_age_seconds }
    if (nextArity === 'one') next.value = defaultValue(field)
    if (nextArity === 'many') next.values = []
    if (nextArity === 'two') next.values = [defaultValue(field), defaultValue(field)]
    onChange(next)
  }

  if (!field) return <div className="rounded-xl border border-warning/30 bg-warning/5 p-3 text-xs text-warning">Schema 没有可用字段</div>

  return (
    <div className="rounded-xl border border-base-300 bg-base-100 p-3 shadow-sm">
      <div className="grid gap-2 md:grid-cols-[minmax(10rem,1.3fr)_minmax(8rem,0.8fr)_minmax(10rem,1.4fr)_auto]">
        <select className={cn('select select-sm w-full', controlClass)} value={field.name} onChange={(event) => changeField(event.target.value)} aria-label="规则字段">
          {schema.field_groups.map((group) => <optgroup key={group} label={group}>{schema.fields.filter((item) => item.group === group).map((item) => <option key={item.name} value={item.name}>{item.label}</option>)}</optgroup>)}
        </select>
        <select className={cn('select select-sm w-full', controlClass)} value={value.op || ''} onChange={(event) => changeOperator(event.target.value)} aria-label="规则操作符">
          {field.operators.map((op) => <option key={op} value={op}>{schema.operators.find((item) => item.value === op)?.label || op}</option>)}
        </select>
        <div className="min-w-0">
          {arity === 'none' ? <div className="flex h-8 items-center text-xs text-base-content/40">无需比较值</div> : null}
          {arity === 'one' ? <SingleValueInput field={field} enumSet={enumSet} value={value.value} onChange={(next) => onChange({ ...value, value: next, values: undefined })} /> : null}
          {arity === 'many' ? <ManyValueInput key={`${field.name}-${value.op}`} field={field} initial={value.values || []} onChange={(values) => onChange({ ...value, value: undefined, values })} /> : null}
          {arity === 'two' ? <div className="grid grid-cols-2 gap-2"><SingleValueInput field={field} enumSet={enumSet} value={value.values?.[0]} onChange={(next) => onChange({ ...value, value: undefined, values: [next, value.values?.[1] ?? defaultValue(field)] })} /><SingleValueInput field={field} enumSet={enumSet} value={value.values?.[1]} onChange={(next) => onChange({ ...value, value: undefined, values: [value.values?.[0] ?? defaultValue(field), next] })} /></div> : null}
        </div>
        <button type="button" className="btn btn-ghost btn-sm btn-square text-error" onClick={onRemove} aria-label="删除条件"><Trash2 className="h-4 w-4" /></button>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-base-200 pt-2">
        <label className="flex cursor-pointer items-center gap-2 text-xs"><input type="checkbox" className="checkbox checkbox-xs" checked={Boolean(value.negate)} onChange={(event) => onChange({ ...value, negate: event.target.checked || undefined })} />反向匹配</label>
        {field.supports_max_age && <label className="flex items-center gap-2 text-xs text-base-content/60">事实时效<input type="number" min={0} className={cn('input input-xs w-24 font-mono', controlClass)} value={value.max_age_seconds || 0} onChange={(event) => onChange({ ...value, max_age_seconds: Number(event.target.value) || undefined })} /><span>秒</span></label>}
        {field.unit && <span className="text-[10px] text-base-content/40">单位：{field.unit}</span>}
        {field.source && <span className="min-w-0 truncate text-[10px] text-base-content/35" title={field.source}>来源：{field.source}</span>}
      </div>
    </div>
  )
}

function SingleValueInput({ field, enumSet, value, onChange }: {
  field: TagFieldDef
  enumSet?: TagSchema['enums'][string]
  value: unknown
  onChange: (value: unknown) => void
}) {
  const listId = useId()
  if (field.kind === 'bool') {
    return <select className={cn('select select-sm w-full', controlClass)} value={value === false ? 'false' : 'true'} onChange={(event) => onChange(event.target.value === 'true')}><option value="true">是</option><option value="false">否</option></select>
  }
  if (field.kind === 'enum' && enumSet && !enumSet.free_input) {
    return <select className={cn('select select-sm w-full', controlClass)} value={valueForInput(value)} onChange={(event) => onChange(event.target.value)}><option value="">请选择</option>{enumSet.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select>
  }
  return <><input type={field.kind === 'int' ? 'number' : 'text'} list={enumSet ? listId : undefined} className={cn('input input-sm w-full', controlClass)} value={valueForInput(value)} onChange={(event) => onChange(parseValue(field, event.target.value))} placeholder={enumSet?.free_input ? '选择或自由输入' : '比较值'} />{enumSet && <datalist id={listId}>{enumSet.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</datalist>}</>
}

function ManyValueInput({ field, initial, onChange }: { field: TagFieldDef; initial: unknown[]; onChange: (value: unknown[]) => void }) {
  const [text, setText] = useState(() => initial.map(valueForInput).join(', '))
  const hint = useMemo(() => field.kind === 'int' ? '用逗号分隔整数' : '用逗号分隔多个值', [field.kind])
  return <input className={cn('input input-sm w-full', controlClass)} value={text} onChange={(event) => { const next = event.target.value; setText(next); onChange(next.split(/[,，\n]+/).map((item) => item.trim()).filter(Boolean).map((item) => parseValue(field, item))) }} placeholder={hint} />
}
