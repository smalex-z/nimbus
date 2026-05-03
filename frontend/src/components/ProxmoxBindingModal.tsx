import { useEffect } from 'react'
import { createPortal } from 'react-dom'
import type { ProxmoxBinding } from '@/api/client'

// ProxmoxBindingModal renders the cluster-binding detail modal — version,
// node count, last contact, reachability. Mounted by callers that already
// have a ProxmoxBinding in hand; doesn't poll itself, so it can attach to
// any view that's already polling /api/proxmox/binding.
//
// Originally lived inside a navbar chip. The chip got demoted (the
// information lives in the Nodes page header now), but the modal stayed
// intact since it's a useful "click to see everything" affordance.
export default function ProxmoxBindingModal({
  binding,
  onClose,
}: {
  binding: ProxmoxBinding
  onClose: () => void
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label="Proxmox cluster binding"
      onClick={onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 460, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Connected to</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 14px' }}>
          {binding.connected_node || binding.cluster_name || hostFromURL(binding.host) || 'Proxmox'}
        </h3>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 10, fontSize: 13 }}>
          <Row label="API endpoint" value={binding.host} mono />
          <Row label="Connected node" value={binding.connected_node || '— (single-node)'} mono />
          <Row label="Cluster" value={binding.cluster_name || '— (single-node)'} />
          <Row label="Version" value={binding.version || '—'} mono />
          <Row label="Nodes" value={String(binding.node_count)} />
          <Row label="Last contact" value={binding.last_seen ? formatRelative(binding.last_seen) : 'never'} />
          <Row
            label="Status"
            value={binding.reachable ? 'reachable' : 'unreachable'}
            valueColor={binding.reachable ? 'var(--ok)' : 'var(--err)'}
          />
        </div>
      </div>
    </div>,
    document.body,
  )
}

// hostFromURL strips scheme + leftmost label so https://router.altsuite.co
// renders as "altsuite.co". Returns the original string when parsing
// fails so the modal never blows up on a weird host config.
function hostFromURL(raw: string): string {
  try {
    const u = new URL(raw)
    const parts = u.hostname.split('.')
    if (parts.length > 2) return parts.slice(1).join('.')
    return u.hostname
  } catch {
    return raw
  }
}

function Row({ label, value, mono = false, valueColor }: {
  label: string
  value: string
  mono?: boolean
  valueColor?: string
}) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 12 }}>
      <span style={{ fontSize: 11, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em' }}>
        {label}
      </span>
      <span
        style={{
          fontSize: 13,
          color: valueColor || 'var(--ink)',
          fontFamily: mono ? 'Geist Mono, monospace' : undefined,
          textAlign: 'right',
          wordBreak: 'break-all',
        }}
      >
        {value}
      </span>
    </div>
  )
}

function formatRelative(iso: string): string {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return iso
  const ms = Date.now() - t
  if (ms < 60_000) return 'just now'
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`
  return new Date(t).toLocaleString()
}
