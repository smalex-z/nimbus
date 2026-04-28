import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { deleteVM, listVMs, vmLifecycle } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import DeleteVMConfirm from '@/components/ui/DeleteVMConfirm'
import SSHDetailsModal from '@/components/ui/SSHDetailsModal'
import StatusBadge from '@/components/ui/StatusBadge'
import TunnelsModal from '@/components/ui/TunnelsModal'
import VMActions from '@/components/ui/VMActions'
import { NetworkIcon, TerminalIcon } from '@/components/ui/icons'
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
            <div className="font-mono text-[11px] text-good mt-1 truncate inline-flex items-center gap-1.5" title={vm.tunnel_url}>
              <NetworkIcon size={11} />
              <span className="truncate">{vm.tunnel_url}</span>
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
              className="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
              title={`Manage Gopher tunnels for ${vm.hostname}`}
              aria-label={`Manage tunnels for ${vm.hostname}`}
            >
              <NetworkIcon />
            </button>
          )}
          <button
            type="button"
            onClick={() => setSshOpen(true)}
            className="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
            title={`SSH details for ${vm.hostname}`}
            aria-label={`SSH details for ${vm.hostname}`}
          >
            <TerminalIcon />
          </button>
          <VMActions
            hostname={vm.hostname}
            status={vm.status as 'running' | 'stopped' | 'paused' | 'unknown'}
            canRemove={canDelete}
            onLifecycle={async (op) => {
              await vmLifecycle(vm.ID, op)
              onChanged()
            }}
            onRemove={canDelete ? () => setDeleteOpen(true) : undefined}
          />
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
        <DeleteVMConfirm
          vm={{
            id: vm.ID,
            hostname: vm.hostname,
            ip: vm.ip,
            vmid: vm.vmid,
            node: vm.node,
          }}
          onConfirm={deleteVM}
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
