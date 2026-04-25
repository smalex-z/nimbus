import { useEffect, useState } from 'react'
import { listNodes } from '@/api/client'
import Card from '@/components/ui/Card'
import StatusBadge from '@/components/ui/StatusBadge'
import UsageBar from '@/components/ui/UsageBar'
import { formatBytes } from '@/lib/format'
import type { NodeView } from '@/types'

export default function Nodes() {
  const [nodes, setNodes] = useState<NodeView[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    listNodes()
      .then((rows) => {
        if (!cancelled) setNodes(rows)
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
      <div>
        <div className="eyebrow">Cluster status</div>
        <h2 className="text-3xl">Nodes</h2>
        <p className="text-base text-ink-2 mt-2">
          Live telemetry from the Proxmox cluster. Used for scoring on every provision.
        </p>
      </div>

      {loading && <p className="mt-8 text-ink-3 font-mono text-sm">Loading…</p>}
      {error && (
        <Card className="mt-8 p-6 text-bad text-sm">Failed to load: {error}</Card>
      )}

      <div className="grid gap-3 mt-7">
        {nodes.map((n) => {
          const memPct = n.mem_total > 0 ? (n.mem_used / n.mem_total) * 100 : 0
          const cpuPct = n.cpu * 100
          return (
            <Card key={n.name} className="p-6">
              <div className="flex items-center justify-between flex-wrap gap-3">
                <div>
                  <div className="font-display text-lg font-medium">{n.name}</div>
                  <div className="font-mono text-[11px] text-ink-3 mt-1">
                    {n.max_cpu} cores · {formatBytes(n.mem_total)} RAM
                  </div>
                </div>
                <StatusBadge status={n.status} />
              </div>

              <div className="mt-5 grid grid-cols-2 gap-5">
                <UsageBar label="Memory" pct={memPct} hint={`${formatBytes(n.mem_used)} / ${formatBytes(n.mem_total)}`} />
                <UsageBar label="CPU" pct={cpuPct} hint={`${cpuPct.toFixed(1)}%`} />
              </div>
            </Card>
          )
        })}
      </div>
    </div>
  )
}

