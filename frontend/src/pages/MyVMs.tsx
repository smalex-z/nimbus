import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { getVMPrivateKey, listVMs } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'
import StatusBadge from '@/components/ui/StatusBadge'
import type { VM } from '@/types'

export default function MyVMs() {
  const [vms, setVms] = useState<VM[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    listVMs()
      .then((rows) => {
        if (!cancelled) setVms(rows)
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div>
      <div className="flex items-end justify-between flex-wrap gap-4 mb-2">
        <div>
          <div className="eyebrow">{vms.length} machine{vms.length === 1 ? '' : 's'}</div>
          <h2 className="text-3xl">Your machines</h2>
          <p className="text-base text-ink-2 mt-2 leading-relaxed">
            Everything you've spun up. SSH details below.
          </p>
        </div>
        <Link to="/">
          <Button>+ New machine</Button>
        </Link>
      </div>

      {loading && <p className="mt-8 text-ink-3 font-mono text-sm">Loading…</p>}
      {error && (
        <Card className="mt-8 p-6 text-bad text-sm">
          Failed to load: {error}
        </Card>
      )}

      {!loading && !error && vms.length === 0 && (
        <Card className="mt-8 p-12 text-center">
          <div className="eyebrow">No machines yet</div>
          <h3 className="text-xl mt-2">Provision your first VM.</h3>
          <Link to="/">
            <Button className="mt-5">Get started</Button>
          </Link>
        </Card>
      )}

      <div className="grid gap-3 mt-7">
        {vms.map((vm) => (
          <VMRow key={vm.ID} vm={vm} />
        ))}
      </div>
    </div>
  )
}

function VMRow({ vm }: { vm: VM }) {
  const [sshOpen, setSshOpen] = useState(false)

  return (
    <Card className="p-5">
      <div className="grid grid-cols-1 sm:grid-cols-[1fr_auto_auto_auto] gap-5 items-center">
        <div>
          <div className="font-display text-lg font-medium">{vm.hostname}</div>
          <div className="font-mono text-[11px] text-ink-3 mt-1 tracking-wide">
            {vm.ip} · vmid {vm.vmid} · node {vm.node} · {vm.os_template}
          </div>
        </div>
        <span className="font-mono text-[11px] px-2.5 py-1 rounded-md bg-[rgba(27,23,38,0.05)] text-ink-2 uppercase tracking-wider justify-self-start sm:justify-self-auto">
          {vm.tier}
        </span>
        <StatusBadge status={vm.status} />
        <button
          type="button"
          onClick={() => setSshOpen(true)}
          className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
        >
          <span aria-hidden>↗</span>
          <span>SSH</span>
        </button>
      </div>
      {sshOpen && <SSHDetailsModal vm={vm} onClose={() => setSshOpen(false)} />}
    </Card>
  )
}

interface SSHDetailsModalProps {
  vm: VM
  onClose: () => void
}

function SSHDetailsModal({ vm, onClose }: SSHDetailsModalProps) {
  const sshCommand = vm.key_name
    ? `ssh -i ~/.ssh/${vm.key_name} ${vm.username}@${vm.ip}`
    : `ssh ${vm.username}@${vm.ip}`

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

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label={`SSH details for ${vm.hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[560px] p-8"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <div className="eyebrow">SSH details</div>
            <h3 className="text-2xl mt-1">{vm.hostname}</h3>
            <p className="text-[13px] text-ink-2 mt-1">
              Connect from a machine that can reach <span className="font-mono">{vm.ip}</span>.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="text-ink-3 hover:text-ink text-xl leading-none p-1 -m-1"
          >
            ×
          </button>
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-6">
          <DetailCell label="IP address" value={vm.ip} />
          <DetailCell label="Username" value={vm.username} />
          <DetailCell label="VMID / Node" value={`${vm.vmid} on ${vm.node}`} />
          <DetailCell label="Key name" value={vm.key_name ?? '—'} copyable={Boolean(vm.key_name)} />
          <DetailCell label="SSH command" value={sshCommand} fullWidth />
        </div>

        {vm.key_name && (
          <div className="mt-6">
            <div className="text-[10px] font-mono uppercase tracking-widest text-ink-3 mb-1.5">
              Private key
            </div>
            <DownloadKeyButton vmId={vm.ID} />
            <p className="mt-3 text-[12px] text-ink-3 leading-relaxed">
              Move the downloaded file into <span className="font-mono">~/.ssh/</span> and
              run <span className="font-mono">chmod 600</span> before connecting.
            </p>
          </div>
        )}

        <div className="flex justify-end mt-7">
          <Button variant="ghost" onClick={onClose}>Close</Button>
        </div>
      </Card>
    </div>
  )
}

interface DetailCellProps {
  label: string
  value: string
  fullWidth?: boolean
  copyable?: boolean
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
