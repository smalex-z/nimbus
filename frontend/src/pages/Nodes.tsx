import { useEffect, useState } from 'react'
import { listNodes } from '@/api/client'
import Card from '@/components/ui/Card'
import type { NodeView } from '@/types'

const formatBytes = (bytes: number): string => {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = bytes
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}

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
          const isOnline = n.status === 'online'
          return (
            <Card key={n.name} className="p-6">
              <div className="flex items-center justify-between flex-wrap gap-3">
                <div>
                  <div className="font-display text-lg font-medium">{n.name}</div>
                  <div className="font-mono text-[11px] text-ink-3 mt-1">
                    {n.max_cpu} cores · {formatBytes(n.mem_total)} RAM
                  </div>
                </div>
                <span
                  className={`inline-flex items-center gap-1.5 font-mono text-xs ${
                    isOnline ? 'text-good' : 'text-bad'
                  }`}
                >
                  <span
                    className={`w-1.5 h-1.5 rounded-full ${
                      isOnline ? 'bg-good' : 'bg-bad'
                    }`}
                  />
                  {n.status.toUpperCase()}
                </span>
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

function UsageBar({ label, pct, hint }: { label: string; pct: number; hint: string }) {
  return (
    <div>
      <div className="flex justify-between text-[11px] font-mono text-ink-3 mb-1.5 uppercase tracking-wider">
        <span>{label}</span>
        <span className="normal-case tracking-normal">{hint}</span>
      </div>
      <div className="h-2.5 rounded-md bg-[rgba(27,23,38,0.06)] overflow-hidden">
        <div
          className="h-full rounded-md"
          style={{
            width: `${Math.min(100, pct).toFixed(0)}%`,
            background: 'linear-gradient(90deg, var(--c1, #F8AF82), var(--c2, #F496B4))',
          }}
        />
      </div>
    </div>
  )
}
