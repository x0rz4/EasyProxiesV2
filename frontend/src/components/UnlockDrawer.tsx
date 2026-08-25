import { useState, useRef } from 'react'
import type { NodeSnapshot, UnlockResult } from '../types'
import { regionFlag } from '../utils/region'

interface UnlockDrawerProps {
  node: NodeSnapshot | null
  result: UnlockResult | null
  isOpen: boolean
  onClose: () => void
}

export default function UnlockDrawer({ node, result, isOpen, onClose }: UnlockDrawerProps) {
  const [speed, setSpeed] = useState<number | null>(null)
  const [speedTesting, setSpeedTesting] = useState(false)
  const [speedError, setSpeedError] = useState('')
  const esRef = useRef<EventSource | null>(null)

  if (!isOpen || !node) return null

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
      const errMessage = (e as any).data ? (e as any).data : '测速连接断开'
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
              <span>{regionFlag(node.region || '')} {node.region?.toUpperCase() || '未知'}</span>
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
              <span className="text-primary">🌐</span> 基础网络信息
            </h3>
            {result ? (
              <div className="bg-base-200 rounded-xl p-4 space-y-3 text-sm font-mono">
                <div className="flex justify-between">
                  <span className="opacity-60">出口 IP</span>
                  <span className="font-bold">{result.ip.ip || '-'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">归属地</span>
                  <span>{regionFlag(result.ip.iso_code || '')} {result.ip.country || '-'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">ASN</span>
                  <span>{result.ip.asn || '-'}</span>
                </div>
                <div className="flex justify-between">
                  <span className="opacity-60">ISP/Org</span>
                  <span className="text-right truncate max-w-[200px]">{result.ip.org || '-'}</span>
                </div>
                <div className="flex justify-between items-center">
                  <span className="opacity-60">IP 类型</span>
                  <div className="flex gap-2">
                    {result.ip.pure && <span className="badge badge-sm badge-success">原生 IP</span>}
                    {result.ip.ip_type && <span className="badge badge-sm badge-outline">{result.ip.ip_type}</span>}
                    {result.ip.usage_type && <span className="badge badge-sm badge-outline">{result.ip.usage_type}</span>}
                  </div>
                </div>
              </div>
            ) : (
              <div className="text-sm opacity-50 text-center py-4 bg-base-200 rounded-xl">暂无检测结果</div>
            )}
          </section>

          {/* Section 2: Security Radar */}
          <section>
            <h3 className="text-md font-bold mb-4 flex items-center gap-2">
              <span className="text-error">🛡️</span> 安全雷达
            </h3>
            {result ? (
              <div className="bg-base-200 rounded-xl p-4 space-y-4 text-sm">
                <div>
                  <div className="flex justify-between mb-1">
                    <span>欺诈分数 (Fraud Score)</span>
                    <span className="font-bold font-mono">{result.ip.fraud_score ?? 0}</span>
                  </div>
                  <progress 
                    className={`progress w-full ${result.ip.fraud_score && result.ip.fraud_score > 50 ? 'progress-error' : 'progress-success'}`} 
                    value={result.ip.fraud_score ?? 0} 
                    max="100"
                  />
                </div>
                <div className="flex justify-between items-center">
                  <span>风险等级</span>
                  <span className={`badge ${
                    result.ip.risk_level === 'High' ? 'badge-error' :
                    result.ip.risk_level === 'Medium' ? 'badge-warning' :
                    result.ip.risk_level === 'Low' ? 'badge-success' : 'badge-ghost'
                  }`}>{result.ip.risk_level || '未知'}</span>
                </div>
              </div>
            ) : (
              <div className="text-sm opacity-50 text-center py-4 bg-base-200 rounded-xl">暂无检测结果</div>
            )}
          </section>

          {/* Section 3: Entertainment & Services */}
          <section>
            <h3 className="text-md font-bold mb-4 flex items-center gap-2">
              <span className="text-secondary">🎬</span> 流媒体及 AI 服务
            </h3>
            {result ? (
              <div className="grid grid-cols-2 gap-3">
                {result.services.map(svc => (
                  <div key={svc.name} className="bg-base-200 rounded-xl p-3 flex flex-col justify-center items-center gap-1 text-center">
                    <span className="font-bold text-sm">{svc.display_name}</span>
                    <span className={`badge badge-sm ${
                      svc.status === 'unlocked' ? 'badge-success' :
                      svc.status === 'originals_only' ? 'badge-warning' :
                      svc.status === 'locked' ? 'badge-error' : 'badge-ghost'
                    }`}>
                      {svc.status === 'unlocked' ? '解锁' :
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
            {speed !== null && (
              <span className="text-xl font-mono font-bold text-primary">
                {speed.toFixed(2)} <span className="text-sm font-normal opacity-60">Mbps</span>
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
