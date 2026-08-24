/**
 * Map a region code (lowercase ISO 3166-1 alpha-2, or "other") to a flag emoji.
 *
 * The flag is computed from the regional indicator symbols, so every country
 * is supported without a hardcoded list. Unknown / non-2-letter codes fall back
 * to a globe.
 */
export function regionFlag(region?: string): string {
  if (!region) return '🌐'
  const code = region.toLowerCase()
  if (code === 'other') return '🌍'
  if (code.length !== 2) return '🌐'
  const base = 0x1f1e6 // regional indicator symbol A
  const a = 'a'.charCodeAt(0)
  const c1 = code.charCodeAt(0) - a + base
  const c2 = code.charCodeAt(1) - a + base
  if (c1 < base || c1 > base + 25 || c2 < base || c2 > base + 25) return '🌐'
  return String.fromCodePoint(c1, c2)
}
