import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { getProxmoxBinding } from '@/api/client'
import type { ProxmoxBinding } from '@/api/client'

// ProxmoxBindingChip lives in the navbar, between the top-level nav and
// the user dropdown. It surfaces "you are connected to <cluster>" so the
// operator never has to wonder which Proxmox endpoint Nimbus is talking
// to. Click → opens a small modal with version / node count / last-seen.
//
// When EPIC #195 (multi-hypervisor) lands the chip becomes a selector;
// the v1 here is read-only and matches the current single-cluster model.
//
// Polled every 30s — cheap (one /version + one /cluster/status round
// trip). On failure the chip flips to "offline" without throwing so the
// operator gets a clear "I can't reach Proxmox" signal.
export default function ProxmoxBindingChip() {
  const [binding, setBinding] = useState<ProxmoxBinding | null>(null)
  const [open, setOpen] = useState(false)

  useEffect(() => {
    let cancelled = false
    const tick = () => {
      getProxmoxBinding()
        .then((b) => { if (!cancelled) setBinding(b) })
        .catch(() => { /* keep last known state on error */ })
    }
    tick()
    const id = setInterval(tick, 30_000)
    return () => { cancelled = true; clearInterval(id) }
  }, [])

  // Display label: prefer cluster name (operator-meaningful), fall back
  // to the apex hostname extracted from the API URL. Empty string while
  // the first poll is in flight — render a placeholder so the chip width
  // doesn't pop.
  const label = binding ? (binding.cluster_name || hostFromURL(binding.host) || '—') : '…'
  const reachable = binding?.reachable !== false

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="flex items-center gap-1.5 px-2.5 py-1 rounded-md border text-[11px] font-mono uppercase tracking-widest cursor-pointer transition-colors"
        style={{
          color: reachable ? 'var(--ink-2)' : 'var(--err)',
          borderColor: reachable ? 'var(--line-strong)' : 'rgba(184,55,55,0.3)',
          background: reachable ? 'rgba(20,18,28,0.03)' : 'rgba(184,55,55,0.05)',
        }}
        title={reachable
          ? `Proxmox: ${binding?.host ?? 'loading'}`
          : 'Proxmox unreachable — last contact stale'}
      >
        <span
          aria-hidden="true"
          style={{
            width: 6, height: 6, borderRadius: 3,
            background: reachable ? 'var(--ok)' : 'var(--err)',
          }}
        />
        <span>{label}</span>
      </button>
      {open && binding && (
        <BindingModal binding={binding} onClose={() => setOpen(false)} />
      )}
    </>
  )
}

// hostFromURL strips scheme + leftmost label so https://router.altsuite.co
// renders as "altsuite.co". Returns the original string when parsing
// fails so the chip never blows up on a weird host config.
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

function BindingModal({ binding, onClose }: { binding: ProxmoxBinding; onClose: () => void }) {
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
          {binding.cluster_name || hostFromURL(binding.host) || 'Proxmox'}
        </h3>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 10, fontSize: 13 }}>
          <Row label="Host" value={binding.host} mono />
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

        <p style={{ margin: '18px 0 0', fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
          Manage cluster nodes (cordon, drain, remove) from the{' '}
          <a href="/nodes" className="n-link">Nodes page</a>.
        </p>
      </div>
    </div>,
    document.body,
  )
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
