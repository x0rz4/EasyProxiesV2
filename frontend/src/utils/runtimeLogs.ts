import type { TimelineEvent } from '../types'

export interface LogEntry {
  nodeTag: string
  nodeName: string
  event: TimelineEvent
  key: string
  time: number
}

export const maxLogEntries = 1000

export function runtimeLog(nodeTag: string, nodeName: string, event: TimelineEvent): LogEntry {
  const time = Date.parse(event.time)
  return { nodeTag, nodeName, event, time: Number.isFinite(time) ? time : 0,
    key: `${nodeTag}|${event.time}|${event.destination ?? ''}|${event.success}|${event.error ?? ''}` }
}

export function mergeLogs(current: LogEntry[], incoming: LogEntry[]): LogEntry[] {
  const merged = new Map(current.map(log => [log.key, log]))
  for (const log of incoming) merged.set(log.key, log)
  return [...merged.values()].sort((a, b) => b.time - a.time).slice(0, maxLogEntries)
}

// A burst overwrites the oldest pending arrivals without repeatedly allocating
// a 1000-element slice. Draining releases every stored reference.
export function createLogBuffer() {
  const slots: (LogEntry | undefined)[] = new Array(maxLogEntries)
  let head = 0, length = 0
  return {
    push(log: LogEntry) {
      slots[(head + length) % maxLogEntries] = log
      if (length === maxLogEntries) head = (head + 1) % maxLogEntries
      else length++
    },
    drain(): LogEntry[] {
      const batch: LogEntry[] = []
      for (let i = 0; i < length; i++) {
        const slot = (head + i) % maxLogEntries
        batch.push(slots[slot]!)
        slots[slot] = undefined
      }
      head = length = 0
      return batch
    },
  }
}
