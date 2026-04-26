import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'

type Props = {
  vm: {
    id: number
    hostname: string
    ip: string
    vmid: number
    node: string
  }
  onConfirm: (id: number) => Promise<void>
  onCancel: () => void
  onDeleted: () => void
}

export default function DeleteVMConfirm({ vm, onConfirm, onCancel, onDeleted }: Props) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onCancel()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [busy, onCancel])

  const handleConfirm = async () => {
    setBusy(true)
    setError(null)
    try {
      await onConfirm(vm.id)
      onDeleted()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'delete failed')
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={() => !busy && onCancel()}
      role="dialog"
      aria-modal="true"
      aria-label={`Delete ${vm.hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[480px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Delete machine</div>
        <h3 className="text-2xl mt-1 mb-4">Destroy {vm.hostname}?</h3>
        <p className="text-sm text-ink-2 leading-relaxed mb-5">
          This stops the VM, removes it from the cluster, and releases its IP.
          The action can't be undone.
        </p>
        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 font-mono text-[11px] mb-7 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2">
          <span className="text-ink-3 uppercase tracking-wider">IP</span>
          <span className="text-ink">{vm.ip || '—'}</span>
          <span className="text-ink-3 uppercase tracking-wider">VMID</span>
          <span className="text-ink">{vm.vmid}</span>
          <span className="text-ink-3 uppercase tracking-wider">Node</span>
          <span className="text-ink">{vm.node}</span>
        </div>
        {error && (
          <div className="mb-5 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
            {error}
          </div>
        )}
        <div className="flex justify-end gap-3">
          <Button variant="ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <button
            type="button"
            onClick={handleConfirm}
            disabled={busy}
            className="px-4 py-2.5 rounded-[10px] bg-bad text-white font-mono text-xs tracking-wide hover:opacity-90 transition-opacity disabled:opacity-50 disabled:cursor-wait"
          >
            {busy ? 'DELETING…' : 'YES, DELETE'}
          </button>
        </div>
      </Card>
    </div>,
    document.body,
  )
}
