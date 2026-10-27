// Preferences must never make the application unusable when browser storage
// is disabled, full, or restricted by a privacy policy.
export function readPreference(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}

export function writePreference(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* use the in-memory value */ }
}
