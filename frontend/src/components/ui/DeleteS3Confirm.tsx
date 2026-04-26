import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import type { S3StorageView } from '@/api/client'

type Props = {
  storage: S3StorageView
  onConfirm: () => Promise<void>
  onCancel: () => void
  onDeleted: () => void
}

// DeleteS3Confirm mirrors DeleteVMConfirm — same modal scaffold, escape
// to cancel, click outside to cancel, busy spinner on the destructive
// button. Spelled out as a separate component (rather than reusing
// DeleteVMConfirm with a faked vm prop) because the metadata grid here
// is storage-specific (Endpoint + Disk vs IP).
export default function DeleteS3Confirm({ storage, onConfirm, onCancel, onDeleted }: Props) {
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
      await onConfirm()
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
      aria-label="Delete S3 storage"
    >
      <Card
        strong
        className="w-full max-w-[480px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Delete S3 storage</div>
        <h3 className="text-2xl mt-1 mb-4">Tear down MinIO?</h3>
        <p className="text-sm text-ink-2 leading-relaxed mb-5">
          This destroys the storage VM, releases its IP, and removes every
          bucket and object on it. The action can't be undone — there is no
          backup. Re-deploying gives you a fresh, empty MinIO instance.
        </p>
        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 font-mono text-[11px] mb-7 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2">
          <span className="text-ink-3 uppercase tracking-wider">Endpoint</span>
          <span className="text-ink break-all">{storage.endpoint || '—'}</span>
          <span className="text-ink-3 uppercase tracking-wider">VMID</span>
          <span className="text-ink">{storage.vmid}</span>
          <span className="text-ink-3 uppercase tracking-wider">Node</span>
          <span className="text-ink">{storage.node}</span>
          <span className="text-ink-3 uppercase tracking-wider">Disk</span>
          <span className="text-ink">{storage.disk_gb} GB</span>
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
