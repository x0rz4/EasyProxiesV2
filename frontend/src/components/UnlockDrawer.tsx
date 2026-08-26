import { useState, useRef } from 'react'
import type { NodeCheckResultItem, NodeSnapshot, UnlockResult } from '../types'
import { regionFlag } from '../utils/region'
import { Globe2, ShieldCheck, Tv } from 'lucide-react'

interface UnlockDrawerProps {
  node: NodeSnapshot | null
  result: UnlockResult | null
  diagnostic: NodeCheckResultItem | null
  isOpen: boolean
  onClose: () => void
}

export default function UnlockDrawer({ node, result, diagnostic, isOpen, onClose }: UnlockDrawerProps) {
  const [speed, setSpeed] = useState<number | null>(null)
  const [speedTesting, setSpeedTesting] = useState(false)
  const [speedError, setSpeedError] = useState('')
  const esRef = useRef<EventSource | null>(null)

  if (!isOpen || !node) return null

  const ipApi = diagnostic?.quality.find((item) => item.provider === 'ip-api')
  const detectedIP = diagnostic?.detection?.exit_ip || result?.ip.ip || ''
  const exitCountryCode = diagnostic?.detection?.exit_country_code || ipApi?.country_code || result?.ip.iso_code || ''

  const handleSpeedtest = () => {
    if (speedTesting) return
    setSpeed(null)
    setSpeedError('')
    setSpeedTesting(true)

    const es = new EventSource(`/api/nodes/${encodeURIComponent(node.tag)}/speedtest`)
    esRef.current = es

    es.addEventListener('progress', (e) => {
      const data = JSON.parse(e.data)
      setSpeed(data.mbps)
    })

    es.addEventListener('done', (e) => {
      const data = JSON.parse(e.data)
      setSpeed(data.mbps)
      setSpeedTesting(false)
      es.close()
    })

    es.addEventListener('error', (e) => {
      // e.data might not exist on native EventSource error events unless we sent it
      const errMessage = e instanceof MessageEvent && typeof e.data === 'string' && e.data
        ? e.data
        : '测速连接断开'
      setSpeedError(errMessage)
      setSpeedTesting(false)
      es.close()
    })
  }

  const handleClose = () => {
    if (esRef.current) {
      esRef.current.close()
    }
    setSpeedTesting(false)
    setSpeed(null)
    onClose()
  }

  return (
    <>
      <div 
        className="fixed inset-0 bg-black/40 z-40 transition-opacity backdrop-blur-sm"
        onClick={handleClose}
      />
      <div className="fixed inset-y-0 right-0 w-full max-w-md bg-base-100 shadow-2xl z-50 transform transition-transform duration-300 flex flex-col translate-x-0">
        
        {/* Header */}
        <div className="px-6 py-4 border-b border-base-200 flex justify-between items-center bg-base-200/50">
          <div>
            <h2 className="text-lg font-bold truncate max-w-[300px]">{node.name}</h2>
            <div className="text-sm opacity-60 flex items-center gap-2 mt-1">
              <span>{regionFlag(exitCountryCode)} 落地 {exitCountryCode.toUpperCase() || '未检测'}</span>
              <span>•</span>
              <span className="font-mono">{node.tag}</span>
            </div>
          </div>
          <button onClick={handleClose} className="btn btn-sm btn-ghost btn-square">
            ✕
          </button>
        </div>

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-6 space-y-8">
          
          {/* Section 1: Basic Network */}
          <section>
            <h3 className="text-md font-bold mb-4 flex items-center gap-2">
              <Globe2 className="h-4 w-4 text-primary" /> 基础网络信息
            </h3>
            {result || diagnostic?.detection ? (
              <div className="bg-base-200 rounded-xl p-4 space-y-3 text-sm font-mono">
                <div className="flex justify-between">
                  <span className="opacity-60">出口 IP</span>
                  <span className="font-bold">{detectedIP || '-'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">归属地</span>
                  <span>{regionFlag(diagnostic?.detection?.exit_country_code || ipApi?.country_code || result?.ip.iso_code || '')} {diagnostic?.detection?.exit_country || ipApi?.country || result?.ip.country || '未检测'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">ASN</span>
                  <span>{ipApi?.asn || result?.ip.asn || '-'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">ISP/Org</span>
                  <span className="text-right truncate max-w-[200px]">{ipApi?.org || ipApi?.isp || result?.ip.org || '-'}</span>
                </div>
                <div className="flex justify-between"><span className="opacity-60">检测延迟</span><span>{diagnostic?.detection?.latency_ms == null ? '未检测' : `${diagnostic.detection.latency_ms} ms`}</span></div>
                <div className="flex justify-between"><span className="opacity-60">平均 / 峰值</span><span className="text-right">{diagnostic?.detection?.average_bytes_per_second == null ? '未检测' : `${formatBytesSpeed(diagnostic.detection.average_bytes_per_second)} / ${formatBytesSpeed(diagnostic.detection.peak_bytes_per_second || 0)}`}</span></div>
                <div className="flex justify-between items-center">
                  <span className="opacity-60">IP 类型</span>
                  <div className="flex gap-2">
                    {result?.ip.ip_type && <span className="badge badge-sm badge-outline">{result.ip.ip_type}</span>}
                    {result?.ip.usage_type && <span className="badge badge-sm badge-outline">{result.ip.usage_type}</span>}
                    {!result?.ip.ip_type && !result?.ip.usage_type && <span className="text-xs opacity-50">以独立质量源结果为准</span>}
                  </div>
                </div>
              </div>
            ) : (
              <div className="text-sm opacity-50 text-center py-4 bg-base-200 rounded-xl">暂无检测结果</div>
            )}
          </section>

          {/* Section 2: Provider-specific IP quality */}
          <section>
            <h3 className="text-md font-bold mb-4 flex items-center gap-2">
              <ShieldCheck className="h-4 w-4 text-error" /> IP 质量（独立来源）
            </h3>
            {diagnostic?.quality?.length ? (
              <div className="space-y-3">
                {diagnostic.quality.map((quality) => (
                  <div key={quality.provider} className="rounded-xl bg-base-200 p-4 text-sm">
                    <div className="mb-3 flex items-center justify-between"><strong>{quality.provider}</strong><span className={`badge badge-sm ${quality.status === 'success' ? 'badge-success' : quality.status === 'partial' ? 'badge-warning' : quality.status === 'failed' ? 'badge-error' : 'badge-ghost'}`}>{quality.status === 'success' ? '完整' : quality.status === 'partial' ? '信息不全' : quality.status === 'failed' ? '失败' : quality.status === 'disabled' ? '未启用' : '未检测'}</span></div>
                    <div className="grid grid-cols-2 gap-2 text-xs">
                      <span className="text-base-content/55">住宅</span><span>{quality.is_residential == null ? '未检测' : quality.is_residential ? '是' : '否'}</span>
                      <span className="text-base-content/55">广播</span><span>{quality.is_broadcast == null ? '未检测' : quality.is_broadcast ? '是' : '否'}</span>
                      <span className="text-base-content/55">代理 / 托管</span><span>{quality.proxy == null && quality.hosting == null ? '未检测' : quality.proxy || quality.hosting ? '是' : '否'}</span>
                      <span className="text-base-content/55">欺诈分</span><span className="font-mono">{quality.fraud_score == null ? '未检测' : `${quality.fraud_score} · ${fraudGrade(quality.fraud_score)}`}</span>
                    </div>
                    {quality.reason && <p className="mt-2 text-xs text-error">{quality.reason}</p>}
                  </div>
                ))}
                {diagnostic.exit_ip_drift && <div role="alert" className="alert alert-warning py-2 text-xs">不同来源检测到的出口 IP 不一致，可能发生出口漂移。</div>}
              </div>
            ) : (
              <div className="text-sm opacity-50 text-center py-4 bg-base-200 rounded-xl">质量提供商未启用或尚未检测</div>
            )}
          </section>

          {/* Section 3: Entertainment & Services */}
          <section>
            <h3 className="text-md font-bold mb-4 flex items-center gap-2">
              <Tv className="h-4 w-4 text-secondary" /> 流媒体及 AI 服务
            </h3>
            {result ? (
              <div className="grid grid-cols-2 gap-3">
                {result.services.map(svc => (
                  <div key={svc.name} className="bg-base-200 rounded-xl p-3 flex flex-col justify-center items-center gap-1 text-center" title={svc.description}>
                    <span className="font-bold text-sm">{svc.display_name}</span>
                    <span className={`badge badge-sm ${
                      svc.status === 'unlocked' ? 'badge-success' :
                      svc.status === 'partial' ? 'badge-warning' :
                      svc.status === 'originals_only' ? 'badge-warning' :
                      svc.status === 'locked' ? 'badge-error' : 'badge-ghost'
                    }`}>
                      {svc.status === 'unlocked' ? '解锁' :
                       svc.status === 'partial' ? '部分可用' :
                       svc.status === 'originals_only' ? '仅自制' :
                       svc.status === 'locked' ? '未解锁' : '失败'}
                    </span>
                    {svc.region && svc.status === 'unlocked' && (
                      <span className="text-xs opacity-60 font-mono mt-1">{regionFlag(svc.region)} {svc.region}</span>
                    )}
                  </div>
                ))}
              </div>
            ) : (
              <div className="text-sm opacity-50 text-center py-4 bg-base-200 rounded-xl">暂无检测结果</div>
            )}
          </section>

        </div>

        {/* Footer - Speedtest */}
        <div className="p-4 border-t border-base-200 bg-base-200/50 space-y-3">
          <div className="flex justify-between items-center">
            <span className="text-sm font-bold opacity-70">下行测速 (Speedtest)</span>
            {(speed !== null || diagnostic?.detection?.average_bytes_per_second != null) && (
              <span className="text-right font-mono font-bold text-primary">
                <span className="block text-xl">{speed !== null ? `${speed.toFixed(2)} Mbps` : formatBytesSpeed(diagnostic?.detection?.average_bytes_per_second || 0)}</span>
                {speed == null && <span className="block text-xs font-normal opacity-60">{(((diagnostic?.detection?.average_bytes_per_second || 0) * 8) / 1_000_000).toFixed(2)} Mbps · 峰值 {formatBytesSpeed(diagnostic?.detection?.peak_bytes_per_second || 0)}</span>}
              </span>
            )}
          </div>
          
          {speedError && (
            <div className="text-error text-xs">{speedError}</div>
          )}

          <button 
            className="btn btn-primary w-full" 
            onClick={handleSpeedtest}
            disabled={speedTesting}
          >
            {speedTesting ? (
              <>
                <span className="loading loading-spinner loading-sm"></span>
                测速中...
              </>
            ) : '发起测速'}
          </button>
        </div>
      </div>
    </>
  )
}

function formatBytesSpeed(bytesPerSecond: number) {
  return `${(bytesPerSecond / 1024 / 1024).toFixed(2)} MB/s`
}

function fraudGrade(score: number) {
  if (score <= 10) return '极佳'
  if (score <= 30) return '优秀'
  if (score <= 50) return '良好'
  if (score <= 70) return '中等'
  if (score <= 89) return '差'
  return '极差'
}
