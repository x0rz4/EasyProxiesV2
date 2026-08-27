import { CircleAlert, Eye, ShieldCheck, ShieldX } from 'lucide-react'
import type { TagPreviewResponse } from '../../types'
import { cn } from '../../utils/cn'

interface TagPreviewPanelProps {
  preview: TagPreviewResponse | null
  loading: boolean
  error: string
}

const shadowReason: Record<string, string> = {
  manual_occupies_group: '该节点已有人工标签占据此互斥组',
  lower_priority: '同一互斥组内有更高优先级标签',
}

export default function TagPreviewPanel({ preview, loading, error }: TagPreviewPanelProps) {
  if (loading && !preview) return <div className="flex min-h-72 items-center justify-center"><span className="loading loading-spinner loading-lg text-primary" /></div>
  if (error) return <div className="rounded-xl border border-error/25 bg-error/5 p-4 text-sm text-error"><CircleAlert className="mb-2 h-5 w-5" />{error}</div>
  if (!preview) return <div className="flex min-h-72 flex-col items-center justify-center rounded-xl border border-dashed border-base-300 text-center"><Eye className="h-7 w-7 text-base-content/25" /><p className="mt-2 text-sm font-medium text-base-content/55">编辑规则后显示试跑结果</p><p className="mt-1 text-xs text-base-content/35">预览不会写入任何标签</p></div>

  return (
    <div className={cn('space-y-4 transition-opacity', loading && 'opacity-60')}>
      <div className="grid grid-cols-2 gap-2">
        <Metric label="命中" value={preview.match_count} tone="text-primary" />
        <Metric label="应用" value={preview.applied_count} tone="text-success" />
        <Metric label="被压制" value={preview.shadowed_count} tone={preview.shadowed_count ? 'text-warning' : ''} />
        <Metric label="事实未知" value={preview.unknown_count} tone={preview.unknown_count ? 'text-error' : ''} />
      </div>
      <div className="flex items-center justify-between text-xs text-base-content/45"><span>总节点 {preview.total_nodes}</span><span>展示 {preview.samples.length} 个样本</span></div>
      <div className="max-h-[52vh] space-y-2 overflow-y-auto pr-1">
        {preview.samples.map((sample) => (
          <article key={sample.node_id} className={cn('rounded-xl border p-3', sample.applied ? 'border-success/25 bg-success/5' : sample.matched ? 'border-warning/30 bg-warning/5' : 'border-base-300 bg-base-200/20')}>
            <div className="flex items-start gap-2">
              {sample.applied ? <ShieldCheck className="mt-0.5 h-4 w-4 shrink-0 text-success" /> : <ShieldX className="mt-0.5 h-4 w-4 shrink-0 text-base-content/35" />}
              <div className="min-w-0 flex-1"><div className="flex items-center gap-2"><span className="truncate text-sm font-semibold">{sample.name}</span>{sample.region && <span className="badge badge-ghost badge-xs uppercase">{sample.region}</span>}</div><div className="mt-2 grid gap-1">{Object.entries(sample.facts).map(([field, value]) => <div key={field} className="grid grid-cols-[minmax(7rem,0.8fr)_1fr] gap-2 text-[10px]"><code className="truncate text-base-content/45" title={field}>{field}</code><span className="truncate font-medium" title={value}>{value || '未知'}</span></div>)}</div></div>
            </div>
            {sample.shadowed && <p className="mt-2 rounded-lg bg-warning/10 px-2 py-1.5 text-[10px] leading-4 text-warning-content">{shadowReason[sample.shadowed.reason] || sample.shadowed.reason}{sample.shadowed.winner_tag_name ? `；胜出标签：${sample.shadowed.winner_tag_name}` : ''}</p>}
          </article>
        ))}
        {preview.samples.length === 0 && <div className="rounded-xl border border-dashed border-base-300 py-10 text-center text-xs text-base-content/40">没有可展示的节点样本</div>}
      </div>
    </div>
  )
}

function Metric({ label, value, tone = '' }: { label: string; value: number; tone?: string }) {
  return <div className="rounded-xl bg-base-200/60 px-3 py-2.5"><div className={cn('text-xl font-black tabular-nums', tone)}>{value}</div><div className="text-[10px] text-base-content/45">{label}</div></div>
}
