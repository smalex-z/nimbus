import { useEffect, useState } from 'react'
import { useAuth } from '@/hooks/useAuth'
import {
  bootstrapTemplates,
  getTemplatesStatus,
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

  return (
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
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <button
            type="button"
            onClick={handleRebuild}
            disabled={running}
            className="px-3 py-1.5 rounded-md text-sm font-medium bg-amber-600 text-white hover:bg-amber-700 disabled:bg-amber-300 disabled:cursor-not-allowed transition-colors"
          >
            {running ? 'Rebuilding…' : 'Rebuild now'}
          </button>
          <button
            type="button"
            onClick={() => setHidden(true)}
            disabled={running}
            className="px-2 py-1.5 rounded-md text-xs text-amber-900/70 hover:text-amber-900 hover:bg-amber-100 disabled:opacity-50 transition-colors"
            title="Hide until next page refresh"
          >
            Dismiss
          </button>
        </div>
      </div>
    </div>
  )
}
