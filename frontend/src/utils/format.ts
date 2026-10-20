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