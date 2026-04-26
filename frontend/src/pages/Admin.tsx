import { useEffect, useMemo, useState } from 'react'
import { getClusterStats, listClusterVMs, listIPs, listNodes } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import CopyButton from '@/components/ui/CopyButton'
import StatusBadge from '@/components/ui/StatusBadge'
import UsageBar from '@/components/ui/UsageBar'
import { formatBytes } from '@/lib/format'
import type { ClusterStats, ClusterVM, ClusterVMStatus, IPAllocation, NodeView, TierName } from '@/types'

interface AdminData {
  nodes: NodeView[]
  vms: ClusterVM[]
  ips: IPAllocation[]
  clusterStats: ClusterStats | null
  loading: boolean
  error: string | null
}

function useAdminData(): AdminData {
  const [nodes, setNodes] = useState<NodeView[]>([])
  const [vms, setVMs] = useState<ClusterVM[]>([])
  const [ips, setIPs] = useState<IPAllocation[]>([])
  const [clusterStats, setClusterStats] = useState<ClusterStats | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    const fetch = () => {
      Promise.all([listNodes(), listClusterVMs(), listIPs(), getClusterStats()])
        .then(([n, v, i, s]) => {
          if (!cancelled) {
            setNodes(n)
            setVMs(v)
            setIPs(i)
            setClusterStats(s)
            setError(null)
          }
        })
        .catch((e: unknown) => {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e))
        })
        .finally(() => {
          if (!cancelled) setLoading(false)
        })
    }
    fetch()
    const id = setInterval(fetch, 15_000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [])

  return { nodes, vms, ips, clusterStats, loading, error }
}

interface FilterState {
  node: string | null
  status: ClusterVMStatus | null
  tier: TierName | null
}

const EMPTY_FILTERS: FilterState = { node: null, status: null, tier: null }

export default function Admin() {
  const { nodes, vms, ips, clusterStats, loading, error } = useAdminData()
  const [filters, setFilters] = useState<FilterState>(EMPTY_FILTERS)


  const filteredVMs = useMemo(() => {
    return vms.filter((vm) => {
      if (filters.node && vm.node !== filters.node) return false
      if (filters.status && vm.status !== filters.status) return false
      if (filters.tier && vm.tier !== filters.tier) return false
      return true
    })
  }, [vms, filters])

  const stats = useMemo(() => {
    const onlineNodes = nodes.filter((n) => n.status === 'online')
    const sumMemUsed = onlineNodes.reduce((a, n) => a + n.mem_used, 0)
    const sumMemTotal = onlineNodes.reduce((a, n) => a + n.mem_total, 0)
    // CPU aggregation: weight each node's utilization by its core count so a
    // 16-core node hitting 50% counts more than an 8-core node at 50%.
    const totalCores = onlineNodes.reduce((a, n) => a + n.max_cpu, 0)
    const usedCores = onlineNodes.reduce((a, n) => a + n.cpu * n.max_cpu, 0)
    const clusterVMsRunning = onlineNodes.reduce((a, n) => a + n.vm_count, 0)
    const clusterVMsTotal = onlineNodes.reduce((a, n) => a + n.vm_count_total, 0)
    const allocatedIPs = ips.filter((i) => i.status === 'allocated').length
    return {
      nodesOnline: onlineNodes.length,
      nodesTotal: nodes.length,
      clusterVMsRunning,
      clusterVMsTotal,
      allocatedIPs,
      totalIPs: ips.length,
      sumMemUsed,
      sumMemTotal,
      totalCores,
      usedCores,
      storageUsed: clusterStats?.storage_used ?? 0,
      storageTotal: clusterStats?.storage_total ?? 0,
    }
  }, [nodes, ips, clusterStats])

  const allNodes = useMemo(() => [...new Set(vms.map((v) => v.node))].sort(), [vms])
  const allTiers = useMemo<TierName[]>(() => {
    const found = new Set(vms.map((v) => v.tier).filter((t): t is TierName => Boolean(t)))
    const order: TierName[] = ['small', 'medium', 'large', 'xl']
    return order.filter((t) => found.has(t))
  }, [vms])

  const hasFilters = Object.values(filters).some(Boolean)
  const clearFilters = () => setFilters(EMPTY_FILTERS)

  const setNodeFilter = (node: string | null) =>
    setFilters((f) => ({ ...f, node: f.node === node ? null : node }))

  return (
    <div>
      <div className="mb-8">
        <div className="eyebrow">Cluster admin</div>
        <h2 className="text-3xl">Dashboard</h2>
        <p className="text-base text-ink-2 mt-2">
          Live overview of nodes and VMs across the cluster. Refreshes every 15 seconds.
        </p>
      </div>

      {loading && <p className="mt-8 text-ink-3 font-mono text-sm">Loading…</p>}
      {error && (
        <Card className="mt-8 p-6 text-bad text-sm">Failed to load: {error}</Card>
      )}

      {!loading && (
        <>
          <SummaryStats stats={stats} />

          <div className="mt-8 mb-2">
            <div className="eyebrow">Nodes</div>
          </div>
          <NodeCardGrid
            nodes={nodes}
            activeNode={filters.node}
            onNodeClick={setNodeFilter}
          />

          <div className="mt-10 mb-2">
            <div className="eyebrow">{filteredVMs.length} machine{filteredVMs.length === 1 ? '' : 's'}</div>
            <h3 className="text-xl">Virtual machines</h3>
          </div>
          <VMTable
            vms={filteredVMs}
            allVMs={vms}
            allNodes={allNodes}
            allTiers={allTiers}
            filters={filters}
            onFilterChange={(patch) => setFilters((f) => ({ ...f, ...patch }))}
            hasFilters={hasFilters}
            onClearFilters={clearFilters}
          />
        </>
      )}
    </div>
  )
}

interface StatsShape {
  nodesOnline: number
  nodesTotal: number
  clusterVMsRunning: number
  clusterVMsTotal: number
  allocatedIPs: number
  totalIPs: number
  sumMemUsed: number
  sumMemTotal: number
  totalCores: number
  usedCores: number
  storageUsed: number
  storageTotal: number
}

function SummaryStats({ stats }: { stats: StatsShape }) {
  const memPct = stats.sumMemTotal > 0 ? (stats.sumMemUsed / stats.sumMemTotal) * 100 : 0
  const cpuPct = stats.totalCores > 0 ? (stats.usedCores / stats.totalCores) * 100 : 0
  const storagePct = stats.storageTotal > 0 ? (stats.storageUsed / stats.storageTotal) * 100 : 0

  return (
    <div className="space-y-3">
      <CountStatsCard
        items={[
          { label: 'Nodes', value: stats.nodesOnline, sub: `/ ${stats.nodesTotal} total`, detail: 'online' },
          { label: 'VMs', value: stats.clusterVMsRunning, sub: `/ ${stats.clusterVMsTotal} total`, detail: 'running' },
          { label: 'IP pool', value: stats.allocatedIPs, sub: `/ ${stats.totalIPs} total`, detail: 'allocated' },
        ]}
      />
      <GaugeStatsCard
        items={[
          { label: 'CPU Usage', pct: cpuPct, detail: `${stats.usedCores.toFixed(1)} / ${stats.totalCores} cores`, id: 'gauge-cpu' },
          { label: 'Memory Usage', pct: memPct, detail: `${formatBytes(stats.sumMemUsed)} / ${formatBytes(stats.sumMemTotal)}`, id: 'gauge-memory' },
          { label: 'Storage Usage', pct: storagePct, detail: `${formatBytes(stats.storageUsed)} / ${formatBytes(stats.storageTotal)}`, id: 'gauge-storage' },
        ]}
      />
    </div>
  )
}

function HalfCircleGauge({ pct, id }: { pct: number; id: string }) {
  const r = 90
  const cx = 100
  const cy = 100
  const circumHalf = Math.PI * r
  const fillLen = Math.min(pct / 100, 1) * circumHalf
  const d = `M ${cx - r},${cy} A ${r},${r} 0 0 1 ${cx + r},${cy}`

  // The gradient is compressed to span exactly the visible fill portion of the arc,
  // so the full c1→c2 spectrum is always visible regardless of fill percentage —
  // matching how CSS linear-gradient renders on the UsageBar fill div.
  const t = Math.min(pct / 100, 1)
  const fillEndX = cx - r * Math.cos(Math.PI * t)  // x-coord at arc parameter t
  const gradX2 = pct > 0 ? fillEndX : cx - r + 0.01  // avoid zero-length gradient

  return (
    <svg viewBox="0 0 200 110" className="w-full">
      <defs>
        <linearGradient id={id} x1={cx - r} y1={0} x2={gradX2} y2={0} gradientUnits="userSpaceOnUse">
          <stop offset="0%" stopColor="#F8AF82" />
          <stop offset="100%" stopColor="#F496B4" />
        </linearGradient>
      </defs>
      <path d={d} fill="none" stroke="rgba(20,18,28,0.07)" strokeWidth="14" strokeLinecap="round" />
      <path
        d={d}
        fill="none"
        stroke={`url(#${id})`}
        strokeWidth="14"
        strokeLinecap="round"
        strokeDasharray={`${fillLen} ${circumHalf}`}
      />
      <text
        x="100"
        y="88"
        textAnchor="middle"
        fontSize="34"
        fontWeight="500"
        fontFamily="Fraunces, serif"
        fill="#14121C"
      >
        {pct.toFixed(0)}%
      </text>
    </svg>
  )
}

function CountStatsCard({
  items,
}: {
  items: { label: string; value: number; sub: string; detail: string }[]
}) {
  return (
    <Card className="py-6">
      <div className="grid grid-cols-3 divide-x divide-line">
        {items.map((item) => (
          <div key={item.label} className="px-8">
            <div className="eyebrow mb-3">{item.label}</div>
            <div className="flex items-baseline gap-2">
              <span className="font-display text-5xl font-medium leading-none">{item.value}</span>
              <span className="font-mono text-xl text-ink-3">{item.sub}</span>
            </div>
            <div className="font-mono text-xs text-ink-3 uppercase tracking-wider mt-2">{item.detail}</div>
          </div>
        ))}
      </div>
    </Card>
  )
}

function GaugeStatsCard({
  items,
}: {
  items: { label: string; pct: number; detail: string; id: string }[]
}) {
  return (
    <Card className="py-6">
      <div className="grid grid-cols-3 divide-x divide-line">
        {items.map((item) => (
          <div key={item.label} className="px-8 flex flex-col items-center">
            <div className="eyebrow w-full mb-2">{item.label}</div>
            <HalfCircleGauge pct={item.pct} id={item.id} />
            <div className="font-mono text-sm text-ink-3 text-center mt-1">{item.detail}</div>
          </div>
        ))}
      </div>
    </Card>
  )
}

function NodeCardGrid({
  nodes,
  activeNode,
  onNodeClick,
}: {
  nodes: NodeView[]
  activeNode: string | null
  onNodeClick: (name: string | null) => void
}) {
  return (
    <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-3">
      {nodes.map((n) => (
        <NodeCard
          key={n.name}
          node={n}
          active={activeNode === n.name}
          onClick={() => onNodeClick(n.name)}
        />
      ))}
    </div>
  )
}

function NodeCard({
  node: n,
  active,
  onClick,
}: {
  node: NodeView
  active: boolean
  onClick: () => void
}) {
  const memUsedPct = n.mem_total > 0 ? (n.mem_used / n.mem_total) * 100 : 0
  const memAllocPct = n.mem_total > 0 ? (n.mem_allocated / n.mem_total) * 100 : 0
  const swapPct = n.swap_total > 0 ? (n.swap_used / n.swap_total) * 100 : 0
  const cpuPct = n.cpu * 100
  // Swap is sticky — Linux rarely pages cold residuals back in even after
  // pressure subsides, so most hosts carry KB-scale swap forever. Suppress
  // the pill below 10 MiB to keep low-pressure cards quiet.
  const swapping = n.swap_used > 10 * 1024 * 1024
  const hasSwap = n.swap_total > 0

  return (
    <button
      onClick={onClick}
      className={`text-left w-full transition-all ${active ? 'ring-2 ring-ink/20 rounded-[14px]' : ''}`}
    >
      <Card className="p-6 hover:bg-[rgba(27,23,38,0.03)] transition-colors">
        <div className="flex items-center justify-between flex-wrap gap-3">
          <div>
            <div className="font-display text-lg font-medium">{n.name}</div>
            <div className="font-mono text-[11px] text-ink-3 mt-1">
              {n.max_cpu} cores · {formatBytes(n.mem_total)} RAM
            </div>
            <div className="font-mono text-[11px] text-ink-3 mt-0.5">
              {n.vm_count} VM{n.vm_count !== 1 ? 's' : ''}
              {active && <span className="text-ink ml-2">· filtered</span>}
            </div>
          </div>
          <div className="flex flex-col items-end gap-1.5">
            <StatusBadge status={n.status} />
            {swapping && (
              <span
                className="inline-flex items-center gap-1 px-2 py-0.5 rounded text-[10px] font-mono uppercase tracking-wider text-[#9a5c2e] bg-[rgba(248,175,130,0.15)] border border-[rgba(248,175,130,0.4)]"
                title="Allocated memory exceeded physical RAM — host is paging to swap."
              >
                swap{' '}
                <span className="normal-case tracking-normal">
                  +{formatBytes(n.swap_used)}
                </span>
              </span>
            )}
          </div>
        </div>
        <div className="mt-5 flex flex-col gap-3">
          <UsageBar label="CPU" pct={cpuPct} hint={`${cpuPct.toFixed(1)}%`} />
          <UsageBar
            label="Memory in use"
            pct={memUsedPct}
            hint={`${formatBytes(n.mem_used)} / ${formatBytes(n.mem_total)}`}
          />
          <UsageBar
            label="Memory allocated"
            pct={memAllocPct}
            hint={`${formatBytes(n.mem_allocated)} / ${formatBytes(n.mem_total)}`}
          />
          {hasSwap && (
            <UsageBar
              label="Swap Usage"
              pct={swapPct}
              hint={`${formatBytes(n.swap_used)} / ${formatBytes(n.swap_total)}`}
            />
          )}
        </div>
      </Card>
    </button>
  )
}

function VMTable({
  vms,
  allVMs,
  allNodes,
  allTiers,
  filters,
  onFilterChange,
  hasFilters,
  onClearFilters,
}: {
  vms: ClusterVM[]
  allVMs: ClusterVM[]
  allNodes: string[]
  allTiers: TierName[]
  filters: FilterState
  onFilterChange: (patch: Partial<FilterState>) => void
  hasFilters: boolean
  onClearFilters: () => void
}) {
  const selectClass =
    'rounded-[8px] bg-white/85 font-sans text-sm text-ink border border-line-2 px-3 py-1.5 focus:outline-none'

  if (allVMs.length === 0) {
    return (
      <Card className="py-16 text-center">
        <div className="eyebrow">No machines</div>
        <p className="text-sm text-ink-2 mt-2">No VMs are running on the cluster.</p>
      </Card>
    )
  }

  return (
    <div>
      <div className="flex flex-wrap gap-2 mb-4 items-center">
        <select
          className={selectClass}
          value={filters.node ?? ''}
          onChange={(e) => onFilterChange({ node: e.target.value || null })}
        >
          <option value="">All nodes</option>
          {allNodes.map((n) => (
            <option key={n} value={n}>{n}</option>
          ))}
        </select>
        <select
          className={selectClass}
          value={filters.status ?? ''}
          onChange={(e) => onFilterChange({ status: (e.target.value as ClusterVMStatus) || null })}
        >
          <option value="">All statuses</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
          <option value="paused">Paused</option>
        </select>
        <select
          className={selectClass}
          value={filters.tier ?? ''}
          onChange={(e) => onFilterChange({ tier: (e.target.value as TierName) || null })}
        >
          <option value="">All tiers</option>
          {allTiers.map((t) => (
            <option key={t} value={t}>{t}</option>
          ))}
        </select>
        {hasFilters && (
          <Button variant="ghost" size="small" onClick={onClearFilters}>
            Clear filters
          </Button>
        )}
      </div>

      {vms.length === 0 ? (
        <Card className="py-16 text-center">
          <div className="eyebrow">No results</div>
          <p className="text-sm text-ink-2 mt-2">No VMs match the current filters.</p>
          <Button variant="ghost" size="small" className="mt-4" onClick={onClearFilters}>
            Clear filters
          </Button>
        </Card>
      ) : (
        <Card className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line">
                {['Name', 'VMID', 'Node', 'IP', 'Tier', 'OS', 'Status', 'Source', 'SSH'].map((col) => (
                  <th
                    key={col}
                    className="text-left text-[11px] font-mono uppercase tracking-wider text-ink-3 px-4 py-3 whitespace-nowrap"
                  >
                    {col}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {vms.map((vm) => {
                const displayName = vm.hostname || vm.name
                const dash = <span className="text-ink-3">—</span>
                return (
                  <tr key={`${vm.node}-${vm.vmid}`} className="border-t border-line hover:bg-[rgba(27,23,38,0.02)]">
                    <td className="px-4 py-3 font-display font-medium whitespace-nowrap">{displayName}</td>
                    <td className="px-4 py-3 font-mono text-xs text-ink-2 whitespace-nowrap">{vm.vmid}</td>
                    <td className="px-4 py-3">
                      <button
                        className="font-mono text-xs text-ink-2 hover:text-ink hover:underline"
                        onClick={() => onFilterChange({ node: vm.node })}
                      >
                        {vm.node}
                      </button>
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-ink-2 whitespace-nowrap">
                      {vm.ip || dash}
                    </td>
                    <td className="px-4 py-3">
                      {vm.tier ? (
                        <span className="font-mono text-[11px] px-2 py-0.5 rounded-md bg-[rgba(27,23,38,0.05)] text-ink-2 uppercase tracking-wider">
                          {vm.tier}
                        </span>
                      ) : dash}
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-ink-2 whitespace-nowrap">
                      {vm.os_template || dash}
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      <StatusBadge status={vm.status} />
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      <span className={`font-mono text-[11px] uppercase tracking-wider ${vm.nimbus_managed ? 'text-good' : 'text-ink-3'}`}>
                        {vm.nimbus_managed ? 'NIMBUS' : 'EXTERNAL'}
                      </span>
                    </td>
                    <td className="px-4 py-3">
                      {vm.nimbus_managed && vm.username && vm.ip ? (
                        <CopyButton value={`ssh ${vm.username}@${vm.ip}`} label="COPY SSH" />
                      ) : dash}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  )
}
