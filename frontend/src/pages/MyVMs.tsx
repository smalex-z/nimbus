import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { createPortal } from 'react-dom'
import { deleteVM, listVMs } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import SSHDetailsModal from '@/components/ui/SSHDetailsModal'
import StatusBadge from '@/components/ui/StatusBadge'
import TunnelsModal from '@/components/ui/TunnelsModal'
import { useAuth } from '@/hooks/useAuth'
import type { VM } from '@/types'

export default function MyVMs() {
  const { user } = useAuth()
  const [vms, setVms] = useState<VM[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const rows = await listVMs()
      setVms(rows)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

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
          <VMRow
            key={vm.ID}
            vm={vm}
            currentUserId={user?.id}
            onChanged={refresh}
          />
        ))}
      </div>
    </div>
  )
}

function VMRow({
  vm,
  currentUserId,
  onChanged,
}: {
  vm: VM
  currentUserId: number | undefined
  onChanged: () => void
}) {
  const [sshOpen, setSshOpen] = useState(false)
  const [tunnelsOpen, setTunnelsOpen] = useState(false)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const hasTunnel = Boolean(vm.tunnel_url)
  // Only show Delete on VMs the current user provisioned. Legacy rows
  // (owner_id null, pre-ownership) and VMs created by other users render
  // without the button — they're not deletable through this UI.
  const canDelete = currentUserId !== undefined && vm.owner_id === currentUserId

  return (
    <Card className="p-5">
      <div className="grid grid-cols-1 sm:grid-cols-[1fr_auto_auto_auto] gap-5 items-center">
        <div>
          <div className="font-display text-lg font-medium">{vm.hostname}</div>
          <div className="font-mono text-[11px] text-ink-3 mt-1 tracking-wide">
            {vm.ip} · vmid {vm.vmid} · node {vm.node} · {vm.os_template}
          </div>
          {vm.tunnel_url && (
            <div className="font-mono text-[11px] text-good mt-1 truncate" title={vm.tunnel_url}>
              🌐 {vm.tunnel_url}
            </div>
          )}
        </div>
        <span className="font-mono text-[11px] px-2.5 py-1 rounded-md bg-[rgba(27,23,38,0.05)] text-ink-2 uppercase tracking-wider justify-self-start sm:justify-self-auto">
          {vm.tier}
        </span>
        <StatusBadge status={vm.status} />
        <div className="flex gap-1.5">
          {hasTunnel && (
            <button
              type="button"
              onClick={() => setTunnelsOpen(true)}
              className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
              title="Manage Gopher tunnels for this VM"
            >
              <span aria-hidden>🌐</span>
              <span>Networks</span>
            </button>
          )}
          <button
            type="button"
            onClick={() => setSshOpen(true)}
            className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
          >
            <span aria-hidden>↗</span>
            <span>SSH</span>
          </button>
          {canDelete && (
            <button
              type="button"
              onClick={() => setDeleteOpen(true)}
              className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-bad hover:border-bad transition-colors"
              title="Destroy this VM and release its resources"
            >
              Delete
            </button>
          )}
        </div>
      </div>
      {sshOpen && (
        <SSHDetailsModal
          target={{
            hostname: vm.hostname,
            ip: vm.ip,
            username: vm.username,
            vmid: vm.vmid,
            node: vm.node,
            dbId: vm.ID,
            keyName: vm.key_name,
            tunnelUrl: vm.tunnel_url,
          }}
          onClose={() => setSshOpen(false)}
        />
      )}
      {tunnelsOpen && (
        <TunnelsModal
          vmId={vm.ID}
          hostname={vm.hostname}
          onClose={() => setTunnelsOpen(false)}
        />
      )}
      {deleteOpen && (
        <DeleteConfirm
          vm={vm}
          onCancel={() => setDeleteOpen(false)}
          onDeleted={() => {
            setDeleteOpen(false)
            onChanged()
          }}
        />
      )}
    </Card>
  )
}

function DeleteConfirm({
  vm,
  onCancel,
  onDeleted,
}: {
  vm: VM
  onCancel: () => void
  onDeleted: () => void
}) {
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

  const onConfirm = async () => {
    setBusy(true)
    setError(null)
    try {
      await deleteVM(vm.ID)
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
      aria-label={`Delete ${vm.hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[480px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Delete machine</div>
        <h3 className="text-2xl mt-1 mb-4">Destroy {vm.hostname}?</h3>
        <p className="text-sm text-ink-2 leading-relaxed mb-5">
          This stops the VM, removes it from the cluster, and releases its IP.
          The action can't be undone.
        </p>
        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 font-mono text-[11px] mb-7 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2">
          <span className="text-ink-3 uppercase tracking-wider">IP</span>
          <span className="text-ink">{vm.ip}</span>
          <span className="text-ink-3 uppercase tracking-wider">VMID</span>
          <span className="text-ink">{vm.vmid}</span>
          <span className="text-ink-3 uppercase tracking-wider">Node</span>
          <span className="text-ink">{vm.node}</span>
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
            onClick={onConfirm}
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
