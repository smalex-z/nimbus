import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { listVMs } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'
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
  const sshCommand = `ssh ${vm.username}@${vm.ip}`
  const isRunning = vm.status === 'running'
  const statusClass = isRunning ? 'text-good' : vm.status === 'failed' ? 'text-bad' : 'text-warn'
  const statusLabel = vm.status.toUpperCase()

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
        <span
          className={`inline-flex items-center gap-1.5 font-mono text-xs ${statusClass}`}
        >
          <span
            className={`w-1.5 h-1.5 rounded-full ${
              isRunning ? 'bg-good' : vm.status === 'failed' ? 'bg-bad' : 'bg-warn'
            }`}
          />
          {statusLabel}
        </span>
        <CopyButton value={sshCommand} label="COPY SSH" />
      </div>
    </Card>
  )
}
