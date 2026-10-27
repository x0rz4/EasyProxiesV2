import { cloneElement, isValidElement, type ReactNode } from 'react'
import { cn } from '../../utils/cn'

export default function Field({ id, label, hint, className, children }: {
  id?: string; label: string; hint?: string; className?: string; children: ReactNode
}) {
  const control = !id && isValidElement<Record<string, unknown>>(children) && typeof children.type === 'string' && ['input', 'select', 'textarea'].includes(children.type)
    ? cloneElement(children, { 'aria-label': children.props['aria-label'] ?? label }) : children
  return <fieldset className={cn('fieldset', className)}>
    {id ? <label htmlFor={id} className="fieldset-legend font-semibold text-base-content/80">{label}</label>
      : <legend className="fieldset-legend font-semibold text-base-content/80">{label}</legend>}
    {control}
    {hint && <p className="mt-1 text-xs text-base-content/45">{hint}</p>}
  </fieldset>
}
