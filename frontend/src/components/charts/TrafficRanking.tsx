import { formatBytes } from '../../utils/format'
import { regionFlag } from '../../utils/region'

interface TrafficRankItem {
  name: string
  upload: number
  download: number
  region?: string
}

interface TrafficRankingProps {
  items: TrafficRankItem[]
  maxItems?: number
  title?: string
}

export default function TrafficRanking({ items, maxItems = 10, title }: TrafficRankingProps) {
  const sorted = [...items]
    .filter(i => (i.upload + i.download) > 0)
    .sort((a, b) => (b.upload + b.download) - (a.upload + a.download))
    .slice(0, maxItems)

  if (sorted.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-8">
        <div className="text-base-content/30 text-sm">暂无流量数据</div>
      </div>
    )
  }

  const maxTraffic = sorted[0].upload + sorted[0].download

  return (
    <div className="w-full">
      {title && (
        <h4 className="text-sm font-semibold mb-3 flex items-center gap-2">
          <span className="w-1 h-4 bg-secondary rounded-full"></span>
          {title}
        </h4>
      )}
      <div className="space-y-2">
        {sorted.map((item, i) => {
          const total = item.upload + item.download
          const widthPct = Math.max((total / maxTraffic) * 100, 8)
          const uploadPct = total > 0 ? (item.upload / total) * 100 : 0
          const flag = regionFlag(item.region)
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
              {/* Stacked bar: upload + download */}
              <div className="flex-1 h-5 bg-base-300/30 rounded-md overflow-hidden relative">
                <div
                  className="h-full rounded-md flex transition-all duration-500"
                  style={{ width: `${widthPct}%` }}
                >
                  {/* Upload portion */}
                  <div
                    className="h-full"
                    style={{
                      width: `${uploadPct}%`,
                      backgroundColor: 'oklch(0.70 0.15 250)',
                      opacity: 0.8,
                    }}
                  />
                  {/* Download portion */}
                  <div
                    className="h-full flex-1"
                    style={{
                      backgroundColor: 'oklch(0.72 0.19 142)',
                      opacity: 0.8,
                    }}
                  />
                </div>
              </div>
              {/* Traffic values */}
              <span className="text-xs font-mono w-20 text-right shrink-0 tabular-nums text-base-content/70">
                {formatBytes(total)}
              </span>
            </div>
          )
        })}
      </div>
      {/* Legend */}
      <div className="flex items-center justify-end gap-4 mt-2 text-xs text-base-content/40">
        <span className="flex items-center gap-1">
          <span className="inline-block w-2.5 h-2.5 rounded-sm" style={{ backgroundColor: 'oklch(0.70 0.15 250)', opacity: 0.8 }}></span>
          上传
        </span>
        <span className="flex items-center gap-1">
          <span className="inline-block w-2.5 h-2.5 rounded-sm" style={{ backgroundColor: 'oklch(0.72 0.19 142)', opacity: 0.8 }}></span>
          下载
        </span>
      </div>
    </div>
  )
}