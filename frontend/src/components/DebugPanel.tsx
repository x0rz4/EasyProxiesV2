import { useEffect, useState } from 'react'
import { fetchDebug, streamDebugLogs } from '../api/client'
import type { DebugLogEvent, DebugNode, TimelineEvent } from '../types'
import { controlClass, PageContent, PageHeader, PageLayout } from './ui/PageLayout'
import { useQuery } from '@tanstack/react-query'
import { Terminal, RefreshCw } from 'lucide-react'

interface LogEntry {
  nodeTag: string
  nodeName: string
  event: TimelineEvent
}

const maxLogEntries = 1000

function logKey(log: LogEntry): string {
  return `${log.nodeTag}|${log.event.time}|${log.event.destination ?? ''}|${log.event.success}|${log.event.error ?? ''}`
}

function mergeLogs(current: LogEntry[], incoming: LogEntry[]): LogEntry[] {
  const merged = new Map(current.map((log) => [logKey(log), log]))
  for (const log of incoming) merged.set(logKey(log), log)
  return [...merged.values()]
    .sort((a, b) => new Date(b.event.time).getTime() - new Date(a.event.time).getTime())
    .slice(0, maxLogEntries)
}

function formatLogTime(value: string): string {
  const date = new Date(value)
  if (!value || Number.isNaN(date.getTime())) return '----/--/-- --:--:--'
  return date.toLocaleString('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  })
}

function LogMessage({ event }: { event: TimelineEvent }) {
  if (event.destination) {
    return <><span className="text-slate-400">connect </span><span className="text-cyan-300">{event.destination}</span>{event.success ? <span className="text-emerald-400"> succeeded</span> : <span className="text-red-400"> failed{event.error ? `: ${event.error}` : ''}</span>}</>
  }
  return <><span className="text-slate-400">probe </span>{event.success ? <span className="text-emerald-400">succeeded</span> : <span className="text-red-400">failed{event.error ? `: ${event.error}` : ''}</span>}{event.latency_ms > 0 && <span className="text-amber-300"> ({event.latency_ms}ms)</span>}</>
}

export default function DebugPanel() {
  const [nodes, setNodes] = useState<DebugNode[]>([])
  const [logs, setLogs] = useState<LogEntry[]>([])
  const [connected, setConnected] = useState(false)
  const [selectedNode, setSelectedNode] = useState('all')

  const { data: debugData, refetch: loadHistory, isFetching, error: queryError } = useQuery({
    queryKey: ['debugHistory'],
    queryFn: fetchDebug,
    refetchOnWindowFocus: false
  })

  useEffect(() => {
    if (!debugData) return
    const timer = window.setTimeout(() => {
      setNodes(debugData.nodes)
      const history = debugData.nodes.flatMap((node) => (node.timeline ?? []).map((event) => ({
        nodeTag: node.tag,
        nodeName: node.name || node.tag,
        event,
      })))
      setLogs((current) => mergeLogs(current, history))
    }, 0)
    return () => window.clearTimeout(timer)
  }, [debugData])

  useEffect(() => {
    const stream = streamDebugLogs((message: DebugLogEvent) => {
      const log = { nodeTag: message.node_tag, nodeName: message.node_name || message.node_tag, event: message.event }
      setLogs((current) => mergeLogs(current, [log]))
    }, setConnected)
    return () => stream.abort()
  }, [])

  const error = (queryError as Error)?.message || ''
  const loading = isFetching && logs.length === 0
  const visibleLogs = selectedNode === 'all' ? logs : logs.filter((log) => log.nodeTag === selectedNode)

  return (
    <PageLayout fill>
      <PageHeader
        sticky={false}
        title="调试面板"
        description="实时查看所有节点的运行日志"
        icon={<Terminal className="h-5 w-5" />}
        actions={<>
            <select className={`select select-sm w-28 sm:w-44 lg:select-md ${controlClass}`} value={selectedNode} onChange={(event) => setSelectedNode(event.target.value)} aria-label="筛选日志节点">
              <option value="all">全部节点 ({nodes.length})</option>
              {nodes.map((node) => <option key={node.tag} value={node.tag}>{node.name || node.tag}</option>)}
            </select>
            <button className="btn btn-primary btn-sm gap-2 lg:btn-md" onClick={() => void loadHistory()} disabled={isFetching} title="刷新历史日志" aria-label="刷新历史日志">
              {isFetching ? <span className="loading loading-spinner loading-xs" /> : <RefreshCw className="h-4 w-4" />}
              <span className="hidden sm:inline">刷新历史</span>
            </button>
          </>}
      />

      <PageContent fill>
        {error && <div role="alert" className="alert alert-error alert-soft mb-4 text-sm"><span>{error}</span></div>}
        <section className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-2xl border border-slate-700 bg-[#10141c] shadow-xl">
          <div className="flex items-center justify-between border-b border-slate-700/80 bg-[#171c26] px-4 py-3 text-xs text-slate-400">
            <div className="flex items-center gap-2"><span className="h-2.5 w-2.5 rounded-full bg-red-400" /><span className="h-2.5 w-2.5 rounded-full bg-amber-400" /><span className="h-2.5 w-2.5 rounded-full bg-emerald-400" /><span className="ml-2 font-mono text-slate-300">runtime.log</span></div>
            <span>{visibleLogs.length} 条日志</span>
          </div>
          <div className="flex-1 overflow-auto p-4 font-mono text-xs leading-6 sm:text-sm">
            {loading ? <div className="flex h-full items-center justify-center"><span className="loading loading-spinner text-primary" /></div> : visibleLogs.length === 0 ? <div className="flex h-full items-center justify-center text-slate-500">{selectedNode === 'all' ? '暂无运行日志' : '该节点暂无运行日志'}</div> : visibleLogs.map((log) => (
              <div key={logKey(log)} className="flex min-w-max gap-3 rounded px-1 hover:bg-white/5">
                <span className="select-none text-slate-600">{formatLogTime(log.event.time)}</span>
                <span className={log.event.success ? 'w-12 text-emerald-400' : 'w-12 text-red-400'}>{log.event.success ? 'INFO' : 'ERROR'}</span>
                <span className="w-40 truncate text-sky-300" title={log.nodeName}>[{log.nodeName}]</span>
                <span className="text-slate-200"><LogMessage event={log.event} /></span>
              </div>
            ))}
          </div>
          <div className="flex items-center justify-between border-t border-slate-700/80 bg-[#171c26] px-4 py-2 text-xs text-slate-500">
            <span>{selectedNode === 'all' ? '全部节点' : nodes.find((node) => node.tag === selectedNode)?.name || selectedNode}</span>
            <span className="flex items-center gap-2"><span className={`h-1.5 w-1.5 rounded-full ${connected ? 'animate-pulse bg-emerald-400' : 'bg-amber-400'}`} />{connected ? '实时流已连接' : '实时流重连中'}</span>
          </div>
        </section>
      </PageContent>
    </PageLayout>
  )
}
