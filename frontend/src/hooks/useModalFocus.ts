import { useEffect, useEffectEvent, useRef } from 'react'

const focusable = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'

export default function useModalFocus(onClose: () => void) {
  const root = useRef<HTMLDivElement>(null)
  const close = useEffectEvent(onClose)
  useEffect(() => {
    const dialog = root.current
    if (!dialog) return
    const previous = document.activeElement as HTMLElement | null
    const controls = () => [...dialog.querySelectorAll<HTMLElement>(focusable)].filter(element => !element.matches(':disabled') && element.getClientRects().length > 0)
    const firstInput = controls().find(element => element.matches('input,select,textarea'))
    ;(firstInput ?? controls()[0] ?? dialog).focus({ preventScroll: true })
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.stopPropagation(); close(); return }
      if (event.key !== 'Tab') return
      const items = controls()
      const first = items[0], last = items.at(-1)
      if (!first) { event.preventDefault(); dialog.focus(); return }
      if (event.shiftKey && (document.activeElement === first || document.activeElement === dialog)) {
        event.preventDefault(); last?.focus()
      } else if (!event.shiftKey && (document.activeElement === last || document.activeElement === dialog)) {
        event.preventDefault(); first.focus()
      }
    }
    dialog.addEventListener('keydown', onKeyDown)
    return () => {
      dialog.removeEventListener('keydown', onKeyDown)
      if (previous?.isConnected) previous.focus({ preventScroll: true })
    }
  }, [])
  return root
}
