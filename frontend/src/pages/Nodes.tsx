import { useCallback, useEffect, useMemo, useState } from 'react'
import { getProxmoxBinding, listNodes, listNodesWithScores } from '@/api/client'
import type { ProxmoxBinding } from '@/api/client'
import type { NodeView, NodeViewWithScores, Specialization, TierName } from '@/types'
import { CordonModal, DrainPlanModal, RemoveNodeModal, TagsModal } from '@/components/NodeActionModals'
import ProxmoxBindingModal, { ChangeBindingModal } from '@/components/ProxmoxBindingModal'
import { formatBytes } from '@/lib/format'

const TIER_PREVIEW_ORDER: TierName[] = ['small', 'medium', 'large', 'xl']

// Nodes — admin-only cluster lifecycle page. Three stacked sections:
//
//   1. Connected-to bar (binding row + Change… button)
//   2. Compact card grid — at-a-glance health & capacity per node, no
//      action chrome on the cards themselves so they read as a dashboard
//   3. Management table — dense rows with action buttons inline; the
//      operator's actual workspace for cordon / drain / tags / remove
//
// Two views of the same data is intentional: the cards optimize for
// visual scan ("which node is hot?") while the table optimizes for
// dispatch ("cordon pve-3 now").
export default function Nodes() {
  const [rows, setRows] = useState<NodeView[] | null>(null)
  const [binding, setBinding] = useState<ProxmoxBinding | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction>(null)
  const [bindingDetailOpen, setBindingDetailOpen] = useState(false)
  const [bindingChangeOpen, setBindingChangeOpen] = useState(false)

  const reload = useCallback(() => {
    listNodes()
      .then(setRows)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
    getProxmoxBinding()
      .then(setBinding)
      .catch(() => { /* keep last binding on transient error */ })
  }, [])

  useEffect(() => { reload() }, [reload])
  useEffect(() => {
    const id = setInterval(reload, 15_000)
    return () => clearInterval(id)
  }, [reload])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 24 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Nodes
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Cordon to stop new provisions on a node; drain to migrate every
          managed VM off before reclaiming the host. Cards summarise; the
          table below dispatches actions.
        </p>
      </div>

      {binding && (
        <ConnectionBar
          binding={binding}
          onDetail={() => setBindingDetailOpen(true)}
          onChange={() => setBindingChangeOpen(true)}
        />
      )}

      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}

      {rows === null && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      )}

      {rows !== null && rows.length === 0 && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          No nodes visible — Proxmox returned an empty cluster.
        </p>
      )}

      {rows !== null && rows.length > 0 && (
        <>
          <CardGrid rows={rows} />
          <ManageTable rows={rows} onAction={setPending} />
          <ScoringMatrix />
        </>
      )}

      {bindingDetailOpen && binding && (
        <ProxmoxBindingModal binding={binding} onClose={() => setBindingDetailOpen(false)} />
      )}
      {bindingChangeOpen && binding && (
        <ChangeBindingModal current={binding} onClose={() => setBindingChangeOpen(false)} />
      )}

      {pending?.kind === 'cordon' && (
        <CordonModal
          node={pending.node}
          onClose={() => setPending(null)}
          onMutated={() => { setPending(null); reload() }}
        />
      )}
      {pending?.kind === 'drain' && (
        <DrainPlanModal
          nodeName={pending.node.name}
          onClose={() => setPending(null)}
          onComplete={() => { setPending(null); reload() }}
        />
      )}
      {pending?.kind === 'remove' && (
        <RemoveNodeModal
          node={pending.node}
          onClose={() => setPending(null)}
          onMutated={() => { setPending(null); reload() }}
        />
      )}
      {pending?.kind === 'tags' && (
        <TagsModal
          node={pending.node}
          onClose={() => setPending(null)}
          onMutated={() => { setPending(null); reload() }}
        />
      )}
    </div>
  )
}

type PendingAction =
  | { kind: 'cordon' | 'drain' | 'remove' | 'tags'; node: NodeView }
  | null

// ConnectionBar — two-line indicator for the active Proxmox binding.
// Top line is the primary identity (status dot + connected node name);
// secondary line carries the operational details (cluster, host URL,
// version, node count). Edit icon on the right opens the reconfigure
// modal; clicking the body opens the read-only detail modal. Splitting
// the click targets keeps the common path (read) from triggering the
// rare path (write).
function ConnectionBar({
  binding,
  onDetail,
  onChange,
}: {
  binding: ProxmoxBinding
  onDetail: () => void
  onChange: () => void
}) {
  const reachable = binding.reachable !== false
  const primary = binding.connected_node || hostFromURL(binding.host) || 'Proxmox'
  return (
    <div
      className="glass"
      style={{
        padding: '14px 18px',
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        background: 'rgba(255,255,255,0.65)',
      }}
    >
      <button
        type="button"
        onClick={onDetail}
        title="Click for cluster details"
        style={{
          flex: 1,
          minWidth: 0,
          background: 'transparent',
          border: 'none',
          padding: 0,
          textAlign: 'left',
          cursor: 'pointer',
          display: 'flex',
          flexDirection: 'column',
          gap: 2,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span
            aria-hidden="true"
            style={{
              width: 8, height: 8, borderRadius: 4, flexShrink: 0,
              background: reachable ? 'var(--ok)' : 'var(--err)',
            }}
          />
          <span style={{ fontSize: 11, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em' }}>
            Connected to
          </span>
          <span style={{ fontSize: 15, fontWeight: 500, color: 'var(--ink)' }}>
            {primary}
          </span>
        </div>
        <div style={{ fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace', paddingLeft: 18, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {binding.cluster_name && <>cluster {binding.cluster_name} · </>}
          {binding.host}
          {binding.version && <> · pve {binding.version}</>}
          {' · '}{binding.node_count} {binding.node_count === 1 ? 'node' : 'nodes'}
        </div>
      </button>
      <button
        type="button"
        onClick={onChange}
        title="Change Proxmox connection"
        aria-label="Change Proxmox connection"
        style={{
          width: 32, height: 32, borderRadius: 6,
          border: '1px solid var(--line)',
          background: 'transparent',
          color: 'var(--ink-2)',
          cursor: 'pointer',
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          transition: 'background 100ms, color 100ms',
        }}
        onMouseEnter={(e) => {
          e.currentTarget.style.background = 'rgba(20,18,28,0.05)'
          e.currentTarget.style.color = 'var(--ink)'
        }}
        onMouseLeave={(e) => {
          e.currentTarget.style.background = 'transparent'
          e.currentTarget.style.color = 'var(--ink-2)'
        }}
      >
        <EditIcon />
      </button>
    </div>
  )
}

function EditIcon() {
  return (
    <svg width={14} height={14} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M11 4H4a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 113 3L12 15l-4 1 1-4 9.5-9.5z" />
    </svg>
  )
}

function hostFromURL(raw: string): string {
  try {
    const u = new URL(raw)
    return u.hostname
  } catch {
    return raw
  }
}

// CardGrid — visual at-a-glance summaries. Compact (3 columns at xl,
// 2 at lg, 1 stacked) so a 6-node cluster fits on one screen. Each
// card trades depth for density: just status + lock + the four key
// utilization figures. The management table below carries the rest.
function CardGrid({ rows }: { rows: NodeView[] }) {
  const sorted = useMemo(() => {
    return [...rows].sort((a, b) => {
      if (a.status !== b.status) {
        if (a.status === 'online') return -1
        if (b.status === 'online') return 1
      }
      return a.name.localeCompare(b.name)
    })
  }, [rows])
  return (
    // Density ramps with viewport: 1 col mobile, 2 col tablet, 3 col
    // small desktop, 4 col xl. Cards have just enough fixed content
    // (header + 5 thin bars) that 4-up at ~280-300px each stays
    // legible — better at-a-glance density for clusters of 6+ nodes.
    <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3">
      {sorted.map((n) => <NodeCard key={n.name} node={n} />)}
    </div>
  )
}

function NodeCard({ node: n }: { node: NodeView }) {
  const cpuPct = n.cpu * 100
  const memPct = n.mem_total > 0 ? (n.mem_used / n.mem_total) * 100 : 0
  const memAllocPct = n.mem_total > 0 ? (n.mem_allocated / n.mem_total) * 100 : 0
  const diskPct = n.disk_total > 0 ? (n.disk_used / n.disk_total) * 100 : 0
  const swapping = n.swap_used > 10 * 1024 * 1024
  const offline = n.status !== 'online'

  return (
    <div
      className="glass"
      style={{
        padding: '16px 18px',
        display: 'flex',
        flexDirection: 'column',
        gap: 12,
        opacity: offline ? 0.55 : 1,
      }}
    >
      {/* Header — name + tier counts on the left, status pills on the right. */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 10 }}>
        <div style={{ minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.name}</span>
            {n.is_self_host && <SelfPill />}
          </div>
          <div style={{ fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace', marginTop: 2 }}>
            {n.max_cpu}c · {formatBytes(n.mem_total)}
            {n.disk_total > 0 && <> · {formatBytes(n.disk_total)} {n.disk_pool_name || 'disk'}</>}
            {' · '}{n.vm_count}/{n.vm_count_total} VM{n.vm_count_total !== 1 ? 's' : ''}
          </div>
          {(n.cpu_model || n.cpu_mhz) && (
            <div
              style={{ fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace', marginTop: 2, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
              title={n.cpu_model || ''}
            >
              {shortCPUModel(n.cpu_model)}
              {n.cpu_mhz ? ` @ ${(n.cpu_mhz / 1000).toFixed(1)} GHz` : ''}
            </div>
          )}
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 4 }}>
          <div style={{ display: 'flex', gap: 4 }}>
            <LockChip state={n.lock_state} reason={n.lock_reason} />
            <StatusDot status={n.status} />
          </div>
          {swapping && (
            <span
              title="Allocated memory exceeded physical RAM — host is paging to swap"
              className="font-mono"
              style={{
                fontSize: 9, padding: '1px 5px', borderRadius: 3,
                color: '#9a5c2e', background: 'rgba(248,175,130,0.15)',
                border: '1px solid rgba(248,175,130,0.4)',
                textTransform: 'uppercase', letterSpacing: '0.06em',
              }}
            >swap +{formatBytes(n.swap_used)}</span>
          )}
        </div>
      </div>

      {/* Compact bar stack. Both used + allocated for RAM and disk —
          they answer different questions (used = live pressure right
          now, allocated = "if every VM filled its declared size, what
          then"). The scheduler gates on allocated, so surfacing it
          alongside used makes placement reasoning visible. */}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        <MiniBar label="CPU" pct={cpuPct} hint={`${cpuPct.toFixed(0)}%`} />
        <MiniBar label="RAM" pct={memPct} hint={`${formatBytes(n.mem_used)} / ${formatBytes(n.mem_total)}`} />
        <MiniBar label="RAM alloc" pct={memAllocPct} hint={`${memAllocPct.toFixed(0)}%`} accent />
        {n.disk_total > 0 && (
          <>
            <MiniBar label="Disk" pct={diskPct} hint={`${formatBytes(n.disk_used)} / ${formatBytes(n.disk_total)}`} />
            <MiniBar
              label="Disk alloc"
              pct={n.disk_total > 0 ? (n.disk_allocated / n.disk_total) * 100 : 0}
              hint={`${formatBytes(n.disk_allocated)} / ${formatBytes(n.disk_total)}`}
              accent
            />
          </>
        )}
      </div>

      {n.tags.length > 0 && (
        <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
          {n.tags.map((t) => (
            <span key={t} style={{
              fontSize: 10, fontFamily: 'Geist Mono, monospace',
              padding: '1px 6px', borderRadius: 3,
              background: 'rgba(20,18,28,0.04)', border: '1px solid var(--line)',
              color: 'var(--ink-body)',
            }}>{t}</span>
          ))}
        </div>
      )}
    </div>
  )
}

// MiniBar — a single-line bar with the metric label on the left and the
// numeric hint on the right. Bar fill colour ramps from neutral → warn
// → bad as percent climbs; "accent" caller forces the warn palette
// regardless (used for "RAM allocated" so it visually distinguishes from
// "RAM in use" even at the same percent).
function MiniBar({ label, pct, hint, accent }: { label: string; pct: number; hint: string; accent?: boolean }) {
  const clamped = Math.max(0, Math.min(100, pct))
  let fill = 'var(--ink-mute)'
  if (accent) {
    fill = 'rgba(184,101,15,0.45)'
  } else if (clamped > 85) {
    fill = 'rgba(184,55,55,0.55)'
  } else if (clamped > 60) {
    fill = 'rgba(184,101,15,0.45)'
  } else {
    fill = 'rgba(20,18,28,0.30)'
  }
  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 10, color: 'var(--ink-mute)', marginBottom: 3 }}>
        <span style={{ textTransform: 'uppercase', letterSpacing: '0.06em', fontFamily: 'Geist Mono, monospace' }}>{label}</span>
        <span style={{ fontFamily: 'Geist Mono, monospace' }}>{hint}</span>
      </div>
      <div
        style={{
          height: 4,
          background: 'rgba(20,18,28,0.06)',
          borderRadius: 2,
          overflow: 'hidden',
        }}
      >
        <div style={{ width: `${clamped}%`, height: '100%', background: fill, transition: 'width 200ms ease' }} />
      </div>
    </div>
  )
}

// pctColor ramps a 0–100 percentage to ink/warn/err. Used for the
// "allocated" cells where high values flag overcommit risk and the
// operator's eye should snap to them.
// shortCPUModel strips the noisy boilerplate Intel/AMD pack into their
// model strings ("Intel(R) Core(TM) i7-9700K CPU @ 3.60GHz" → "Core
// i7-9700K"). Keeps the family + part number — enough to tell
// generations apart at a glance.
function shortCPUModel(raw?: string): string {
  if (!raw) return ''
  let s = raw
  s = s.replace(/\(R\)/g, '').replace(/\(TM\)/g, '')
  s = s.replace(/Intel\s+/i, '').replace(/AMD\s+/i, '')
  s = s.replace(/\s+CPU\s+@\s+\S+/i, '') // strip "CPU @ 3.60GHz" — we render mhz separately
  s = s.replace(/\s+@\s+\S+/i, '') // older format without "CPU"
  s = s.replace(/\s+/g, ' ').trim()
  return s
}

function pctColor(pct: number): string {
  if (pct > 85) return 'var(--err)'
  if (pct > 60) return '#9a5c2e'
  return 'var(--ink-body)'
}

function StatusDot({ status }: { status: NodeView['status'] }) {
  const ok = status === 'online'
  return (
    <span
      title={status}
      aria-label={status}
      style={{
        width: 8, height: 8, borderRadius: 4, marginTop: 4,
        background: ok ? 'var(--ok)' : 'var(--err)',
      }}
    />
  )
}

function LockChip({ state, reason }: { state: NodeView['lock_state']; reason?: string }) {
  if (state === 'none') return null
  const palette: Record<string, { color: string; bg: string; border: string; tip: string }> = {
    cordoned: { color: '#9a5c2e', bg: 'rgba(248,175,130,0.15)', border: 'rgba(248,175,130,0.4)', tip: 'Cordoned — scheduler skips this node, existing VMs untouched' },
    draining: { color: '#9a5c2e', bg: 'rgba(248,175,130,0.15)', border: 'rgba(248,175,130,0.4)', tip: 'Draining — migration in flight; do not touch' },
    drained:  { color: 'var(--ink-mute)', bg: 'rgba(20,18,28,0.05)', border: 'var(--line)', tip: 'Drained — no managed VMs left, ready to remove from cluster' },
  }
  const p = palette[state]
  return (
    <span
      className="font-mono"
      title={reason ? `${p.tip}\n\nReason: ${reason}` : p.tip}
      style={{
        fontSize: 9, fontWeight: 600, padding: '1px 6px', borderRadius: 3,
        textTransform: 'uppercase', letterSpacing: '0.06em',
        color: p.color, background: p.bg, border: `1px solid ${p.border}`,
      }}
    >{state}</span>
  )
}

function SelfPill() {
  return (
    <span
      className="font-mono"
      title="Nimbus runs on this node — Remove is disabled to prevent locking yourself out"
      style={{
        fontSize: 9, padding: '1px 5px', borderRadius: 3,
        color: 'var(--ink-mute)', background: 'rgba(20,18,28,0.05)',
        border: '1px solid var(--line)', textTransform: 'uppercase',
        letterSpacing: '0.06em', flexShrink: 0,
      }}
    >self</span>
  )
}

// ManageTable — dense per-row view with action buttons inline. This is
// the operator's workspace for cordon / drain / tags / remove; the
// cards above are for visual scan.
//
// Action buttons are flat-styled (text + icon, no border) and appear
// inline rather than behind a "..." dropdown so each row's action
// affordance is visible without an extra click. Buttons grey out when
// they're not valid for the current lock state (e.g. Drain on a
// drained node).
function ManageTable({ rows, onAction }: { rows: NodeView[]; onAction: (a: PendingAction) => void }) {
  const sorted = useMemo(
    () => [...rows].sort((a, b) => a.name.localeCompare(b.name)),
    [rows],
  )
  return (
    <div className="glass" style={{ padding: '20px 24px', display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>
          Manage
        </span>
        <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>
          {rows.length} {rows.length === 1 ? 'node' : 'nodes'}
        </span>
      </div>
      <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
        <table className="w-full text-left" style={{ fontSize: 12, borderCollapse: 'collapse' }}>
          <thead>
            <tr style={{ color: 'var(--ink-mute)', fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
              <th style={{ padding: '6px 8px', fontWeight: 500 }}>Node</th>
              <th style={{ padding: '6px 8px', fontWeight: 500 }}>Lock</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }}>CPU</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }} title="RAM in use right now">RAM</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }} title="RAM allocated (sum of every VM's configured maxmem)">RAM alloc</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }} title="Disk in use right now (thin-provisioned written bytes)">Disk</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }} title="Disk allocated (sum of every VM's configured maxdisk)">Disk alloc</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }}>VMs</th>
              <th style={{ padding: '6px 8px', fontWeight: 500 }}>Tags</th>
              <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((n) => (
              <ManageRow key={n.name} node={n} onAction={onAction} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function ManageRow({ node: n, onAction }: { node: NodeView; onAction: (a: PendingAction) => void }) {
  const cpuPct = n.cpu * 100
  const memPct = n.mem_total > 0 ? (n.mem_used / n.mem_total) * 100 : 0
  const memAllocPct = n.mem_total > 0 ? (n.mem_allocated / n.mem_total) * 100 : 0
  const diskPct = n.disk_total > 0 ? (n.disk_used / n.disk_total) * 100 : 0
  const diskAllocPct = n.disk_total > 0 ? (n.disk_allocated / n.disk_total) * 100 : 0
  return (
    <tr style={{ borderTop: '1px solid var(--line)', opacity: n.status === 'online' ? 1 : 0.55 }}>
      <td style={{ padding: '8px 8px' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 500, color: 'var(--ink)' }}>
          <StatusDot status={n.status} />
          {n.name}
          {n.is_self_host && <SelfPill />}
        </div>
        {n.ip && <div style={{ fontSize: 10, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace', marginTop: 1 }}>{n.ip}</div>}
      </td>
      <td style={{ padding: '8px 8px' }}>
        {n.lock_state === 'none'
          ? <span style={{ color: 'var(--ink-mute)', fontSize: 10 }}>—</span>
          : <LockChip state={n.lock_state} reason={n.lock_reason} />}
      </td>
      <td style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: 'var(--ink-body)' }}>{cpuPct.toFixed(0)}%</td>
      <td style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: 'var(--ink-body)' }}>{memPct.toFixed(0)}%</td>
      <td
        style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: pctColor(memAllocPct) }}
        title="RAM allocated to non-template VMs (sum of maxmem)"
      >
        {memAllocPct.toFixed(0)}%
      </td>
      <td
        style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: 'var(--ink-body)' }}
        title={n.disk_pool_name ? `${n.disk_pool_name} pool — bytes written` : 'no disk telemetry'}
      >
        {n.disk_total > 0 ? `${diskPct.toFixed(0)}%` : '—'}
      </td>
      <td
        style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: pctColor(diskAllocPct) }}
        title={n.disk_pool_name ? `${n.disk_pool_name} pool — sum of every VM's configured maxdisk (thin-provisioned commitment)` : 'no disk telemetry'}
      >
        {n.disk_total > 0 ? `${diskAllocPct.toFixed(0)}%` : '—'}
      </td>
      <td style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace', color: 'var(--ink-body)' }}>
        {n.vm_count}/{n.vm_count_total}
      </td>
      <td style={{ padding: '8px 8px', color: 'var(--ink-body)' }}>
        {n.tags.length === 0 ? <span style={{ color: 'var(--ink-mute)' }}>—</span> : n.tags.join(', ')}
      </td>
      <td style={{ padding: '8px 8px', textAlign: 'right' }}>
        <RowActions node={n} onAction={onAction} />
      </td>
    </tr>
  )
}

// RowActions — inline buttons for the table. Cordon/Uncordon swap based
// on lock state; Drain disabled while draining/drained; Remove only when
// drained AND not self-host. Each carries a title= tooltip explaining the
// verb so an operator unfamiliar with kubectl knows what they do.
function RowActions({ node: n, onAction }: { node: NodeView; onAction: (a: PendingAction) => void }) {
  const isCordonable = n.lock_state === 'none'
  const isUncordonable = n.lock_state === 'cordoned' || n.lock_state === 'drained'
  const isDrainable = n.lock_state === 'none' || n.lock_state === 'cordoned'
  const isRemovable = n.lock_state === 'drained' && !n.is_self_host

  return (
    <div style={{ display: 'inline-flex', gap: 2 }}>
      {isCordonable && (
        <ActionBtn
          label="Cordon"
          tip="Stop the scheduler from placing new VMs here. Existing VMs keep running. Reversible."
          onClick={() => onAction({ kind: 'cordon', node: n })}
        />
      )}
      {isUncordonable && (
        <ActionBtn
          label="Uncordon"
          tip="Allow the scheduler to place new VMs here again."
          onClick={() => onAction({ kind: 'cordon', node: n })}
        />
      )}
      <ActionBtn
        label="Drain"
        disabled={!isDrainable}
        tip={isDrainable
          ? 'Migrate every managed VM off this node. Opens a plan modal — review and override per-VM destinations before confirming.'
          : 'Already draining or drained.'}
        onClick={() => onAction({ kind: 'drain', node: n })}
      />
      <ActionBtn
        label="Tags"
        tip="Free-text labels (e.g. gpu, nvme-fast). Stored for the future workload-aware scheduler."
        onClick={() => onAction({ kind: 'tags', node: n })}
      />
      <ActionBtn
        label="Remove"
        destructive
        disabled={!isRemovable}
        tip={n.is_self_host
          ? 'Cannot remove the node Nimbus runs on.'
          : !isRemovable
            ? 'Drain the node first.'
            : 'Removes this node from the Proxmox cluster (pvecm delnode). The host itself is unaffected.'}
        onClick={() => onAction({ kind: 'remove', node: n })}
      />
    </div>
  )
}

function ActionBtn({
  label,
  tip,
  onClick,
  disabled = false,
  destructive = false,
}: {
  label: string
  tip: string
  onClick: () => void
  disabled?: boolean
  destructive?: boolean
}) {
  return (
    <button
      type="button"
      title={tip}
      disabled={disabled}
      onClick={onClick}
      style={{
        padding: '4px 8px',
        fontSize: 11,
        fontWeight: 500,
        borderRadius: 4,
        border: '1px solid transparent',
        background: 'transparent',
        color: destructive ? 'var(--err)' : 'var(--ink-2)',
        cursor: disabled ? 'not-allowed' : 'pointer',
        opacity: disabled ? 0.35 : 1,
        transition: 'background 100ms, border-color 100ms',
      }}
      onMouseEnter={(e) => {
        if (disabled) return
        e.currentTarget.style.background = destructive ? 'rgba(184,55,55,0.06)' : 'rgba(20,18,28,0.05)'
        e.currentTarget.style.borderColor = destructive ? 'rgba(184,55,55,0.2)' : 'var(--line)'
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.background = 'transparent'
        e.currentTarget.style.borderColor = 'transparent'
      }}
    >
      {label}
    </button>
  )
}

// ScoringMatrix renders the per-node × per-workload score grid. Shown
// below the management table so the page reads as: glance (cards) →
// dispatch (table) → scheduler diagnostics (matrix).
//
// The tier picker at the top drives the preview — operators ask "if
// I provisioned a medium VM, where would it land?" and the matrix
// answers across all four workload types simultaneously. Cells show
// the score as a percent, ramped green/amber/red, with a hover
// tooltip rendering the components map breakdown.
//
// Cheap to compute: one cluster snapshot reused across all
// (node, workload) combinations server-side.
function ScoringMatrix() {
  const [tier, setTier] = useState<TierName>('medium')
  const [data, setData] = useState<NodeViewWithScores[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setLoading(true)
    listNodesWithScores(tier)
      .then((rows) => { setData(rows); setError(null) })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
      .finally(() => setLoading(false))
  }, [tier])

  const sorted = useMemo(() => {
    if (!data) return []
    // Highest score first — operators want to see the best fit at the top.
    return [...data].sort((a, b) => (b.score?.score ?? 0) - (a.score?.score ?? 0))
  }, [data])

  return (
    <div className="glass" style={{ padding: '20px 24px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
        <div>
          <span style={{ fontSize: 14, fontWeight: 600, color: 'var(--ink)' }}>Placement scores</span>
          <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.5 }}>
            Where would a fresh VM at the preview tier land? Higher = more headroom after placement. Hover a score for the components breakdown.
          </p>
        </div>
        <div style={{ display: 'inline-flex', gap: 4, padding: 3, borderRadius: 8, border: '1px solid var(--line)', background: 'rgba(20,18,28,0.03)' }}>
          {TIER_PREVIEW_ORDER.map((t) => (
            <button
              key={t}
              type="button"
              onClick={() => setTier(t)}
              style={{
                fontSize: 11, padding: '4px 10px', borderRadius: 5,
                border: 'none', cursor: 'pointer',
                background: tier === t ? 'var(--ink)' : 'transparent',
                color: tier === t ? 'white' : 'var(--ink-2)',
                fontFamily: 'Geist Mono, monospace',
                textTransform: 'uppercase', letterSpacing: '0.06em', fontWeight: 500,
              }}
            >
              {t}
            </button>
          ))}
        </div>
      </div>
      {error && <p style={{ margin: 0, fontSize: 12, color: 'var(--err)' }}>{error}</p>}
      {loading && !data && (
        <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)' }}>Computing scores…</p>
      )}
      {sorted.length > 0 && (
        <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
          <table className="w-full text-left" style={{ fontSize: 12, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ color: 'var(--ink-mute)', fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
                <th style={{ padding: '6px 8px', fontWeight: 500 }}>Node</th>
                <th style={{ padding: '6px 8px', fontWeight: 500 }}>Spec</th>
                <th style={{ padding: '6px 8px', fontWeight: 500 }}>Tags</th>
                <th style={{ padding: '6px 8px', fontWeight: 500, textAlign: 'right' }}>Score</th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((n) => (
                <tr key={n.name} style={{ borderTop: '1px solid var(--line)', opacity: n.status === 'online' ? 1 : 0.55 }}>
                  <td style={{ padding: '8px 8px', fontWeight: 500, color: 'var(--ink)' }}>{n.name}</td>
                  <td style={{ padding: '8px 8px' }}>
                    <SpecChip spec={inferSpec(n)} />
                  </td>
                  <td style={{ padding: '8px 8px', color: 'var(--ink-body)' }}>
                    {n.tags.length === 0 ? <span style={{ color: 'var(--ink-mute)' }}>—</span> : (
                      <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}>
                        {n.tags.map((t) => (
                          <span key={t} style={{
                            fontSize: 10, fontFamily: 'Geist Mono, monospace',
                            padding: '1px 6px', borderRadius: 3,
                            background: 'rgba(20,18,28,0.04)', border: '1px solid var(--line)',
                          }}>{t}</span>
                        ))}
                      </span>
                    )}
                  </td>
                  <td
                    style={{ padding: '8px 8px', textAlign: 'right', fontFamily: 'Geist Mono, monospace' }}
                    title={cellTooltip(tier, n.score)}
                  >
                    <ScoreCell breakdown={n.score} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

// inferSpec falls back to deriving the specialization from the node's
// resource shape when the backend payload didn't include it (e.g. older
// API responses). Mirrors nodescore.DetectSpecialization's thresholds.
function inferSpec(n: NodeViewWithScores): Specialization {
  if (n.score?.spec) return n.score.spec
  if (n.max_cpu <= 0 || n.mem_total === 0) return 'balanced'
  const gibPerCore = n.mem_total / (1 << 30) / n.max_cpu
  if (gibPerCore < 4) return 'cpu'
  if (gibPerCore > 8) return 'memory'
  return 'balanced'
}

function SpecChip({ spec }: { spec: Specialization }) {
  const palette: Record<Specialization, { color: string; bg: string; border: string; label: string }> = {
    cpu:      { color: '#9a5c2e', bg: 'rgba(248,175,130,0.15)', border: 'rgba(248,175,130,0.4)', label: 'cpu-opt' },
    memory:   { color: '#3b6e9c', bg: 'rgba(130,175,248,0.15)', border: 'rgba(130,175,248,0.4)', label: 'mem-opt' },
    balanced: { color: 'var(--ink-mute)', bg: 'rgba(20,18,28,0.05)', border: 'var(--line)', label: 'balanced' },
  }
  const p = palette[spec]
  return (
    <span
      className="font-mono"
      style={{
        fontSize: 9, fontWeight: 600, padding: '1px 6px', borderRadius: 3,
        textTransform: 'uppercase', letterSpacing: '0.06em',
        color: p.color, background: p.bg, border: `1px solid ${p.border}`,
      }}
    >{p.label}</span>
  )
}

function ScoreCell({ breakdown }: { breakdown?: import('@/types').ScoreBreakdown }) {
  if (!breakdown) return <span style={{ color: 'var(--ink-mute)' }}>—</span>
  if (breakdown.score === 0) {
    return (
      <span style={{ color: 'var(--err)' }} title={breakdown.reasons?.join(', ')}>
        rejected
      </span>
    )
  }
  const pct = breakdown.score * 100
  const color = pct > 70 ? 'var(--ok)' : pct > 40 ? '#9a5c2e' : 'var(--err)'
  return <span style={{ color }}>{pct.toFixed(0)}%</span>
}

function cellTooltip(tier: TierName, b?: import('@/types').ScoreBreakdown): string | undefined {
  if (!b) return undefined
  if (b.score === 0) {
    return `${tier}: rejected — ${b.reasons?.join(', ') || 'no eligible reasons reported'}`
  }
  if (!b.components) return `${tier}: score ${(b.score * 100).toFixed(0)}%`
  const c = b.components
  const fmt = (k: string, label: string) => {
    const headK = `${k}_headroom`
    const wK = `${k}_weighted`
    if (c[wK] === undefined) return ''
    return `${label}(${(c[headK] ?? 0).toFixed(2)})=${(c[wK] ?? 0).toFixed(3)}`
  }
  const parts = [fmt('mem', 'mem'), fmt('cpu', 'cpu'), fmt('disk', 'disk')].filter(Boolean)
  return `${tier}: ${parts.join(' + ')} = ${(c.total ?? b.score).toFixed(3)}`
}
