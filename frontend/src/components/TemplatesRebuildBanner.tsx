import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { useAuth } from '@/hooks/useAuth'
import {
  bootstrapTemplates,
  getTemplatesStatus,
  sweepTemplates,
  type RemovedTemplate,
  type SweepResult,
  type TemplatesStatus,
} from '@/api/client'

// TemplatesRebuildBanner shows a sticky yellow strip at the top of the
// app when one or more node_templates rows point at a Proxmox template
// that's no longer baked (i.e. lacks the nimbus-baked-v1 tag set by
// bootstrap). That happens on a deployment that upgraded past the
// D-boot cidata rewrite without re-running bootstrap; provisioning
// from such a template fails at the verifyTemplateBaked guard with a
// clear "re-run bootstrap" message, but a banner is friendlier than
// waiting for the first provision attempt to fail.
//
// Admin-only — non-admins can't act on the banner (the bootstrap
// endpoint is admin-gated) so we hide it from them entirely rather
// than render something they can't dismiss.
export default function TemplatesRebuildBanner() {
  const { user } = useAuth()
  const [status, setStatus] = useState<TemplatesStatus | null>(null)
  const [running, setRunning] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [hidden, setHidden] = useState(false)
  // sweep flow: preview holds the dry-run result while the operator
  // reviews; sweeping=true gates buttons during the live destroy pass.
  const [preview, setPreview] = useState<SweepResult | null>(null)
  const [previewing, setPreviewing] = useState(false)
  const [sweeping, setSweeping] = useState(false)
  const [sweepError, setSweepError] = useState<string | null>(null)

  // Fetch on mount when we know the user is an admin. We don't poll —
  // the banner reappears on the next page navigation or refresh, which
  // is plenty for a once-per-upgrade prompt.
  useEffect(() => {
    if (!user?.is_admin) return
    let cancelled = false
    getTemplatesStatus()
      .then((s) => {
        if (!cancelled) setStatus(s)
      })
      .catch(() => {
        // Network errors leave status null → banner stays hidden.
        // Bootstrap status is admin-gated and best-effort UX; we'd
        // rather show nothing than a confusing error strip.
      })
    return () => {
      cancelled = true
    }
  }, [user?.is_admin])

  // Don't render anything until we have a verdict.
  if (!user?.is_admin || hidden || !status) return null
  if (status.unbaked === 0) return null

  const handleRebuild = async () => {
    if (running) return
    if (
      !window.confirm(
        `Rebuild ${status.unbaked} template${status.unbaked === 1 ? '' : 's'}? ` +
          'This downloads cloud images, runs the bake ceremony (install qemu-guest-agent, clean cloud-init state), ' +
          'and converts to template — typically 5–10 minutes per OS per node. Existing VMs keep running.'
      )
    ) {
      return
    }
    setRunning(true)
    setError(null)
    try {
      await bootstrapTemplates()
      const next = await getTemplatesStatus()
      setStatus(next)
      if (next.unbaked === 0) setHidden(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'rebuild failed')
    } finally {
      setRunning(false)
    }
  }

  // Sweep flow is two-pass: dry-run loads the preview into state and
  // opens the modal; confirming inside the modal calls back to runSweep
  // which fires the live destroy pass and refreshes the rebuild status.
  const openSweepPreview = async () => {
    if (previewing || sweeping) return
    setPreviewing(true)
    setSweepError(null)
    try {
      const dry = await sweepTemplates(true)
      setPreview(dry)
    } catch (err) {
      setSweepError(err instanceof Error ? err.message : 'preview failed')
    } finally {
      setPreviewing(false)
    }
  }

  const runSweep = async () => {
    if (sweeping) return
    setSweeping(true)
    setSweepError(null)
    try {
      await sweepTemplates(false)
      setPreview(null)
      // Templates may have flipped to "all baked" once duplicates are
      // gone — refresh the status so the banner self-dismisses when
      // there's nothing left to fix.
      const next = await getTemplatesStatus()
      setStatus(next)
      if (next.unbaked === 0) setHidden(true)
    } catch (err) {
      setSweepError(err instanceof Error ? err.message : 'sweep failed')
    } finally {
      setSweeping(false)
    }
  }

  return (
    <>
      <div
        role="alert"
        className="sticky top-[57px] z-40 border-b border-amber-300/60 bg-amber-50/90 backdrop-blur-md"
      >
        <div className="mx-auto max-w-[1440px] px-8 py-3 flex items-start gap-4">
          <div className="flex-1 min-w-0">
            <div className="text-sm font-medium text-amber-900">
              {status.unbaked} of {status.total} VM template
              {status.total === 1 ? '' : 's'} need rebuilding
            </div>
            <div className="text-xs text-amber-800/90 mt-0.5 leading-relaxed">
              Nimbus upgraded its template format — older templates lack the
              <code className="mx-1 px-1 py-0.5 rounded bg-amber-100 text-amber-900 font-mono text-[11px]">
                nimbus-baked-v1
              </code>
              tag and the pre-installed qemu-guest-agent that new provisions
              require. Existing VMs are unaffected; provisioning new ones from
              these templates will fail until you rebuild.
              {error && (
                <span className="block mt-1 text-red-700">
                  Rebuild error: {error}
                </span>
              )}
              {sweepError && (
                <span className="block mt-1 text-red-700">
                  Sweep error: {sweepError}
                </span>
              )}
            </div>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <button
              type="button"
              onClick={openSweepPreview}
              disabled={running || previewing || sweeping}
              className="px-3 py-1.5 rounded-md text-sm font-medium border border-amber-600/40 text-amber-900 bg-white/70 hover:bg-white disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
              title="Find duplicate, unbaked, and failed-bake template VMs and destroy them"
            >
              {previewing ? 'Scanning…' : 'Clean up extras'}
            </button>
            <button
              type="button"
              onClick={handleRebuild}
              disabled={running || sweeping}
              className="px-3 py-1.5 rounded-md text-sm font-medium bg-amber-600 text-white hover:bg-amber-700 disabled:bg-amber-300 disabled:cursor-not-allowed transition-colors"
            >
              {running ? 'Rebuilding…' : 'Rebuild now'}
            </button>
            <button
              type="button"
              onClick={() => setHidden(true)}
              disabled={running || sweeping}
              className="px-2 py-1.5 rounded-md text-xs text-amber-900/70 hover:text-amber-900 hover:bg-amber-100 disabled:opacity-50 transition-colors"
              title="Hide until next page refresh"
            >
              Dismiss
            </button>
          </div>
        </div>
      </div>
      {preview && (
        <SweepPreviewModal
          preview={preview}
          sweeping={sweeping}
          onCancel={() => setPreview(null)}
          onConfirm={runSweep}
        />
      )}
    </>
  )
}

// SweepPreviewModal renders the dry-run output so the operator can review
// exactly which template VMs the sweeper will destroy before confirming.
// Per-node grouped table; each removal carries vmid + name + reason chip.
function SweepPreviewModal({
  preview,
  sweeping,
  onCancel,
  onConfirm,
}: {
  preview: SweepResult
  sweeping: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  const nodesWithRemovals = preview.nodes.filter((n) => (n.removed?.length ?? 0) > 0)
  const total = preview.total_removed
  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label="Confirm template cleanup"
    >
      <div
        className="glass"
        style={{
          maxWidth: 720,
          width: '100%',
          maxHeight: 'calc(100vh - 2rem)',
          overflow: 'hidden',
          display: 'flex',
          flexDirection: 'column',
          padding: '24px 28px',
        }}
        onClick={(e) => e.stopPropagation()}
      >
        <div style={{ flex: '0 0 auto', marginBottom: 12 }}>
          <div className="eyebrow">Template cleanup</div>
          <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
            {total === 0 ? 'Nothing to clean up' : `Destroy ${total} template VM${total === 1 ? '' : 's'}?`}
          </h3>
          <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
            {total === 0
              ? 'No duplicate, unbaked, or failed-bake template VMs were found. Your nodes are tidy.'
              : 'These VMs are redundant or leftover from earlier bootstrap runs. The lowest-VMID baked template for each OS will be kept on every node; the rest get destroyed.'}
          </p>
        </div>

        {nodesWithRemovals.length > 0 && (
          <div style={{ flex: '1 1 auto', overflowY: 'auto', margin: '0 -28px', padding: '0 28px' }}>
            {nodesWithRemovals.map((n) => (
              <div key={n.node} style={{ margin: '10px 0' }}>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 4 }}>
                  <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>{n.node}</span>
                  <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>
                    {n.removed?.length ?? 0} to destroy · {Object.keys(n.kept ?? {}).length} kept
                  </span>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                  {n.removed?.map((r) => (
                    <RemovalRow key={r.vmid} row={r} />
                  ))}
                </div>
                {n.errors && n.errors.length > 0 && (
                  <div style={{ marginTop: 4, fontSize: 11, color: 'var(--err)' }}>
                    {n.errors.map((e, i) => <div key={i}>⚠ {e}</div>)}
                  </div>
                )}
              </div>
            ))}
          </div>
        )}

        <div style={{ flex: '0 0 auto', display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
          <button type="button" className="n-btn" onClick={onCancel} disabled={sweeping}>Cancel</button>
          {total > 0 && (
            <button
              type="button"
              className="n-btn"
              onClick={onConfirm}
              disabled={sweeping}
              style={{ borderColor: 'var(--err)', color: 'var(--err)' }}
            >
              {sweeping ? 'Cleaning up…' : `Destroy ${total} VM${total === 1 ? '' : 's'}`}
            </button>
          )}
        </div>
      </div>
    </div>,
    document.body,
  )
}

const REASON_LABEL: Record<RemovedTemplate['reason'], { label: string; color: string }> = {
  duplicate: { label: 'duplicate', color: '#9a5c2e' },
  unbaked_with_baked_sibling: { label: 'unbaked', color: '#b83a3a' },
  failed_bake_leftover: { label: 'failed bake', color: 'var(--ink-mute)' },
}

function RemovalRow({ row }: { row: RemovedTemplate }) {
  const reason = REASON_LABEL[row.reason]
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: '60px 1fr auto',
        gap: 10,
        fontSize: 12,
        padding: '3px 0',
        alignItems: 'center',
      }}
    >
      <code style={{ color: 'var(--ink-mute)' }}>#{row.vmid}</code>
      <span style={{ color: 'var(--ink)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {row.name || '(unnamed)'}
      </span>
      <span
        style={{
          fontSize: 10,
          padding: '2px 8px',
          borderRadius: 3,
          color: reason.color,
          background: 'rgba(20,18,28,0.05)',
          border: '1px solid var(--line)',
          textTransform: 'uppercase',
          letterSpacing: '0.04em',
          fontWeight: 500,
        }}
      >
        {reason.label}
      </span>
    </div>
  )
}
