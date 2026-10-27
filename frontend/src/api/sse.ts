import { ApiError, authFetch, readErrorMessage } from './transport'

export interface StreamEvent { type: string; data: string }
const maxEventSize = 16 * 1024 * 1024

// Preserve framing across arbitrary UTF-8 chunks, including split CRLF and
// multi-line data. Comments/heartbeats never become application events.
export class EventStreamDecoder {
  private buffer = ''
  private data: string[] = []
  private type = 'message'
  private size = 0

  feed(text: string): StreamEvent[] {
    this.buffer += text
    const events: StreamEvent[] = []
    while (true) {
      const end = this.buffer.search(/[\r\n]/)
      if (end < 0 || (end === this.buffer.length - 1 && this.buffer[end] === '\r')) break
      const line = this.buffer.slice(0, end)
      const width = this.buffer.slice(end, end + 2) === '\r\n' ? 2 : 1
      this.buffer = this.buffer.slice(end + width)
      this.size += line.length
      if (this.size > maxEventSize) throw new ApiError('流事件过大，请缩小检测范围', 413)
      if (!line) {
        if (this.data.length) events.push({ type: this.type, data: this.data.join('\n') })
        this.data = []
        this.type = 'message'
        this.size = 0
      } else {
        const colon = line.indexOf(':')
        const field = colon < 0 ? line : line.slice(0, colon)
        const value = colon < 0 ? '' : line.slice(colon + (line[colon + 1] === ' ' ? 2 : 1))
        if (field === 'data') this.data.push(value)
        if (field === 'event') this.type = value || 'message'
      }
    }
    if (this.size + this.buffer.length > maxEventSize) throw new ApiError('流事件过大，请缩小检测范围', 413)
    return events
  }
}

export async function* readEvents(path: string, controller: AbortController, method = 'GET', onOpen?: () => void): AsyncGenerator<StreamEvent> {
  const response = await authFetch(path, { method, signal: controller.signal, headers: { Accept: 'text/event-stream' } })
  if (!response.ok) throw new ApiError(await readErrorMessage(response), response.status)
  if (!response.body || !response.headers.get('Content-Type')?.toLowerCase().startsWith('text/event-stream')) {
    await response.body?.cancel()
    throw new ApiError('服务器未返回事件流', 502)
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  const frames = new EventStreamDecoder()
  const onAbort = () => { void reader.cancel().catch(() => {}) }
  controller.signal.addEventListener('abort', onAbort, { once: true })
  try {
    onOpen?.()
    while (!controller.signal.aborted) {
      const { done, value } = await reader.read()
      if (done) break
      for (const event of frames.feed(decoder.decode(value, { stream: true }))) {
        if (controller.signal.aborted) return
        yield event
      }
    }
  } finally {
    controller.signal.removeEventListener('abort', onAbort)
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}

export function parseStreamJSON<T>(data: string): T | undefined {
  try { return JSON.parse(data) as T } catch { return undefined }
}

export function waitForReconnect(signal: AbortSignal, ms: number): Promise<void> {
  return new Promise(resolve => {
    if (signal.aborted) { resolve(); return }
    const onAbort = () => { clearTimeout(timer); resolve() }
    const timer = setTimeout(() => { signal.removeEventListener('abort', onAbort); resolve() }, ms)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}
