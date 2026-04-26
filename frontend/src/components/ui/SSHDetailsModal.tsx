import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { getVMPrivateKey } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'
import { buildSSHCommand, parseTunnelURL } from '@/lib/format'

export interface SSHTarget {
  hostname: string
  ip: string
  username: string
  vmid: number
  node: string
  // dbId is the Nimbus DB row id, required to fetch the stored private key.
  // Omit when the caller has no DB id (modal then hides the download button).
  dbId?: number
  keyName?: string
  // tunnelUrl is "host:port" from Gopher when the VM has an established
  // public SSH tunnel. Modal shows a second SSH command when present.
  tunnelUrl?: string
}

interface SSHDetailsModalProps {
  target: SSHTarget
  onClose: () => void
}

export default function SSHDetailsModal({ target, onClose }: SSHDetailsModalProps) {
  const sshCommand = buildSSHCommand(target.username, target.ip, target.keyName)
  const tunnel = target.tunnelUrl ? parseTunnelURL(target.tunnelUrl) : undefined
  const publicSSHCommand = tunnel
    ? buildSSHCommand(target.username, tunnel.host, target.keyName, tunnel.port)
    : undefined

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])

  const canDownload = Boolean(target.dbId && target.keyName)

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label={`SSH details for ${target.hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[760px] max-h-[calc(100vh-2rem)] overflow-y-auto p-10"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-6">
          <div className="min-w-0">
            <div className="eyebrow">SSH details</div>
            <h3 className="text-3xl mt-1 break-words">{target.hostname}</h3>
            <p className="text-sm text-ink-2 mt-2 leading-relaxed">
              Connect from a machine that can reach <span className="font-mono">{target.ip}</span>.
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

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3.5 mt-7">
          <DetailCell label="IP address" value={target.ip} />
          <DetailCell label="Username" value={target.username} />
          <DetailCell label="VMID / Node" value={`${target.vmid} on ${target.node}`} />
          <DetailCell
            label="Key name"
            value={target.keyName ?? '—'}
            copyable={Boolean(target.keyName)}
          />
          <SSHCommandCell lanCommand={sshCommand} wanCommand={publicSSHCommand} />
        </div>

        {canDownload && (
          <div className="mt-7">
            <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-2">
              Private key
            </div>
            <DownloadKeyButton vmId={target.dbId as number} />
            <p className="mt-3 text-[12px] text-ink-3 leading-relaxed">
              Move the downloaded file into <span className="font-mono">~/.ssh/</span> and
              run <span className="font-mono">chmod 600</span> before connecting.
            </p>
          </div>
        )}

        <div className="flex justify-end mt-9">
          <Button variant="ghost" onClick={onClose}>Close</Button>
        </div>
      </Card>
    </div>,
    document.body,
  )
}

interface DetailCellProps {
  label: string
  value: string
  fullWidth?: boolean
  copyable?: boolean
}

// SSHCommandCell renders the SSH command line. When a public-tunnel command
// is also available, the label switches into a WAN/LAN toggle and the cell
// defaults to WAN — that's the more useful one for SSHing in from anywhere.
// Falls back to a plain "SSH command" cell when only LAN is available.
function SSHCommandCell({ lanCommand, wanCommand }: { lanCommand: string; wanCommand?: string }) {
  const [mode, setMode] = useState<'wan' | 'lan'>(wanCommand ? 'wan' : 'lan')
  if (!wanCommand) {
    return <DetailCell label="SSH command" value={lanCommand} fullWidth />
  }
  const value = mode === 'wan' ? wanCommand : lanCommand
  return (
    <div className="p-3.5 rounded-[10px] bg-white/85 border border-line sm:col-span-2">
      <div className="flex items-center justify-between mb-1.5">
        <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3">
          SSH
        </div>
        <div
          role="tablist"
          aria-label="SSH endpoint"
          className="inline-flex rounded-md border border-line overflow-hidden text-[10px] font-mono uppercase tracking-wider"
        >
          <button
            type="button"
            role="tab"
            aria-selected={mode === 'wan'}
            onClick={() => setMode('wan')}
            title="Public tunnel — reachable from anywhere"
            className={`px-2 py-0.5 transition-colors ${
              mode === 'wan' ? 'bg-ink text-white' : 'bg-transparent text-ink-3 hover:text-ink'
            }`}
          >
            🌐 WAN
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={mode === 'lan'}
            onClick={() => setMode('lan')}
            title="LAN — only reachable from inside the cluster network"
            className={`px-2 py-0.5 border-l border-line transition-colors ${
              mode === 'lan' ? 'bg-ink text-white' : 'bg-transparent text-ink-3 hover:text-ink'
            }`}
          >
            🏠 LAN
          </button>
        </div>
      </div>
      <div className="font-mono text-sm text-ink break-all flex items-center justify-between gap-3">
        <span>{value}</span>
        <CopyButton value={value} />
      </div>
    </div>
  )
}

function DetailCell({ label, value, fullWidth = false, copyable = true }: DetailCellProps) {
  return (
    <div
      className={`p-3.5 rounded-[10px] bg-white/85 border border-line ${
        fullWidth ? 'sm:col-span-2' : ''
      }`}
    >
      <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
        {label}
      </div>
      <div className="font-mono text-sm text-ink break-all flex items-center justify-between gap-3">
        <span>{value}</span>
        {copyable && <CopyButton value={value} />}
      </div>
    </div>
  )
}

function DownloadKeyButton({ vmId }: { vmId: number }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onClick = async () => {
    setBusy(true)
    setError(null)
    try {
      const { key_name, private_key } = await getVMPrivateKey(vmId)
      const content = private_key.endsWith('\n') ? private_key : private_key + '\n'
      const blob = new Blob([content], { type: 'application/x-pem-file' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = key_name
      document.body.appendChild(a)
      a.click()
      document.body.removeChild(a)
      URL.revokeObjectURL(url)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'download failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex flex-col items-start gap-1">
      <button
        type="button"
        onClick={onClick}
        disabled={busy}
        title="Download the private key stored in the Nimbus vault."
        className="inline-flex items-center gap-2 px-4 py-2.5 rounded-[10px] bg-ink text-white font-mono text-xs tracking-wide hover:bg-ink-2 transition-colors disabled:opacity-50 disabled:cursor-wait"
      >
        <span aria-hidden>↓</span>
        <span>{busy ? 'FETCHING…' : 'DOWNLOAD PRIVATE KEY'}</span>
      </button>
      {error && <span className="text-[11px] text-bad">{error}</span>}
    </div>
  )
}
