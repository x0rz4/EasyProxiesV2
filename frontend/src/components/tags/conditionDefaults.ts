import type { TagCondition, TagFieldDef, TagSchema } from '../../types'

function defaultValue(field: TagFieldDef): unknown {
  if (field.kind === 'bool') return true
  if (field.kind === 'int') return 0
  return ''
}

export function createLeafCondition(schema: TagSchema): TagCondition {
  const field = schema.fields[0]
  if (!field) return {}
  const op = field.operators[0] || 'eq'
  const arity = schema.operators.find((item) => item.value === op)?.value_arity || 'one'
  const condition: TagCondition = { field: field.name, op }
  if (arity === 'one') condition.value = defaultValue(field)
  if (arity === 'many') condition.values = []
  if (arity === 'two') condition.values = [defaultValue(field), defaultValue(field)]
  return condition
}

export function createDefaultCondition(schema: TagSchema): TagCondition {
  return { match: 'all', children: [createLeafCondition(schema)] }
}
