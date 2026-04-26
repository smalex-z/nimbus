import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { listVMs } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import SSHDetailsModal from '@/components/ui/SSHDetailsModal'
import StatusBadge from '@/components/ui/StatusBadge'
import TunnelsModal from '@/components/ui/TunnelsModal'
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
  const [tunnelsOpen, setTunnelsOpen] = useState(false)
  const hasTunnel = Boolean(vm.tunnel_url)

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
    </Card>
  )
}
