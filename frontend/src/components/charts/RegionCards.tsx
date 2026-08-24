import { regionFlag } from '../../utils/region'

interface RegionStat {
  region: string
  total: number
  healthy: number
  avgLatency: number
}

interface RegionCardsProps {
  regions: RegionStat[]
  title?: string
}

function healthColor(rate: number): string {
  if (rate >= 0.8) return 'progress-success'
  if (rate >= 0.5) return 'progress-warning'
  return 'progress-error'
}

function latencyColorClass(ms: number): string {
  if (ms <= 0) return 'text-base-content/40'
  if (ms <= 100) return 'text-success'
  if (ms <= 300) return 'text-warning'
  return 'text-error'
}

export default function RegionCards({ regions, title }: RegionCardsProps) {
  const sorted = [...regions].sort((a, b) => b.total - a.total)

  if (sorted.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-8">
        <div className="text-base-content/30 text-sm">暂无地区数据</div>
      </div>
    )
  }

  return (
    <div className="w-full">
      {title && (
        <h4 className="text-sm font-semibold mb-3 flex items-center gap-2">
          <span className="w-1 h-4 bg-secondary rounded-full"></span>
          {title}
        </h4>
      )}
      <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-3">
        {sorted.map((r) => {
          const healthRate = r.total > 0 ? r.healthy / r.total : 0
          const healthPct = Math.round(healthRate * 100)
          return (
            <div
              key={r.region}
              className="rounded-xl bg-base-200/50 border border-base-300/30 p-3 hover:border-primary/30 transition-colors"
            >
              {/* Header: Flag + Region */}
              <div className="flex items-center gap-2 mb-2">
                <span className="text-lg">{regionFlag(r.region)}</span>
                <span className="font-semibold text-sm uppercase">{r.region}</span>
                <span className="text-xs text-base-content/40 ml-auto">{r.total} 节点</span>
              </div>
              {/* Health bar */}
              <div className="flex items-center gap-2 mb-1.5">
                <progress
                  className={`progress ${healthColor(healthRate)} h-2 flex-1`}
                  value={healthPct}
                  max={100}
                />
                <span className="text-xs font-mono font-medium w-8 text-right tabular-nums">
                  {healthPct}%
                </span>
              </div>
              {/* Stats row */}
              <div className="flex justify-between text-xs text-base-content/50">
                <span>
                  健康 <strong className="text-success">{r.healthy}</strong>/{r.total}
                </span>
                <span className={latencyColorClass(r.avgLatency)}>
                  {r.avgLatency > 0 ? `${r.avgLatency}ms` : '-'}
                </span>
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}