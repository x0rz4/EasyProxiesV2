import type { ReactNode } from 'react'
import useModalFocus from '../../hooks/useModalFocus'

export default function Modal({ label, onClose, children }: { label: string; onClose: () => void; children: ReactNode }) {
  const root = useModalFocus(onClose)
  return <div ref={root} tabIndex={-1} className="modal modal-open" role="dialog" aria-modal="true" aria-label={label}>{children}</div>
}
