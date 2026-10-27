import { useEffect } from 'react'

export function allowNavigation(): boolean {
  return window.dispatchEvent(new Event('app:before-navigate', { cancelable: true }))
}

export default function useUnsavedChanges(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return
    const unload = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = '' }
    const navigate = (event: Event) => { if (!window.confirm('有未保存的修改，确认放弃并离开？')) event.preventDefault() }
    window.addEventListener('beforeunload', unload)
    window.addEventListener('app:before-navigate', navigate)
    return () => {
      window.removeEventListener('beforeunload', unload)
      window.removeEventListener('app:before-navigate', navigate)
    }
  }, [dirty])
}
