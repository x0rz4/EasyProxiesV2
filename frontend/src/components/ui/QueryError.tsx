export default function QueryError({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  if (!error) return null
  return <div role="alert" className="alert alert-error alert-soft text-sm">
    <span>数据加载失败：{error instanceof Error ? error.message : '请稍后重试'}</span>
    {onRetry && <button className="btn btn-ghost btn-xs" onClick={onRetry}>重试</button>}
  </div>
}
