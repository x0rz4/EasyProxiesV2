/**
 * Format a byte count into a human-readable string (e.g., "1.5 GB").
 */
export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  if (bytes < 0) return '-'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  const idx = Math.min(i, sizes.length - 1)
  return `${(bytes / Math.pow(k, idx)).toFixed(idx === 0 ? 0 : 1)} ${sizes[idx]}`
}

/**
 * Format a speed in bytes/sec into a human-readable string (e.g., "2.3 MB/s").
 */
export function formatSpeed(bytesPerSec: number): string {
  if (bytesPerSec <= 0) return '0 B/s'
  return `${formatBytes(bytesPerSec)}/s`
}

/**
 * Format an ISO timestamp as a short relative-time string (e.g., "3 分钟前").
 * Returns '' for invalid or empty input. Future timestamps render as "刚刚".
 */
export function formatRelative(value?: string): string {
  if (!value) return ''
  const date = new Date(value)
  const t = date.getTime()
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  if (diff < 0) return '刚刚'
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return '刚刚'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min} 分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr} 小时前`
  const day = Math.floor(hr / 24)
  if (day < 30) return `${day} 天前`
  return date.toLocaleDateString('zh-CN')
}