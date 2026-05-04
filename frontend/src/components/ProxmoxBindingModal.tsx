import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { changeProxmoxBinding, discoverProxmoxAdmin } from '@/api/client'
import type { DiscoveredEndpoint, ProxmoxBinding, ProxmoxDiscovery } from '@/api/client'

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

// ChangeBindingModal collects new Proxmox credentials, probes them
// server-side (8 s timeout), persists to the env file, and triggers a
// process restart. The page reloads after a short delay so the new
// connection is in effect when the operator returns.
//
// Why restart instead of in-process swap: every consumer of the live
// proxmox.Client (provision, ippool, nodemgr, …) holds the pointer at
// construction time. Hot-swapping would require surgery in many places
// and silent behaviour changes; the install wizard's same-shape flow
// has been the convention since v1, so this matches.
export function ChangeBindingModal({
  current,
  onClose,
}: {
  current: ProxmoxBinding
  onClose: () => void
}) {
  const [host, setHost] = useState(current.host)
  // Token ID is pre-filled from current binding (it's the user@realm!
  // tokenname half — not secret, just identifies which token slot we
  // use). Default falls back to the wizard's `root@pam!nimbus` so a
  // fresh install doesn't make the operator type it from scratch.
  const [tokenID, setTokenID] = useState(current.token_id || 'root@pam!nimbus')
  // Token secret is genuinely write-only — we never round-trip it from
  // the server. Empty submission means "keep the current secret" (the
  // common case: switching entry nodes within the same cluster, since
  // Proxmox tokens are cluster-wide). Operator only fills it in when
  // actually rotating the token.
  const [tokenSecret, setTokenSecret] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [restarting, setRestarting] = useState(false)
  const [showAdvanced, setShowAdvanced] = useState(false)

  // Discovery state — populates the "detected" pill row beneath the URL
  // field. Auto-fired on open; cheap (corosync read + LAN TLS scan,
  // capped at 6 s).
  const [discovery, setDiscovery] = useState<ProxmoxDiscovery | null>(null)
  const [discovering, setDiscovering] = useState(true)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !busy) onClose() }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose, busy])

  useEffect(() => {
    let cancelled = false
    discoverProxmoxAdmin()
      .then((d) => { if (!cancelled) setDiscovery(d) })
      .catch(() => { /* silent — pills just don't show */ })
      .finally(() => { if (!cancelled) setDiscovering(false) })
    return () => { cancelled = true }
  }, [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await changeProxmoxBinding({
        proxmox_host: host.trim(),
        proxmox_token_id: tokenID.trim(),
        proxmox_token_secret: tokenSecret.trim(),
      })
      // Server queues a 500 ms-delayed restart. Wait a beat longer
      // before reloading so the fresh process is up to serve us.
      setRestarting(true)
      setTimeout(() => window.location.reload(), 3000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label="Change Proxmox connection"
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 540, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Change Proxmox connection</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
          {current.connected_node || current.cluster_name || 'Reconfigure'}
        </h3>
        <p style={{ margin: '0 0 18px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Nimbus will probe the new credentials, persist them to <code>/etc/nimbus/nimbus.env</code>,
          and reload itself. The page refreshes automatically.
        </p>

        {restarting ? (
          <div
            style={{
              padding: '14px 16px',
              background: 'rgba(20,18,28,0.04)',
              border: '1px solid var(--line)',
              borderRadius: 10,
              fontSize: 13,
              color: 'var(--ink-body)',
            }}
          >
            Reloading Nimbus with the new connection… page will refresh in a moment.
          </div>
        ) : (
          <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="n-field">
              <label className="n-label" htmlFor="px-host">Proxmox API URL</label>
              <input
                id="px-host"
                className="n-input"
                type="url"
                placeholder="https://pve.example.com:8006"
                value={host}
                onChange={(e) => setHost(e.target.value)}
                required
                autoFocus
              />
              <DiscoveryPills
                discovering={discovering}
                endpoints={discovery?.endpoints || []}
                selectedURL={host}
                onPick={setHost}
              />
            </div>

            <div className="n-field">
              <label className="n-label" htmlFor="px-token-secret">
                Token secret <span style={{ fontWeight: 400, color: 'var(--ink-mute)' }}>(optional)</span>
              </label>
              <input
                id="px-token-secret"
                className="n-input"
                type="password"
                placeholder="leave blank to keep the current secret"
                value={tokenSecret}
                onChange={(e) => setTokenSecret(e.target.value)}
                autoComplete="off"
              />
              <span style={{ fontSize: 11, color: 'var(--ink-mute)', marginTop: 4 }}>
                Proxmox tokens are cluster-wide, so switching to a different node usually
                doesn't need a new secret. Only fill this in when you're actually rotating
                the token — Nimbus keeps the current one when this field is blank.
              </span>
            </div>

            <button
              type="button"
              onClick={() => setShowAdvanced((v) => !v)}
              style={{
                fontSize: 11, fontFamily: 'Geist Mono, monospace',
                color: 'var(--ink-mute)', textTransform: 'uppercase',
                letterSpacing: '0.06em', alignSelf: 'flex-start',
                background: 'none', border: 'none', cursor: 'pointer', padding: 0,
              }}
            >
              {showAdvanced ? '▼ Hide' : '▶ Show'} advanced (token ID)
            </button>
            {showAdvanced && (
              <div className="n-field">
                <label className="n-label" htmlFor="px-token-id">Token ID</label>
                <input
                  id="px-token-id"
                  className="n-input"
                  type="text"
                  placeholder="root@pam!nimbus"
                  value={tokenID}
                  onChange={(e) => setTokenID(e.target.value)}
                  required
                  autoComplete="off"
                />
                <span style={{ fontSize: 11, color: 'var(--ink-mute)', marginTop: 4 }}>
                  Format: <code>user@realm!tokenname</code>. The default <code>root@pam!nimbus</code>
                  is what the install wizard creates.
                </span>
              </div>
            )}

            {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
              <button type="button" className="n-btn" onClick={onClose} disabled={busy}>Cancel</button>
              <button
                type="submit"
                className="n-btn n-btn-primary"
                disabled={busy || !host || !tokenID}
              >
                {busy ? 'Probing…' : 'Save & reload'}
              </button>
            </div>
          </form>
        )}
      </div>
    </div>,
    document.body,
  )
}

// DiscoveryPills renders the row of "detected" Proxmox endpoints under
// the URL field. Each pill shows the node name + IP when known (corosync
// or TLS-cert CN), or just IP. Click selects that endpoint into the URL
// field. Mirror of the install wizard's pill row so the two surfaces feel
// consistent.
function DiscoveryPills({
  discovering,
  endpoints,
  selectedURL,
  onPick,
}: {
  discovering: boolean
  endpoints: DiscoveredEndpoint[]
  selectedURL: string
  onPick: (url: string) => void
}) {
  if (discovering) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 6, fontSize: 11, color: 'var(--ink-mute)' }}>
        <span style={{ width: 4, height: 4, borderRadius: 2, background: 'var(--ink-mute)' }} className="animate-pulse" />
        Scanning for PVE nodes…
      </div>
    )
  }
  if (endpoints.length === 0) return null
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 6, alignItems: 'center' }}>
      <span style={{ fontSize: 11, fontFamily: 'Geist Mono, monospace', color: 'var(--ink-mute)' }}>detected:</span>
      {endpoints.map((ep) => {
        const selected = selectedURL === ep.url
        const label = ep.node_name || ep.ip || ep.url
        const sub = ep.node_name ? ep.ip : ''
        return (
          <button
            type="button"
            key={ep.url}
            onClick={() => onPick(ep.url)}
            title={`${ep.url}${ep.source === 'corosync' ? ' (from corosync.conf)' : ep.source === 'localhost' ? ' (this host)' : ' (LAN scan)'}`}
            style={{
              fontFamily: 'Geist Mono, monospace',
              fontSize: 11,
              padding: '2px 8px',
              borderRadius: 4,
              border: '1px solid',
              borderColor: selected ? 'var(--ink)' : 'var(--line-strong)',
              background: selected ? 'var(--ink)' : 'transparent',
              color: selected ? 'white' : 'var(--ink-2)',
              cursor: 'pointer',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 6,
            }}
          >
            <span>{label}</span>
            {sub && <span style={{ color: selected ? 'rgba(255,255,255,0.7)' : 'var(--ink-mute)' }}>{sub}</span>}
          </button>
        )
      })}
    </div>
  )
}
