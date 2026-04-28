import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  createVMTunnel,
  deleteVMTunnel,
  listVMTunnels,
  type VMTunnel,
} from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'

interface TunnelsModalProps {
  vmId: number
  hostname: string
  onClose: () => void
}

export default function TunnelsModal({ vmId, hostname, onClose }: TunnelsModalProps) {
  const [tunnels, setTunnels] = useState<VMTunnel[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [port, setPort] = useState('')
  const [subdomain, setSubdomain] = useState('')
  const [transport, setTransport] = useState<'tcp' | 'udp'>('tcp')
  const [botProtection, setBotProtection] = useState(false)
  const [submitError, setSubmitError] = useState<string | null>(null)

  const load = async () => {
    setError(null)
    try {
      const rows = await listVMTunnels(vmId)
      setTunnels(rows)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load tunnels')
    }
  }

  useEffect(() => {
    void load()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prevOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prevOverflow
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const onAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitError(null)
    const portNum = parseInt(port, 10)
    if (!Number.isFinite(portNum) || portNum < 1 || portNum > 65535) {
      setSubmitError('port must be 1-65535')
      return
    }
    setBusy(true)
    try {
      // For UDP, Gopher ignores subdomain + bot_protection server-side;
      // we still scrub them client-side so the response we display matches
      // what the user actually configured (less surprising than letting
      // the server quietly drop fields).
      const isUDP = transport === 'udp'
      await createVMTunnel(vmId, {
        target_port: portNum,
        transport,
        subdomain: !isUDP && subdomain.trim() ? subdomain.trim() : undefined,
        bot_protection_enabled: !isUDP && botProtection ? true : undefined,
      })
      setPort('')
      setSubdomain('')
      setTransport('tcp')
      setBotProtection(false)
      await load()
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'failed to add tunnel')
    } finally {
      setBusy(false)
    }
  }

  const onDelete = async (tunnelId: string) => {
    setBusy(true)
    try {
      await deleteVMTunnel(vmId, tunnelId)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed to delete tunnel')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label={`Tunnels for ${hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[760px] max-h-[calc(100vh-2rem)] overflow-y-auto p-10"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-6">
          <div className="min-w-0">
            <div className="eyebrow">Networks</div>
            <h3 className="text-3xl mt-1 break-words">{hostname}</h3>
            <p className="text-sm text-ink-2 mt-2 leading-relaxed">
              Per-port public tunnels via the Gopher gateway. Each tunnel exposes
              a service running on this VM at a public hostname.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="text-ink-3 hover:text-ink text-2xl leading-none p-1 -m-1 flex-shrink-0"
          >
            ×
          </button>
        </div>

        <div className="mt-7">
          <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-2">
            Existing tunnels
          </div>
          {tunnels === null && !error && (
            <p className="text-ink-3 font-mono text-sm">Loading…</p>
          )}
          {error && (
            <Card className="p-4 text-bad text-sm">Failed to load: {error}</Card>
          )}
          {tunnels !== null && tunnels.length === 0 && !error && (
            <p className="text-ink-3 text-sm">No tunnels yet — add one below.</p>
          )}
          {tunnels !== null && tunnels.length > 0 && (
            <div className="grid gap-2">
              {tunnels.map((t) => (
                <TunnelRow key={t.id} tunnel={t} hostname={hostname} busy={busy} onDelete={onDelete} />
              ))}
            </div>
          )}
        </div>

        <form onSubmit={onAdd} className="mt-8 pt-6 border-t border-line">
          <div className="text-base font-display font-medium mb-5">Add Tunnel</div>

          <div className="mb-5">
            <label className="block text-[13px] font-medium text-ink mb-2">
              Transport{' '}
              <span
                className="text-ink-3 cursor-help"
                title="TCP for HTTP/SSH/most services. UDP for raw datagram services (DNS, gaming, WireGuard) — UDP tunnels skip Caddy and the subdomain."
                aria-label="info"
              >
                ⓘ
              </span>
            </label>
            <div role="tablist" className="inline-flex rounded-md border border-line overflow-hidden">
              <button
                type="button"
                role="tab"
                aria-selected={transport === 'tcp'}
                onClick={() => setTransport('tcp')}
                className={`px-4 py-2 text-sm font-medium transition-colors ${
                  transport === 'tcp' ? 'bg-ink text-white' : 'bg-white/85 text-ink hover:bg-white'
                }`}
              >
                TCP
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={transport === 'udp'}
                onClick={() => setTransport('udp')}
                className={`px-4 py-2 text-sm font-medium border-l border-line transition-colors ${
                  transport === 'udp' ? 'bg-ink text-white' : 'bg-white/85 text-ink hover:bg-white'
                }`}
              >
                UDP
              </button>
            </div>
          </div>

          <div className="mb-5">
            <label className="block text-[13px] font-medium text-ink mb-1">
              Local Port{' '}
              <span className="text-ink-3 font-normal">(port your service listens on)</span>
            </label>
            <div className="flex items-center rounded-[8px] border border-line bg-white/85 focus-within:border-ink">
              <span className="px-3 py-2 font-mono text-sm text-ink-3 border-r border-line bg-[rgba(27,23,38,0.03)]">
                {transport === 'udp' ? 'localhost (udp):' : 'localhost:'}
              </span>
              <input
                type="number"
                value={port}
                onChange={(e) => setPort(e.target.value)}
                placeholder="3000"
                min={1}
                max={65535}
                required
                className="flex-1 px-3 py-2 font-mono text-sm bg-transparent focus:outline-none"
              />
            </div>
          </div>

          <div className={`mb-5 ${transport === 'udp' ? 'opacity-50' : ''}`}>
            <label className="block text-[13px] font-medium text-ink mb-1">
              Subdomain{' '}
              <span className="text-ink-3 font-normal">
                (optional — exposes service via HTTPS subdomain)
              </span>
            </label>
            <input
              type="text"
              value={subdomain}
              onChange={(e) => setSubdomain(e.target.value)}
              placeholder={transport === 'udp' ? 'not supported on UDP' : hostname}
              disabled={transport === 'udp'}
              className="block w-full px-3 py-2 rounded-[8px] border border-line bg-white/85 font-mono text-sm focus:outline-none focus:border-ink disabled:cursor-not-allowed"
            />
            <p className="mt-1.5 text-[12px] text-ink-3">
              {transport === 'udp'
                ? 'UDP tunnels are reachable at <gateway>:<server_port> only — Gopher assigns the port on creation.'
                : 'Leave blank to expose by port only.'}
            </p>
          </div>

          <div
            className={`mb-1 p-3.5 rounded-[10px] border border-dashed border-line bg-[rgba(27,23,38,0.02)] ${
              transport === 'udp' ? 'opacity-50' : ''
            }`}
          >
            <div className="flex items-start gap-3">
              <input
                type="checkbox"
                id="bot-protection"
                checked={botProtection}
                disabled={transport === 'udp'}
                onChange={(e) => setBotProtection(e.target.checked)}
                className="mt-1 cursor-pointer disabled:cursor-not-allowed"
              />
              <label htmlFor="bot-protection" className="cursor-pointer flex-1">
                <div className="flex items-center gap-2 text-[13px] font-medium text-ink">
                  Bot-protection challenge
                  <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] px-1.5 py-0.5 rounded">
                    Alpha
                  </span>
                </div>
                <p className="mt-1 text-[12px] text-ink-3 leading-relaxed">
                  Gates HTTP traffic with a proof-of-work JavaScript challenge before the
                  request reaches your service. Requires a subdomain (TCP only). Solving the
                  challenge issues a 24-hour cookie.
                </p>
              </label>
            </div>
          </div>

          {submitError && (
            <p className="mt-3 text-[12px] text-bad">{submitError}</p>
          )}

          <div className="flex justify-end gap-2 mt-6">
            <Button variant="ghost" type="button" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? 'Creating…' : 'Create Tunnel'}
            </Button>
          </div>
        </form>
      </Card>
    </div>,
    document.body,
  )
}

interface TunnelRowProps {
  tunnel: VMTunnel
  hostname: string
  busy: boolean
  onDelete: (id: string) => void
}

function TunnelRow({ tunnel, hostname, busy, onDelete }: TunnelRowProps) {
  const display = tunnel.tunnel_url || tunnel.subdomain || tunnel.id
  const isFailed = tunnel.status === 'failed'
  // The SSH base tunnel maps the public host's :port to the guest's
  // sshd. Spelling out "→ {hostname}:localhost:{port}" makes that
  // mapping explicit instead of just "port 22" — which reads like a
  // free-floating port number and doesn't say where the traffic ends up.
  const targetPort = tunnel.target_port
  const isSSHBase = targetPort === 22
  return (
    <div
      className={`p-3.5 rounded-[10px] bg-white/85 border ${isFailed ? 'border-[rgba(184,101,15,0.35)]' : 'border-line'}`}
    >
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="font-mono text-sm text-ink break-all">
            {tunnel.tunnel_url ? (
              <a
                href={tunnel.tunnel_url}
                target="_blank"
                rel="noreferrer"
                className="underline"
              >
                {display}
              </a>
            ) : (
              display
            )}
            {isSSHBase && (
              <span
                className="ml-2 align-middle text-[10px] font-mono uppercase tracking-wider text-good bg-[rgba(60,150,90,0.10)] border border-[rgba(60,150,90,0.25)] rounded px-1.5 py-0.5"
                title="The provision-time SSH exposure for this VM. Public host:port maps to the guest's sshd."
              >
                SSH base
              </span>
            )}
          </div>
          <div className="text-[11px] font-mono text-ink-3 mt-1 flex flex-wrap gap-x-2 gap-y-0.5">
            <span title={`Mapped to ${hostname}'s localhost:${targetPort}`}>
              → <span className="text-ink-2">{hostname}</span>
              <span className="text-ink-3"> · localhost:{targetPort}</span>
            </span>
            {tunnel.transport && tunnel.transport !== 'tcp' && (
              <span className="uppercase">· {tunnel.transport}</span>
            )}
            {tunnel.private && <span>· private</span>}
            {tunnel.bot_protection_enabled && (
              <span className="text-warn">· bot-protected</span>
            )}
            {tunnel.status && tunnel.status !== 'active' && (
              <span className={isFailed ? 'text-warn' : ''}>· {tunnel.status}</span>
            )}
          </div>
          {tunnel.error && (
            <div className="text-[11px] text-warn mt-1">{tunnel.error}</div>
          )}
        </div>
        <div className="flex gap-1.5 flex-shrink-0">
          {tunnel.tunnel_url && <CopyButton value={tunnel.tunnel_url} />}
          <button
            type="button"
            onClick={() => onDelete(tunnel.id)}
            disabled={busy}
            className="px-2.5 py-1 rounded-md text-[11px] font-mono uppercase tracking-wider border border-line-2 text-ink-2 hover:border-bad hover:text-bad transition-colors disabled:opacity-50"
            title="Remove this tunnel"
          >
            Remove
          </button>
        </div>
      </div>
    </div>
  )
}
