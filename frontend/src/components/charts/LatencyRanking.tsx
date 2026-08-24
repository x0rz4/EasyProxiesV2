import { regionFlag } from '../../utils/region'

interface RankItem {
  name: string
  latency: number
  region?: string
  flag?: string
}

interface LatencyRankingProps {
  items: RankItem[]
  maxItems?: number
  title?: string
}

function latencyColor(ms: number): string {
  if (ms <= 100) return 'oklch(0.72 0.19 142)' // green
  if (ms <= 200) return 'oklch(0.80 0.18 84)'  // yellow
  if (ms <= 300) return 'oklch(0.75 0.18 55)'  // orange
  return 'oklch(0.63 0.24 29)'                  // red
}

export default function LatencyRanking({ items, maxItems = 10, title }: LatencyRankingProps) {
  const sorted = [...items]
    .filter(i => i.latency > 0)
    .sort((a, b) => a.latency - b.latency)
    .slice(0, maxItems)

  if (sorted.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-8">
        <div className="text-base-content/30 text-sm">暂无延迟数据</div>
      </div>
    )
  }

  const maxLatency = sorted[sorted.length - 1].latency

  return (
    <div className="w-full">
      {title && (
        <h4 className="text-sm font-semibold mb-3 flex items-center gap-2">
          <span className="w-1 h-4 bg-accent rounded-full"></span>
          {title}
        </h4>
      )}
      <div className="space-y-2">
        {sorted.map((item, i) => {
          const widthPct = Math.max((item.latency / maxLatency) * 100, 8)
          const flag = item.flag || regionFlag(item.region)
          return (
            <div key={i} className="flex items-center gap-2">
              {/* Rank */}
              <span className="w-5 text-xs font-mono text-base-content/40 text-right shrink-0">
                {i + 1}
              </span>
              {/* Flag + Name */}
              <span className="text-sm shrink-0 w-32 truncate">
                {flag} {item.name}
              </span>
              {/* Bar */}
              <div className="flex-1 h-5 bg-base-300/30 rounded-md overflow-hidden relative">
                <div
                  className="h-full rounded-md transition-all duration-500"
                  style={{
                    width: `${widthPct}%`,
                    backgroundColor: latencyColor(item.latency),
                    opacity: 0.8,
                  }}
                />
              </div>
              {/* Latency value */}
              <span className="text-xs font-mono font-medium w-14 text-right shrink-0 tabular-nums"
                style={{ color: latencyColor(item.latency) }}
              >
                {item.latency}ms
              </span>
            </div>
          )
        })}
      </div>
    </div>
  )
}