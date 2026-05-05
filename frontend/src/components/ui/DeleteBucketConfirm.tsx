import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'

type BucketSummary = {
  name: string
  objectCount: number
  totalSizeBytes: number
  ownerName?: string
  ownerEmail?: string
  createdAt?: string
}

type Props = {
  bucket: BucketSummary
  // forceDelete=true means the modal warns about object data loss even
  // when the bucket isn't empty (admin path). When false, the caller is
  // expected to refuse non-empty buckets server-side; the modal just
  // confirms the user wanted to delete an empty bucket.
  forceDelete?: boolean
  onConfirm: () => Promise<void>
  onCancel: () => void
  onDeleted: () => void
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}

// DeleteBucketConfirm mirrors DeleteS3Confirm — same scaffold, escape +
// click-outside cancel, busy spinner on the destructive button. Used by
// both the admin /s3 cross-user list (force-delete) and the per-user
// /buckets list (refuses non-empty server-side).
export default function DeleteBucketConfirm({
  bucket,
  forceDelete = false,
  onConfirm,
  onCancel,
  onDeleted,
}: Props) {
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

  const isEmpty = bucket.objectCount === 0
  const eyebrow = forceDelete ? 'Force-delete bucket' : 'Delete bucket'
  const headline = forceDelete && !isEmpty ? 'Empty and delete this bucket?' : 'Delete this bucket?'

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={() => !busy && onCancel()}
      role="dialog"
      aria-modal="true"
      aria-label={eyebrow}
    >
      <Card
        strong
        className="w-full max-w-[480px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">{eyebrow}</div>
        <h3 className="text-2xl mt-1 mb-4">{headline}</h3>
        <p className="text-sm text-ink-2 leading-relaxed mb-5">
          {forceDelete && !isEmpty ? (
            <>
              This bucket holds <strong>{bucket.objectCount} object{bucket.objectCount === 1 ? '' : 's'}</strong>{' '}
              ({formatBytes(bucket.totalSizeBytes)}). Force-deleting empties
              the bucket and removes it. Object data is gone permanently —
              there is no backup.
            </>
          ) : isEmpty ? (
            <>The bucket is empty. Removing it is a clean operation, but the name becomes available for someone else to claim.</>
          ) : (
            <>
              The bucket holds <strong>{bucket.objectCount} object{bucket.objectCount === 1 ? '' : 's'}</strong>{' '}
              ({formatBytes(bucket.totalSizeBytes)}). The server will refuse this delete unless you empty the bucket first.
            </>
          )}
        </p>
        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 font-mono text-[11px] mb-7 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2">
          <span className="text-ink-3 uppercase tracking-wider">Bucket</span>
          <span className="text-ink break-all">{bucket.name}</span>
          {bucket.ownerName && (
            <>
              <span className="text-ink-3 uppercase tracking-wider">Owner</span>
              <span className="text-ink break-all">
                {bucket.ownerName}
                {bucket.ownerEmail ? ` · ${bucket.ownerEmail}` : ''}
              </span>
            </>
          )}
          <span className="text-ink-3 uppercase tracking-wider">Objects</span>
          <span className="text-ink">{bucket.objectCount}</span>
          <span className="text-ink-3 uppercase tracking-wider">Size</span>
          <span className="text-ink">{formatBytes(bucket.totalSizeBytes)}</span>
          {bucket.createdAt && (
            <>
              <span className="text-ink-3 uppercase tracking-wider">Created</span>
              <span className="text-ink">{new Date(bucket.createdAt).toLocaleDateString()}</span>
            </>
          )}
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
