import { useCallback, useEffect, useMemo, useState } from 'react'
import { adminDeleteVM, getClusterStats, listClusterVMs, listIPs, listNodes } from '@/api/client'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import DeleteVMConfirm from '@/components/ui/DeleteVMConfirm'
import OSIcon from '@/components/ui/OSIcon'
import Pagination from '@/components/ui/Pagination'
import SSHDetailsModal, { type SSHTarget } from '@/components/ui/SSHDetailsModal'
import StatusBadge from '@/components/ui/StatusBadge'
import TunnelsModal from '@/components/ui/TunnelsModal'
import UsageBar from '@/components/ui/UsageBar'
import VMDetailsPopover from '@/components/ui/VMDetailsPopover'
import { humanizeOSTemplate, resolveOSId } from '@/lib/os'
import { formatBytes, formatRelativeTime } from '@/lib/format'
import type { ClusterStats, ClusterVM, ClusterVMStatus, IPAllocation, IPSource, IPStatus, NodeView, TierName, VMSource } from '@/types'

interface AdminData {
  nodes: NodeView[]
  vms: ClusterVM[]
  ips: IPAllocation[]
  clusterStats: ClusterStats | null
  // statsLoading is true while the lightweight overview (nodes, IPs, cluster
  // stats) is being fetched. vmsLoading is its own flag because /cluster/vms
  // can be much slower (cold qemu-agent OS-info cache after a restart, etc.)
  // — splitting the two lets the stats grid render while the VM table is
  // still spinning.
  statsLoading: boolean
  vmsLoading: boolean
  statsError: string | null
  vmsError: string | null
  // removeVM optimistically drops a row from local state after a successful
  // admin delete, so the table reflects the change immediately instead of
  // waiting for the next 15s poll.
  removeVM: (id: number) => void
}

// pollEachWith fires `fn` immediately, then every `intervalMs`, until the
// returned cleanup is invoked. The cleanup is effect-safe: ignores results
// from in-flight calls. Used twice below — once for the overview triple and
// once for the heavy /cluster/vms walk.
function usePoll<T>(
  fn: () => Promise<T>,
  intervalMs: number,
  onResult: (v: T) => void,
  onError: (e: string) => void,
  onSettled: () => void,
) {
  useEffect(() => {
    let cancelled = false
    const tick = () => {
      fn()
        .then((v) => {
          if (!cancelled) {
            onResult(v)
            onError('')
          }
        })
        .catch((e: unknown) => {
          if (!cancelled) onError(e instanceof Error ? e.message : String(e))
        })
        .finally(() => {
          if (!cancelled) onSettled()
        })
    }
    tick()
    const id = setInterval(tick, intervalMs)
    return () => {
      cancelled = true
      clearInterval(id)
    }
    // The callback set is stable for the lifetime of the page; deps lint is
    // disabled here intentionally because including the callbacks would
    // re-create the interval on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
}

function useAdminData(): AdminData {
  const [nodes, setNodes] = useState<NodeView[]>([])
  const [vms, setVMs] = useState<ClusterVM[]>([])
  const [ips, setIPs] = useState<IPAllocation[]>([])
  const [clusterStats, setClusterStats] = useState<ClusterStats | null>(null)
  const [statsLoading, setStatsLoading] = useState(true)
  const [vmsLoading, setVmsLoading] = useState(true)
  const [statsError, setStatsError] = useState<string | null>(null)
  const [vmsError, setVmsError] = useState<string | null>(null)

  // Overview triple — fast endpoints (DB-only or thin Proxmox calls).
  usePoll(
    () => Promise.all([listNodes(), listIPs(), getClusterStats()]),
    15_000,
    ([n, i, s]) => {
      setNodes(n)
      setIPs(i)
      setClusterStats(s)
    },
    (e) => setStatsError(e || null),
    () => setStatsLoading(false),
  )

  // Heavy walk — qemu-agent probes per running VM. Polls on its own cadence
  // so a slow response doesn't delay the overview tile rendering.
  usePoll(
    () => listClusterVMs(),
    15_000,
    (v) => setVMs(v),
    (e) => setVmsError(e || null),
    () => setVmsLoading(false),
  )

  const removeVM = useCallback((id: number) => {
    setVMs((prev) => prev.filter((v) => v.id !== id))
  }, [])

  return { nodes, vms, ips, clusterStats, statsLoading, vmsLoading, statsError, vmsError, removeVM }
}

interface FilterState {
  node: string | null
  status: ClusterVMStatus | null
  tier: TierName | null
}

const EMPTY_FILTERS: FilterState = { node: null, status: null, tier: null }

const headerCellClass =
  'text-left text-[11px] font-mono uppercase tracking-wider text-ink-3 px-4 py-3 whitespace-nowrap'

export default function Admin() {
  const { nodes, vms, ips, clusterStats, statsLoading, vmsLoading, statsError, vmsError, removeVM } = useAdminData()
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

  // Sort nodes deterministically so the cards don't reshuffle on every poll.
  // /api/nodes returns whatever order Proxmox walks, which jitters the grid.
  // Online-first then alphabetical: live capacity stays at the top of the
  // grid, offline/unknown nodes drop below where they belong.
  const sortedNodes = useMemo(() => {
    const statusOrder = { online: 0, unknown: 1, offline: 2 }
    return [...nodes].sort((a, b) => {
      const byStatus = (statusOrder[a.status] ?? 3) - (statusOrder[b.status] ?? 3)
      if (byStatus !== 0) return byStatus
      return a.name.localeCompare(b.name)
    })
  }, [nodes])

  const allNodes = useMemo(() => [...new Set(vms.map((v) => v.node))].sort(), [vms])
  const allTiers = useMemo<TierName[]>(() => {
    const order: TierName[] = ['small', 'medium', 'large', 'xl']
    const found = new Set(vms.map((v) => v.tier))
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

      {statsLoading && (
        <p className="mt-8 text-ink-3 font-mono text-sm">Loading overview…</p>
      )}
      {statsError && (
        <Card className="mt-8 p-6 text-bad text-sm">Failed to load overview: {statsError}</Card>
      )}

      {!statsLoading && (
        <>
          <SummaryStats stats={stats} />

          <div className="mt-8 mb-2">
            <div className="eyebrow">Nodes</div>
          </div>
          <NodeCardGrid
            nodes={sortedNodes}
            activeNode={filters.node}
            onNodeClick={setNodeFilter}
          />

          <div className="mt-10 mb-2">
            <div className="eyebrow">
              {vmsLoading
                ? 'loading machines…'
                : `${filteredVMs.length} machine${filteredVMs.length === 1 ? '' : 's'}`}
            </div>
            <h3 className="text-xl">Virtual machines</h3>
          </div>
          {vmsError && (
            <Card className="mt-2 p-4 text-bad text-sm">
              Failed to load machine table: {vmsError}
            </Card>
          )}
          {vmsLoading && vms.length === 0 ? (
            <Card className="mt-2 p-6 text-ink-3 font-mono text-sm">
              Walking nodes for VM details… this can take a few seconds on the first load after a restart while the qemu-agent OS-info cache is cold.
            </Card>
          ) : (
            <VMTable
              vms={filteredVMs}
              allVMs={vms}
              allNodes={allNodes}
              allTiers={allTiers}
              filters={filters}
              onFilterChange={(patch) => setFilters((f) => ({ ...f, ...patch }))}
              hasFilters={hasFilters}
              onClearFilters={clearFilters}
              onVMDeleted={removeVM}
            />
          )}

          <div className="mt-10 mb-2">
            <div className="eyebrow">
              {ips.length} address{ips.length === 1 ? '' : 'es'}
            </div>
            <h3 className="text-xl">IP pool</h3>
          </div>
          <IPTable ips={ips} />
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

// osLabelFor returns the user-facing OS string for a VM row. Priority:
//   1. Agent osinfo `version-id` ("22.04") prefixed by distro name from agent
//      `id` — most accurate, available when qemu-guest-agent is running.
//   2. Nimbus os_template ("ubuntu-22.04" → "Ubuntu 22.04").
//   3. Raw Proxmox ostype hint (l26/win10) humanized to "Linux"/"Windows 10".
//   4. Empty when nothing is known — caller renders a dash.
function osLabelFor(vm: ClusterVM): string {
  if (vm.os_id && vm.os_version_id) {
    const distro = vm.os_id[0].toUpperCase() + vm.os_id.slice(1)
    return `${distro} ${vm.os_version_id}`
  }
  if (vm.os_pretty) return vm.os_pretty
  if (vm.os_template) return humanizeOSTemplate(vm.os_template)
  return ''
}

function SourceLabel({ source }: { source: VMSource }) {
  // Three states: local (this Nimbus), foreign (another Nimbus on the same
  // cluster), external (not Nimbus-provisioned). Foreign and local share the
  // green tone since both are Nimbus-managed; foreign carries a sub-label so
  // admins can tell whose instance owns the credentials.
  switch (source) {
    case 'local':
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-good">
          NIMBUS
        </span>
      )
    case 'foreign':
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-good">
          NIMBUS <span className="text-ink-3">· FOREIGN</span>
        </span>
      )
    default:
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-ink-3">
          EXTERNAL
        </span>
      )
  }
}

// Small inline icons for the per-row action buttons. Kept inline (not a
// shared component) because they're styled exactly for the 14×14 button
// slot and don't carry over anywhere else in the app.
function TerminalIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <rect x="1.5" y="2.5" width="13" height="11" rx="1.5" />
      <path d="M4 6l2.5 2L4 10" />
      <path d="M8.5 10.5h3.5" />
    </svg>
  )
}

function NetworkIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <circle cx="8" cy="8" r="6" />
      <ellipse cx="8" cy="8" rx="3" ry="6" />
      <path d="M2 8h12" />
    </svg>
  )
}

function TrashIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d="M3 4h10" />
      <path d="M6 4V2.75A.75.75 0 0 1 6.75 2h2.5a.75.75 0 0 1 .75.75V4" />
      <path d="M4.5 4l.75 9a1 1 0 0 0 1 .9h3.5a1 1 0 0 0 1-.9L11.5 4" />
      <path d="M6.5 7v4M9.5 7v4" />
    </svg>
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
  onVMDeleted,
}: {
  vms: ClusterVM[]
  allVMs: ClusterVM[]
  allNodes: string[]
  allTiers: TierName[]
  filters: FilterState
  onFilterChange: (patch: Partial<FilterState>) => void
  hasFilters: boolean
  onClearFilters: () => void
  onVMDeleted: (id: number) => void
}) {
  const [sshTarget, setSshTarget] = useState<SSHTarget | null>(null)
  const [tunnelsTarget, setTunnelsTarget] = useState<{ vmId: number; hostname: string } | null>(null)
  const [editTarget, setEditTarget] = useState<ClusterVM | null>(null)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(10)
  // VMID column doubles as the sort control. Default desc so the most
  // recently-provisioned VM (highest VMID) is at the top of the table —
  // matches what admins usually want to see first after a fresh provision.
  const [vmidSortDir, setVmidSortDir] = useState<'asc' | 'desc'>('desc')

  const sortedVMs = useMemo(() => {
    const copy = [...vms]
    copy.sort((a, b) =>
      vmidSortDir === 'asc' ? a.vmid - b.vmid : b.vmid - a.vmid,
    )
    return copy
  }, [vms, vmidSortDir])

  // Clamp the current page when the filtered set shrinks (filter change,
  // VMs disappearing). Without this you can land on "page 5 of 2" after
  // filtering and the table goes empty.
  useEffect(() => {
    const maxPage = Math.max(0, Math.ceil(sortedVMs.length / pageSize) - 1)
    if (page > maxPage) setPage(maxPage)
  }, [sortedVMs.length, pageSize, page])

  const pagedVMs = useMemo(
    () => sortedVMs.slice(page * pageSize, (page + 1) * pageSize),
    [sortedVMs, page, pageSize],
  )

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
                {['Name'].map((col) => (
                  <th key={col} className={headerCellClass}>{col}</th>
                ))}
                <th className={headerCellClass}>
                  <button
                    type="button"
                    onClick={() => setVmidSortDir((d) => (d === 'asc' ? 'desc' : 'asc'))}
                    className="inline-flex items-center gap-1.5 hover:text-ink transition-colors uppercase tracking-wider"
                    title={`Sort by VMID ${vmidSortDir === 'asc' ? 'descending' : 'ascending'}`}
                  >
                    <span>VMID</span>
                    <span className="text-[9px]" aria-hidden>
                      {vmidSortDir === 'asc' ? '▲' : '▼'}
                    </span>
                  </button>
                </th>
                {['Node', 'IP', 'Tier', 'OS', 'Status', 'Source', 'SSH', 'Actions'].map((col) => (
                  <th key={col} className={headerCellClass}>{col}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {pagedVMs.map((vm) => {
                const displayName = vm.hostname || vm.name
                const dash = <span className="text-ink-3">—</span>
                const osFamily = resolveOSId({
                  agentId: vm.os_id,
                  name: vm.name,
                  template: vm.os_template,
                  ostype: vm.os_template, // external VMs put raw ostype here
                })
                const osLabel = osLabelFor(vm)
                return (
                  <tr key={`${vm.node}-${vm.vmid}`} className="border-t border-line hover:bg-[rgba(27,23,38,0.02)]">
                    <td className="px-4 py-3 font-display font-medium whitespace-nowrap">
                      <VMDetailsPopover vm={vm}>{displayName}</VMDetailsPopover>
                    </td>
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
                    <td className="px-4 py-3 whitespace-nowrap">
                      {osLabel ? (
                        <span className="inline-flex items-center gap-1.5">
                          <OSIcon family={osFamily} className="text-ink-2" />
                          <span className="font-mono text-xs text-ink-2">{osLabel}</span>
                        </span>
                      ) : dash}
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      <StatusBadge status={vm.status} />
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      <SourceLabel source={vm.source} />
                    </td>
                    <td className="px-4 py-3">
                      {vm.source === 'local' && vm.username && vm.ip ? (
                        <button
                          type="button"
                          onClick={() =>
                            setSshTarget({
                              hostname: vm.hostname || vm.name,
                              ip: vm.ip!,
                              username: vm.username!,
                              vmid: vm.vmid,
                              node: vm.node,
                              dbId: vm.id,
                              keyName: vm.key_name,
                              tunnelUrl: vm.tunnel_url,
                            })
                          }
                          className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-mono text-[11px] tracking-wider uppercase border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
                          title={`SSH details for ${vm.hostname || vm.name}`}
                          aria-label={`SSH details for ${vm.hostname || vm.name}`}
                        >
                          <TerminalIcon />
                          <span>SSH</span>
                        </button>
                      ) : dash}
                    </td>
                    <td className="px-4 py-3">
                      {vm.source === 'local' && vm.id !== undefined ? (
                        <div className="flex gap-1.5">
                          {vm.tunnel_url && (
                            <button
                              type="button"
                              onClick={() =>
                                setTunnelsTarget({
                                  vmId: vm.id!,
                                  hostname: vm.hostname || vm.name,
                                })
                              }
                              className="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors"
                              title="Manage Gopher tunnels for this VM"
                              aria-label={`Manage tunnels for ${vm.hostname || vm.name}`}
                            >
                              <NetworkIcon />
                            </button>
                          )}
                          <button
                            type="button"
                            onClick={() => setEditTarget(vm)}
                            className="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-bad hover:border-bad hover:bg-[rgba(184,58,58,0.06)] transition-colors"
                            title={`Delete ${vm.hostname || vm.name}`}
                            aria-label={`Delete ${vm.hostname || vm.name}`}
                          >
                            <TrashIcon />
                          </button>
                        </div>
                      ) : dash}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          <Pagination
            total={vms.length}
            page={page}
            pageSize={pageSize}
            onPageChange={setPage}
            onPageSizeChange={(n) => { setPageSize(n); setPage(0) }}
          />
        </Card>
      )}
      {sshTarget && (
        <SSHDetailsModal target={sshTarget} onClose={() => setSshTarget(null)} />
      )}
      {tunnelsTarget && (
        <TunnelsModal
          vmId={tunnelsTarget.vmId}
          hostname={tunnelsTarget.hostname}
          onClose={() => setTunnelsTarget(null)}
        />
      )}
      {editTarget && editTarget.id !== undefined && (
        <DeleteVMConfirm
          vm={{
            id: editTarget.id,
            hostname: editTarget.hostname || editTarget.name,
            ip: editTarget.ip || '',
            vmid: editTarget.vmid,
            node: editTarget.node,
          }}
          onConfirm={adminDeleteVM}
          onCancel={() => setEditTarget(null)}
          onDeleted={() => {
            const id = editTarget.id!
            setEditTarget(null)
            onVMDeleted(id)
          }}
        />
      )}
    </div>
  )
}

// ipToOctets converts an IPv4 string to a number suitable for sorting. Returns
// 0 for unparseable input — those land at the top of the table where they're
// easy to spot. IPv6 isn't sorted numerically (TODO if it becomes relevant).
function ipToOctets(ip: string): number {
  const parts = ip.split('.')
  if (parts.length !== 4) return 0
  let n = 0
  for (const p of parts) {
    const v = parseInt(p, 10)
    if (!Number.isFinite(v) || v < 0 || v > 255) return 0
    n = n * 256 + v
  }
  return n
}

interface IPFilterState {
  status: IPStatus | null
  source: IPSource | null
}

const EMPTY_IP_FILTERS: IPFilterState = { status: null, source: null }

function IPTable({ ips }: { ips: IPAllocation[] }) {
  // Default-hide free rows. A typical /24 pool has ~240 entries; the
  // interesting ones are reserved + allocated. Admins flip this on when
  // they want to see what's still available.
  const [showFree, setShowFree] = useState(false)
  const [filters, setFilters] = useState<IPFilterState>(EMPTY_IP_FILTERS)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(10)

  const sorted = useMemo(() => {
    return [...ips].sort((a, b) => ipToOctets(a.ip) - ipToOctets(b.ip))
  }, [ips])

  const filtered = useMemo(() => {
    return sorted.filter((row) => {
      if (!showFree && row.status === 'free') return false
      if (filters.status && row.status !== filters.status) return false
      if (filters.source && row.source !== filters.source) return false
      return true
    })
  }, [sorted, showFree, filters])

  // Clamp page when filtered set shrinks (filter change, IPs released).
  useEffect(() => {
    const maxPage = Math.max(0, Math.ceil(filtered.length / pageSize) - 1)
    if (page > maxPage) setPage(maxPage)
  }, [filtered.length, pageSize, page])

  const paged = useMemo(
    () => filtered.slice(page * pageSize, (page + 1) * pageSize),
    [filtered, page, pageSize],
  )

  const counts = useMemo(() => {
    let allocated = 0
    let reserved = 0
    let external = 0
    let free = 0
    for (const row of ips) {
      switch (row.status) {
        case 'allocated':
          allocated++
          if (row.source === 'external') external++
          break
        case 'reserved': reserved++; break
        case 'free': free++; break
      }
    }
    return { allocated, reserved, external, free }
  }, [ips])

  const hasFilters = Object.values(filters).some(Boolean) || showFree
  const clearFilters = () => {
    setFilters(EMPTY_IP_FILTERS)
    setShowFree(false)
  }

  if (ips.length === 0) {
    return (
      <Card className="py-16 text-center">
        <div className="eyebrow">Empty pool</div>
        <p className="text-sm text-ink-2 mt-2">No IPs are configured in the pool range.</p>
      </Card>
    )
  }

  const selectClass =
    'rounded-[8px] bg-white/85 font-sans text-sm text-ink border border-line-2 px-3 py-1.5 focus:outline-none'

  return (
    <div>
      <div className="flex flex-wrap gap-2 mb-4 items-center">
        <select
          className={selectClass}
          value={filters.status ?? ''}
          onChange={(e) => setFilters((f) => ({ ...f, status: (e.target.value as IPStatus) || null }))}
        >
          <option value="">All statuses</option>
          <option value="allocated">Allocated</option>
          <option value="reserved">Reserved</option>
          <option value="free">Free</option>
        </select>
        <select
          className={selectClass}
          value={filters.source ?? ''}
          onChange={(e) => setFilters((f) => ({ ...f, source: (e.target.value as IPSource) || null }))}
        >
          <option value="">All sources</option>
          <option value="local">Nimbus (local)</option>
          <option value="adopted">Nimbus (foreign)</option>
          <option value="external">External (LAN)</option>
        </select>
        <label className="inline-flex items-center gap-1.5 font-mono text-xs text-ink-2">
          <input
            type="checkbox"
            className="accent-ink"
            checked={showFree}
            onChange={(e) => setShowFree(e.target.checked)}
          />
          Show free
        </label>
        <span className="ml-auto font-mono text-[11px] text-ink-3 uppercase tracking-wider">
          {counts.allocated} allocated · {counts.reserved} reserved · {counts.external} external · {counts.free} free
        </span>
        {hasFilters && (
          <Button variant="ghost" size="small" onClick={clearFilters}>
            Clear filters
          </Button>
        )}
      </div>

      {filtered.length === 0 ? (
        <Card className="py-16 text-center">
          <div className="eyebrow">No results</div>
          <p className="text-sm text-ink-2 mt-2">No IPs match the current filters.</p>
          <Button variant="ghost" size="small" className="mt-4" onClick={clearFilters}>
            Clear filters
          </Button>
        </Card>
      ) : (
        <Card className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line">
                {['IP', 'Status', 'Source', 'VMID', 'Hostname', 'Last seen'].map((col) => (
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
              {paged.map((row) => (
                <IPRow key={row.ip} row={row} />
              ))}
            </tbody>
          </table>
          <Pagination
            total={filtered.length}
            page={page}
            pageSize={pageSize}
            onPageChange={setPage}
            onPageSizeChange={(n) => { setPageSize(n); setPage(0) }}
          />
        </Card>
      )}
    </div>
  )
}

function IPRow({ row }: { row: IPAllocation }) {
  const dash = <span className="text-ink-3">—</span>
  const lastSeen = row.last_seen_at || row.allocated_at || row.reserved_at || null
  return (
    <tr className="border-t border-line hover:bg-[rgba(27,23,38,0.02)]">
      <td className="px-4 py-3 font-mono text-xs text-ink whitespace-nowrap">{row.ip}</td>
      <td className="px-4 py-3 whitespace-nowrap">
        <IPStatusBadge status={row.status} />
      </td>
      <td className="px-4 py-3 whitespace-nowrap">
        {row.status === 'free' ? dash : <IPSourceLabel source={row.source} />}
      </td>
      <td className="px-4 py-3 font-mono text-xs text-ink-2 whitespace-nowrap">
        {row.vmid ?? dash}
      </td>
      <td className="px-4 py-3 font-mono text-xs text-ink-2 whitespace-nowrap">
        {row.hostname || dash}
      </td>
      <td className="px-4 py-3 font-mono text-xs text-ink-3 whitespace-nowrap">
        {lastSeen ? (
          <span title={lastSeen}>
            {formatRelativeTime(lastSeen)}
            {row.missed_cycles && row.missed_cycles > 0 ? (
              <span className="ml-1.5 text-warn">
                · {row.missed_cycles} miss{row.missed_cycles === 1 ? '' : 'es'}
              </span>
            ) : null}
          </span>
        ) : dash}
      </td>
    </tr>
  )
}

function IPStatusBadge({ status }: { status: IPStatus }) {
  const tone = status === 'allocated'
    ? 'text-good'
    : status === 'reserved'
      ? 'text-warn'
      : 'text-ink-3'
  return (
    <span className={`font-mono text-[11px] uppercase tracking-wider ${tone}`}>
      {status}
    </span>
  )
}

function IPSourceLabel({ source }: { source: IPSource | undefined }) {
  switch (source) {
    case 'local':
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-good">
          NIMBUS
        </span>
      )
    case 'adopted':
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-good">
          NIMBUS <span className="text-ink-3">· FOREIGN</span>
        </span>
      )
    case 'external':
      return (
        <span className="font-mono text-[11px] uppercase tracking-wider text-ink-2">
          EXTERNAL
        </span>
      )
    default:
      return <span className="text-ink-3">—</span>
  }
}
