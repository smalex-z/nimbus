import { useCallback, useEffect, useState } from 'react'
import { getProxmoxBinding, listNodes } from '@/api/client'
import type { ProxmoxBinding } from '@/api/client'
import type { NodeView } from '@/types'
import { CordonModal, DrainPlanModal, RemoveNodeModal, TagsModal } from '@/components/NodeActionModals'
import ProxmoxBindingModal from '@/components/ProxmoxBindingModal'
import Card from '@/components/ui/Card'
import StatusBadge from '@/components/ui/StatusBadge'
import UsageBar from '@/components/ui/UsageBar'
import NavDropdown from '@/components/ui/NavDropdown'
import { formatBytes } from '@/lib/format'

// Nodes — admin-only page for cluster lifecycle. Three sections stacked:
//
//   1. Connected-Proxmox row — surfaces "you are talking to <node> in
//      <cluster>" so the operator never wonders which cluster they're
//      looking at. Click → opens ProxmoxBindingModal with the full
//      detail (version, node count, last contact, reachability).
//   2. Per-node card grid — same visual language as the Admin dashboard
//      (UsageBars for CPU / mem-in-use / mem-allocated / swap). Adds
//      lock-state badge + actions menu (cordon / drain / tags / remove).
//   3. Modals (cordon / drain plan / tags / remove) — mounted at the
//      page level so card-level actions can dispatch into them.
//
// Lock state vocabulary follows kubectl (cordon / drain) — the row
// dropdown carries tooltips so an operator who's never used kubectl
// still knows what each verb does without leaving the page.
export default function Nodes() {
  const [rows, setRows] = useState<NodeView[] | null>(null)
  const [binding, setBinding] = useState<ProxmoxBinding | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction>(null)
  const [bindingOpen, setBindingOpen] = useState(false)

  const reload = useCallback(() => {
    listNodes()
      .then(setRows)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
    getProxmoxBinding()
      .then(setBinding)
      .catch(() => { /* keep last binding on transient error */ })
  }, [])

  useEffect(() => { reload() }, [reload])

  // Slow refresh so cordon/drain state changes from another operator's
  // session show up here without a full page reload. Cheap (one
  // /api/nodes round trip + one /api/proxmox/binding).
  useEffect(() => {
    const id = setInterval(reload, 15_000)
    return () => clearInterval(id)
  }, [reload])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Nodes
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Cordon a node to stop new provisions landing on it. Drain to migrate
          every managed VM off (with operator-reviewable destination plan)
          before physically reclaiming the host.
        </p>
      </div>

      {binding && <BindingRow binding={binding} onClick={() => setBindingOpen(true)} />}

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
        <NodeCardGrid rows={rows} onAction={setPending} />
      )}

      {bindingOpen && binding && (
        <ProxmoxBindingModal binding={binding} onClose={() => setBindingOpen(false)} />
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

// BindingRow shows the "connected to <node>" indicator. Compact (one
// row of glass) so it doesn't compete with the node grid for attention,
// but always visible — the operator never has to wonder which cluster
// they're configuring.
function BindingRow({
  binding,
  onClick,
}: {
  binding: ProxmoxBinding
  onClick: () => void
}) {
  const reachable = binding.reachable !== false
  const primary = binding.connected_node || binding.cluster_name || hostFromURL(binding.host) || 'Proxmox'
  return (
    <button
      type="button"
      onClick={onClick}
      className="glass"
      style={{
        padding: '14px 18px',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: 16,
        cursor: 'pointer',
        textAlign: 'left',
        background: 'rgba(255,255,255,0.6)',
      }}
      title="Click for cluster details"
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <span
          aria-hidden="true"
          style={{
            width: 8, height: 8, borderRadius: 4,
            background: reachable ? 'var(--ok)' : 'var(--err)',
          }}
        />
        <div style={{ display: 'flex', flexDirection: 'column' }}>
          <span style={{ fontSize: 11, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em' }}>
            Connected to
          </span>
          <span style={{ fontSize: 14, fontWeight: 500, color: 'var(--ink)', marginTop: 2 }}>
            {primary}
            {binding.cluster_name && binding.connected_node !== binding.cluster_name && (
              <span style={{ color: 'var(--ink-mute)', fontWeight: 400, marginLeft: 8 }}>
                · cluster {binding.cluster_name}
              </span>
            )}
          </span>
        </div>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 18, fontSize: 12, color: 'var(--ink-body)' }}>
        <span style={{ fontFamily: 'Geist Mono, monospace' }}>{binding.host}</span>
        {binding.version && (
          <span style={{ fontFamily: 'Geist Mono, monospace', color: 'var(--ink-mute)' }}>
            pve {binding.version}
          </span>
        )}
        <span style={{ color: 'var(--ink-mute)' }}>
          {binding.node_count} {binding.node_count === 1 ? 'node' : 'nodes'} →
        </span>
      </div>
    </button>
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

// NodeCardGrid arranges per-node cards in a responsive grid — same
// breakpoints the Admin dashboard uses so the two surfaces feel
// consistent.
function NodeCardGrid({ rows, onAction }: { rows: NodeView[]; onAction: (a: PendingAction) => void }) {
  // Online first, then alpha. Self-host stays where it sorts naturally.
  const sorted = [...rows].sort((a, b) => {
    if (a.status !== b.status) {
      if (a.status === 'online') return -1
      if (b.status === 'online') return 1
    }
    return a.name.localeCompare(b.name)
  })
  return (
    <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-3">
      {sorted.map((n) => (
        <NodeCard key={n.name} node={n} onAction={onAction} />
      ))}
    </div>
  )
}

// NodeCard mirrors the Admin dashboard's node card layout (CPU / mem
// in use / mem allocated / swap usage bars) and adds the lock-state
// badge + an action menu in the corner. Reusing the visual language
// keeps the operator's mental model consistent across the two surfaces.
function NodeCard({ node: n, onAction }: { node: NodeView; onAction: (a: PendingAction) => void }) {
  const memUsedPct = n.mem_total > 0 ? (n.mem_used / n.mem_total) * 100 : 0
  const memAllocPct = n.mem_total > 0 ? (n.mem_allocated / n.mem_total) * 100 : 0
  const swapPct = n.swap_total > 0 ? (n.swap_used / n.swap_total) * 100 : 0
  const cpuPct = n.cpu * 100
  const swapping = n.swap_used > 10 * 1024 * 1024
  const hasSwap = n.swap_total > 0

  return (
    <Card className="p-6">
      <div className="flex items-start justify-between flex-wrap gap-3">
        <div>
          <div className="font-display text-lg font-medium flex items-center gap-2">
            {n.name}
            {n.is_self_host && (
              <span
                className="font-mono"
                title="Nimbus runs on this node — Remove is disabled to prevent locking yourself out"
                style={{
                  fontSize: 9, padding: '1px 5px', borderRadius: 3,
                  color: 'var(--ink-mute)', background: 'rgba(20,18,28,0.05)',
                  border: '1px solid var(--line)', textTransform: 'uppercase',
                  letterSpacing: '0.06em',
                }}
              >self</span>
            )}
          </div>
          <div className="font-mono text-[11px] text-ink-3 mt-1">
            {n.max_cpu} cores · {formatBytes(n.mem_total)} RAM
            {n.ip && <> · {n.ip}</>}
          </div>
          <div className="font-mono text-[11px] text-ink-3 mt-0.5">
            <span title={`${n.vm_count} running of ${n.vm_count_total} total`}>
              {n.vm_count}/{n.vm_count_total} VM{n.vm_count_total !== 1 ? 's' : ''}
            </span>
            {n.tags.length > 0 && (
              <span> · {n.tags.join(', ')}</span>
            )}
          </div>
        </div>
        <div className="flex flex-col items-end gap-1.5">
          <div className="flex items-center gap-2">
            <LockBadge state={n.lock_state} reason={n.lock_reason} />
            <StatusBadge status={n.status} />
            <RowActions node={n} onAction={onAction} />
          </div>
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
            label="Swap usage"
            pct={swapPct}
            hint={`${formatBytes(n.swap_used)} / ${formatBytes(n.swap_total)}`}
          />
        )}
      </div>
    </Card>
  )
}

function LockBadge({ state, reason }: { state: NodeView['lock_state']; reason?: string }) {
  if (state === 'none') return null
  const palette: Record<string, { color: string; bg: string; border: string; tip: string }> = {
    cordoned: {
      color: '#9a5c2e',
      bg: 'rgba(248,175,130,0.15)',
      border: 'rgba(248,175,130,0.4)',
      tip: 'Cordoned — scheduler skips this node, existing VMs keep running',
    },
    draining: {
      color: '#9a5c2e',
      bg: 'rgba(248,175,130,0.15)',
      border: 'rgba(248,175,130,0.4)',
      tip: 'Draining — migration in flight; do not touch',
    },
    drained: {
      color: 'var(--ink-mute)',
      bg: 'rgba(20,18,28,0.05)',
      border: 'var(--line)',
      tip: 'Drained — no managed VMs left, ready to remove from cluster',
    },
  }
  const p = palette[state]
  return (
    <span
      className="font-mono"
      title={reason ? `${p.tip}\n\nReason: ${reason}` : p.tip}
      style={{
        fontSize: 10, fontWeight: 600, padding: '2px 6px', borderRadius: 4,
        textTransform: 'uppercase', letterSpacing: '0.06em',
        color: p.color, background: p.bg, border: `1px solid ${p.border}`,
      }}
    >
      {state}
    </span>
  )
}

// RowActions is the per-card "..." menu. Each item carries a `title` so
// hovering reveals what the verb actually does — important for cordon
// and drain since most operators outside the kubectl world won't recognize
// the verbs at a glance.
function RowActions({ node, onAction }: { node: NodeView; onAction: (a: PendingAction) => void }) {
  const dismissAndDo = (fn: () => void) => () => {
    document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    fn()
  }
  return (
    <NavDropdown
      placement="bottom-end"
      triggerOn="click"
      triggerClassName="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink-2 hover:border-ink hover:text-ink transition-colors"
      panelClassName="rounded-lg border border-line bg-white py-1 min-w-[240px] shadow-lg"
      trigger={<MoreIcon />}
    >
      {node.lock_state === 'none' && (
        <ActionItem
          label="Cordon…"
          tip="Stop the scheduler from placing new VMs here. Existing VMs keep running. Reversible — Uncordon brings the node back."
          onClick={dismissAndDo(() => onAction({ kind: 'cordon', node }))}
        />
      )}
      {(node.lock_state === 'cordoned' || node.lock_state === 'drained') && (
        <ActionItem
          label="Uncordon"
          tip="Allow the scheduler to place new VMs here again."
          onClick={dismissAndDo(() => onAction({ kind: 'cordon', node }))}
        />
      )}
      {(node.lock_state === 'none' || node.lock_state === 'cordoned') && (
        <ActionItem
          label="Drain…"
          tip="Migrate every managed VM off this node, then mark it drained. Opens a plan modal — you review and (optionally) override per-VM destinations before confirming. Used when you're physically reclaiming the host."
          onClick={dismissAndDo(() => onAction({ kind: 'drain', node }))}
        />
      )}
      <ActionItem
        label="Tags…"
        tip="Free-text labels (e.g. gpu, nvme-fast). Stored for the future workload-aware scheduler; doesn't affect placement yet."
        onClick={dismissAndDo(() => onAction({ kind: 'tags', node }))}
      />
      <div className="my-1 border-t border-line" />
      <ActionItem
        label="Remove from cluster…"
        destructive
        disabled={node.is_self_host || node.lock_state !== 'drained'}
        tip={
          node.is_self_host
            ? 'Cannot remove the node Nimbus runs on.'
            : node.lock_state !== 'drained'
              ? 'Drain the node first — this calls pvecm delnode, which Proxmox refuses while VMs are still on it.'
              : 'Removes this node from the Proxmox cluster (pvecm delnode). The host itself is unaffected.'
        }
        onClick={dismissAndDo(() => onAction({ kind: 'remove', node }))}
      />
    </NavDropdown>
  )
}

function ActionItem({
  label,
  tip,
  onClick,
  destructive = false,
  disabled = false,
}: {
  label: string
  tip: string
  onClick: () => void
  destructive?: boolean
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      title={tip}
      disabled={disabled}
      onClick={onClick}
      className={`block w-full text-left px-3 py-1.5 text-[13px] cursor-pointer ${
        destructive
          ? 'text-bad hover:bg-[rgba(184,55,55,0.06)]'
          : 'text-ink hover:bg-[rgba(27,23,38,0.05)]'
      } disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent`}
    >
      {label}
    </button>
  )
}

function MoreIcon() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <circle cx={5} cy={12} r={1.75} />
      <circle cx={12} cy={12} r={1.75} />
      <circle cx={19} cy={12} r={1.75} />
    </svg>
  )
}
