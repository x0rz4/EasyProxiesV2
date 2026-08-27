import type { CSSProperties } from 'react'
import type { Tag } from '../../types'
import { cn } from '../../utils/cn'

interface TagBadgeProps {
  tag: Pick<Tag, 'name' | 'color'>
  compact?: boolean
  muted?: boolean
  className?: string
}

function foregroundFor(color: string): string {
  const match = /^#([0-9a-f]{6})$/i.exec(color)
  if (!match) return 'currentColor'
  const value = match[1]
  const red = Number.parseInt(value.slice(0, 2), 16)
  const green = Number.parseInt(value.slice(2, 4), 16)
  const blue = Number.parseInt(value.slice(4, 6), 16)
  return (red * 299 + green * 587 + blue * 114) / 1000 > 150 ? '#111827' : '#ffffff'
}

export default function TagBadge({ tag, compact = false, muted = false, className }: TagBadgeProps) {
  const color = tag.color?.trim()
  const style: CSSProperties | undefined = color
    ? { backgroundColor: color, borderColor: color, color: foregroundFor(color) }
    : undefined
  return (
    <span
      className={cn(
        'badge max-w-full border font-semibold',
        compact ? 'badge-xs text-[10px]' : 'badge-sm text-xs',
        !color && 'badge-ghost',
        muted && 'opacity-65',
        className,
      )}
      style={style}
      title={tag.name}
    >
      <span className="truncate">{tag.name}</span>
    </span>
  )
}
