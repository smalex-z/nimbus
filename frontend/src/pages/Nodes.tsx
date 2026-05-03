import { useCallback, useEffect, useMemo, useState } from 'react'
import { listNodes } from '@/api/client'
import type { NodeView } from '@/types'
import { CordonModal, DrainPlanModal, RemoveNodeModal, TagsModal } from '@/components/NodeActionModals'
import NavDropdown from '@/components/ui/NavDropdown'

// Nodes — admin-only page listing every Proxmox node Nimbus knows about,
// with per-row actions for cordon / drain / uncordon / tag / remove.
//
// Lock state semantics (mirrors Kubernetes vocabulary by design — most
// operators recognize cordon/drain from kubectl):
//   none      — normal: scheduler can land VMs here
//   cordoned  — soft lock: scheduler skips, existing VMs untouched
//   draining  — hard lock: scheduler skips + a drain operation is in flight
//   drained   — terminal: zero managed VMs left, ready to remove from cluster
//
// The drain action opens a modal showing the migration plan — operator
// reviews + (optionally) overrides per-VM destinations before confirming.
// Uses NDJSON streaming progress for the actual execution. See
// DrainPlanModal in components/NodeActionModals.tsx.
export default function Nodes() {
  const [rows, setRows] = useState<NodeView[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction>(null)

  const reload = useCallback(() => {
    listNodes()
      .then(setRows)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
  }, [])

  useEffect(() => { reload() }, [reload])

  // Re-fetch on a slow loop so cordon/drain state changes from another
  // operator's session show up here without a page reload. Cheap (one
  // /api/nodes round trip; Proxmox calls are already cached server-side).
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
        <NodesTable rows={rows} onAction={setPending} />
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

function NodesTable({ rows, onAction }: { rows: NodeView[]; onAction: (a: PendingAction) => void }) {
  const sorted = useMemo(() => {
    // Online nodes first, then alphabetical. Self-host stays where it
    // sorts naturally — operators want to see it in the same place
    // regardless of whether it's selected.
    return [...rows].sort((a, b) => {
      if (a.status !== b.status) {
        if (a.status === 'online') return -1
        if (b.status === 'online') return 1
      }
      return a.name.localeCompare(b.name)
    })
  }, [rows])

  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Cluster nodes
        </span>
        <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
          {rows.length} {rows.length === 1 ? 'node' : 'nodes'}
        </span>
      </div>
      <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
        <table className="w-full text-left" style={{ fontSize: 13, borderCollapse: 'collapse' }}>
          <thead>
            <tr style={{ color: 'var(--ink-mute)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>Node</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>Status</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>Lock</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>CPU</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>RAM</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>VMs</th>
              <th style={{ padding: '8px 8px', fontWeight: 500 }}>Tags</th>
              <th style={{ padding: '8px 8px', fontWeight: 500, width: 1 }} aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {sorted.map((n) => (
              <tr key={n.name} style={{ borderTop: '1px solid var(--line)' }}>
                <td style={{ padding: '10px 8px' }}>
                  <div style={{ color: 'var(--ink)', fontWeight: 500, display: 'flex', alignItems: 'center', gap: 6 }}>
                    {n.name}
                    {n.is_self_host && (
                      <span
                        className="font-mono"
                        title="The node Nimbus itself runs on — Remove is disabled"
                        style={{
                          fontSize: 9, padding: '1px 5px', borderRadius: 3,
                          color: 'var(--ink-mute)', background: 'rgba(20,18,28,0.05)',
                          border: '1px solid var(--line)', textTransform: 'uppercase',
                          letterSpacing: '0.06em',
                        }}
                      >self</span>
                    )}
                  </div>
                  {n.ip && (
                    <div style={{ fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace', marginTop: 2 }}>
                      {n.ip}
                    </div>
                  )}
                </td>
                <td style={{ padding: '10px 8px' }}><StatusPill status={n.status} /></td>
                <td style={{ padding: '10px 8px' }}>
                  <LockPill state={n.lock_state} reason={n.lock_reason} />
                </td>
                <td style={{ padding: '10px 8px', color: 'var(--ink-body)', whiteSpace: 'nowrap' }}>
                  {(n.cpu * 100).toFixed(0)}% / {n.max_cpu}c
                </td>
                <td style={{ padding: '10px 8px', color: 'var(--ink-body)', whiteSpace: 'nowrap' }}>
                  {n.mem_total > 0 ? `${((n.mem_used / n.mem_total) * 100).toFixed(0)}%` : '—'}
                </td>
                <td style={{ padding: '10px 8px', color: 'var(--ink-body)', whiteSpace: 'nowrap' }}>
                  {n.vm_count} / {n.vm_count_total}
                </td>
                <td style={{ padding: '10px 8px' }}><TagChips tags={n.tags} /></td>
                <td style={{ padding: '10px 8px', textAlign: 'right' }}>
                  <RowActions node={n} onAction={onAction} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function StatusPill({ status }: { status: NodeView['status'] }) {
  if (status === 'online') {
    return (
      <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
        <span className="n-pill-dot" />
        online
      </span>
    )
  }
  return (
    <span
      className="n-pill"
      style={{
        fontSize: 10,
        color: 'var(--err)',
        background: 'rgba(184,55,55,0.08)',
        border: '1px solid rgba(184,55,55,0.25)',
      }}
    >
      {status}
    </span>
  )
}

function LockPill({ state, reason }: { state: NodeView['lock_state']; reason?: string }) {
  if (state === 'none') {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  const palette: Record<string, { color: string; bg: string; border: string }> = {
    cordoned: { color: '#9a5c2e', bg: 'rgba(248,175,130,0.15)', border: 'rgba(248,175,130,0.4)' },
    draining: { color: '#9a5c2e', bg: 'rgba(248,175,130,0.15)', border: 'rgba(248,175,130,0.4)' },
    drained:  { color: 'var(--ink-mute)', bg: 'rgba(20,18,28,0.05)', border: 'var(--line)' },
  }
  const p = palette[state]
  return (
    <span
      className="font-mono"
      title={reason || ''}
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

function TagChips({ tags }: { tags: string[] }) {
  if (!tags || tags.length === 0) {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  return (
    <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}>
      {tags.map((t) => (
        <span
          key={t}
          style={{
            fontSize: 10, fontFamily: 'Geist Mono, monospace',
            padding: '2px 6px', borderRadius: 4,
            background: 'rgba(20,18,28,0.05)', border: '1px solid var(--line)',
            color: 'var(--ink-body)',
          }}
        >
          {t}
        </span>
      ))}
    </span>
  )
}

function RowActions({ node, onAction }: { node: NodeView; onAction: (a: PendingAction) => void }) {
  // Mirror the dismiss-then-do pattern UsersTable uses so the dropdown
  // closes cleanly before the modal mounts (otherwise the floating
  // panel sits over the modal backdrop).
  const dismissAndDo = (fn: () => void) => () => {
    document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    fn()
  }
  return (
    <NavDropdown
      placement="bottom-end"
      triggerOn="click"
      triggerClassName="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink-2 hover:border-ink hover:text-ink transition-colors"
      panelClassName="rounded-lg border border-line bg-white py-1 min-w-[200px] shadow-lg"
      trigger={<MoreIcon />}
    >
      {node.lock_state === 'none' && (
        <button
          type="button"
          onClick={dismissAndDo(() => onAction({ kind: 'cordon', node }))}
          className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
        >
          Cordon…
        </button>
      )}
      {(node.lock_state === 'cordoned' || node.lock_state === 'drained') && (
        <button
          type="button"
          onClick={dismissAndDo(() => onAction({ kind: 'cordon', node }))}
          className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
        >
          Uncordon
        </button>
      )}
      {(node.lock_state === 'none' || node.lock_state === 'cordoned') && (
        <button
          type="button"
          onClick={dismissAndDo(() => onAction({ kind: 'drain', node }))}
          className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
        >
          Drain…
        </button>
      )}
      <button
        type="button"
        onClick={dismissAndDo(() => onAction({ kind: 'tags', node }))}
        className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
      >
        Tags…
      </button>
      <div className="my-1 border-t border-line" />
      <button
        type="button"
        disabled={node.is_self_host || node.lock_state !== 'drained'}
        onClick={dismissAndDo(() => onAction({ kind: 'remove', node }))}
        title={
          node.is_self_host
            ? 'Cannot remove the node Nimbus runs on'
            : node.lock_state !== 'drained'
              ? 'Drain the node first'
              : ''
        }
        className="block w-full text-left px-3 py-1.5 text-[13px] text-bad hover:bg-[rgba(184,55,55,0.06)] cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent"
      >
        Remove from cluster…
      </button>
    </NavDropdown>
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
