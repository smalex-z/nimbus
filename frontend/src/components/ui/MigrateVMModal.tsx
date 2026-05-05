import { useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import { adminMigrateVM, listNodes, OnlineMigrationFailedError } from '@/api/client'
import { TIERS, type NodeView, type TierName } from '@/types'

// MigrateVMModal — admin-only flow for moving a VM between cluster nodes.
//
// Visually matches the drain plan's per-row dropdown + aggregate impact
// panel: each destination option in the dropdown shows the projected RAM%
// after the move, and a NodeImpactPanel below the picker renders the
// source losing this VM and the target gaining it side by side. Operator
// gets the same "is this destination going to be hot?" read they have in
// drain, but scoped to a single VM.
//
// Two-step UX matching the backend's online → offline confirmation
// design. The first POST tries a live migration when the VM is running;
// on rejection the server responds 409 with the upstream Proxmox reason
// and we surface a confirmation prompt asking the operator whether to
// stop+migrate+start. Stopped VMs go straight to offline mode.
//
// Projection math is client-side rather than reusing /drain-plan: that
// endpoint computes "if we drain ALL VMs on this node, where do they
// land?" — its projected RAM% reflects all migrations, not just this
// one's, so the numbers would mislead for a single-VM move. Computing
// `(target.mem_allocated + tier_bytes) / target.mem_total` directly
// here gives the honest single-VM projection.

type Props = {
  vm: {
    id: number
    hostname: string
    node: string
    tier: TierName
    status: 'running' | 'stopped' | 'paused' | 'unknown'
  }
  onClose: () => void
  onMigrated: (newNode: string) => void
}

type View = 'picker' | 'busy' | 'confirm-offline' | 'error'

interface Projection {
  node: string
  currentRamPct: number
  plannedRamPct: number
  currentVMCount: number
  plannedVMCount: number
  severity: 'ok' | 'caution' | 'high'
  // memTotal is carried so the dropdown rendering can use the same
  // projected RAM% the impact panel does — they're computed off the
  // same source.
  memTotal: number
  memAllocated: number
}

export default function MigrateVMModal({ vm, onClose, onMigrated }: Props) {
  const [view, setView] = useState<View>('picker')
  const [target, setTarget] = useState('')
  const [nodes, setNodes] = useState<NodeView[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [offlineReason, setOfflineReason] = useState('')

  // Disable Esc + backdrop dismiss while a migration is in flight — a
  // mid-flight cancel can't actually cancel the upstream Proxmox task,
  // so closing the modal would just hide a still-running operation.
  const busy = view === 'busy'

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [busy, onClose])

  useEffect(() => {
    listNodes()
      .then(setNodes)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed to load nodes'))
  }, [])

  // Eligible targets = online nodes that aren't the VM's current node.
  // Drained / cordoned nodes can still receive a migration — that's a
  // policy decision Proxmox doesn't enforce, and an admin manually
  // migrating a VM is opting into whatever placement they pick.
  const targets = useMemo(() => {
    if (!nodes) return []
    return nodes.filter((n) => n.name !== vm.node && n.status === 'online')
  }, [nodes, vm.node])

  const tierBytes = TIERS[vm.tier].memMB * 1024 * 1024

  // sourceProjection + projectionByNode drive both the dropdown labels
  // (each candidate's projected RAM%) and the impact panel (source +
  // target rows side by side). Computed once per nodes-load so a poll
  // refresh re-runs the math without re-rendering on every keystroke.
  const sourceProjection: Projection | null = useMemo(() => {
    if (!nodes) return null
    const src = nodes.find((n) => n.name === vm.node)
    if (!src) return null
    const currentRamPct = pct(src.mem_allocated, src.mem_total)
    // After this VM leaves the source, its committed mem drops by tier_bytes.
    const plannedAlloc = Math.max(0, src.mem_allocated - tierBytes)
    const plannedRamPct = pct(plannedAlloc, src.mem_total)
    return {
      node: src.name,
      currentRamPct,
      plannedRamPct,
      // We don't have a live VM count per node here, but cluster_vms is
      // the source of truth and the impact panel just needs deltas. Show
      // "—1" relative to current, leaving the absolute count blank.
      currentVMCount: 0,
      plannedVMCount: -1,
      severity: severityOf(plannedRamPct),
      memTotal: src.mem_total,
      memAllocated: src.mem_allocated,
    }
  }, [nodes, vm.node, tierBytes])

  const projectionByNode = useMemo(() => {
    const out: Record<string, Projection> = {}
    if (!nodes) return out
    for (const n of nodes) {
      if (n.name === vm.node) continue
      const currentRamPct = pct(n.mem_allocated, n.mem_total)
      const plannedAlloc = n.mem_allocated + tierBytes
      const plannedRamPct = pct(plannedAlloc, n.mem_total)
      out[n.name] = {
        node: n.name,
        currentRamPct,
        plannedRamPct,
        currentVMCount: 0,
        plannedVMCount: 1,
        severity: severityOf(plannedRamPct),
        memTotal: n.mem_total,
        memAllocated: n.mem_allocated,
      }
    }
    return out
  }, [nodes, vm.node, tierBytes])

  const targetProjection = target ? projectionByNode[target] : null

  const submit = async (allowOffline: boolean) => {
    if (!target) return
    setView('busy')
    setError(null)
    try {
      const res = await adminMigrateVM(vm.id, target, allowOffline)
      onMigrated(res.target_node)
    } catch (err) {
      if (err instanceof OnlineMigrationFailedError) {
        setOfflineReason(err.reason)
        setView('confirm-offline')
        return
      }
      setError(err instanceof Error ? err.message : 'migration failed')
      setView('error')
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={() => !busy && onClose()}
      role="dialog"
      aria-modal="true"
      aria-label={`Migrate ${vm.hostname}`}
    >
      <Card
        strong
        className="w-full max-w-[600px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Migrate machine</div>
        <h3 className="text-2xl mt-1 mb-4">Move {vm.hostname}</h3>

        {view === 'confirm-offline' ? (
          <OfflineConfirmBody
            reason={offlineReason}
            target={target}
            onCancel={onClose}
            onContinue={() => submit(true)}
          />
        ) : (
          <PickerBody
            sourceNode={vm.node}
            tier={vm.tier}
            target={target}
            onTargetChange={setTarget}
            targets={targets}
            projectionByNode={projectionByNode}
            sourceProjection={sourceProjection}
            targetProjection={targetProjection}
            nodesLoaded={nodes !== null}
            view={view}
            error={error}
            onCancel={onClose}
            onMigrate={() => submit(false)}
          />
        )}
      </Card>
    </div>,
    document.body,
  )
}

function PickerBody({
  sourceNode,
  tier,
  target,
  onTargetChange,
  targets,
  projectionByNode,
  sourceProjection,
  targetProjection,
  nodesLoaded,
  view,
  error,
  onCancel,
  onMigrate,
}: {
  sourceNode: string
  tier: TierName
  target: string
  onTargetChange: (v: string) => void
  targets: NodeView[]
  projectionByNode: Record<string, Projection>
  sourceProjection: Projection | null
  targetProjection: Projection | null
  nodesLoaded: boolean
  view: View
  error: string | null
  onCancel: () => void
  onMigrate: () => void
}) {
  const busy = view === 'busy'
  const tierMem = `${(TIERS[tier].memMB / 1024).toFixed(0)} GiB`
  return (
    <>
      <p className="text-sm text-ink-2 leading-relaxed mb-5">
        Live migration runs without downtime. If Proxmox refuses live
        migration (snapshots, local CD/DVD, etc.) we'll surface the reason
        and you can choose to stop, migrate, then start the VM on the new
        node.
      </p>

      <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 font-mono text-[11px] mb-5 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2">
        <span className="text-ink-3 uppercase tracking-wider">Source</span>
        <span className="text-ink">{sourceNode}</span>
        <span className="text-ink-3 uppercase tracking-wider">Tier</span>
        <span className="text-ink">{tier} · {tierMem}</span>
      </div>

      <label className="block text-xs uppercase tracking-wider text-ink-3 mb-2">
        Target node
      </label>
      <select
        value={target}
        onChange={(e) => onTargetChange(e.target.value)}
        disabled={busy || !nodesLoaded || targets.length === 0}
        className="w-full mb-1 px-3 py-2.5 rounded-[10px] border border-line-2 bg-white/85 text-sm text-ink focus:outline-none focus:border-ink disabled:opacity-50"
      >
        <option value="">
          {!nodesLoaded
            ? 'Loading…'
            : targets.length === 0
              ? 'No eligible target nodes'
              : 'Choose target node…'}
        </option>
        {targets.map((n) => {
          const p = projectionByNode[n.name]
          const ramAfter = p ? `${p.plannedRamPct.toFixed(0)}% RAM` : ''
          return (
            <option key={n.name} value={n.name}>
              {n.name}
              {ramAfter ? ` · ${ramAfter} after` : ''}
            </option>
          )
        })}
      </select>
      {nodesLoaded && targets.length === 0 && (
        <p className="text-[12px] text-ink-3 mb-5 mt-1.5">
          The VM is on the only online node. Bring another node online
          before retrying.
        </p>
      )}

      {target && sourceProjection && targetProjection && (
        <NodeImpactPanel source={sourceProjection} target={targetProjection} />
      )}

      {busy && (
        <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2 text-sm text-ink-2">
          Migrating to {target}… this can take several minutes for a busy
          VM. Don't close this tab.
        </div>
      )}

      {error && view === 'error' && (
        <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
          {error}
        </div>
      )}

      <div className="flex justify-end gap-3 mt-7">
        <Button variant="ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button onClick={onMigrate} disabled={busy || !target}>
          {busy ? 'Migrating…' : 'Migrate'}
        </Button>
      </div>
    </>
  )
}

// NodeImpactPanel renders a two-row "source loses, target gains"
// summary styled like the drain plan's AggregatePanel — same visual
// vocabulary so operators reading the migrate modal recognize the
// pattern from drain. Severity coloring (high/caution/ok) matches the
// backend's >85 / >60 thresholds.
function NodeImpactPanel({
  source,
  target,
}: {
  source: Projection
  target: Projection
}) {
  return (
    <div className="mt-5 mb-3 p-3.5 rounded-[10px] bg-[rgba(20,18,28,0.03)] border border-line-2">
      <div className="text-[11px] uppercase tracking-wider text-ink-3 mb-2 font-mono">
        Node impact
      </div>
      <div className="flex flex-col gap-1.5">
        <ImpactRow row={source} delta="loses" />
        <ImpactRow row={target} delta="gains" />
      </div>
    </div>
  )
}

function ImpactRow({ row, delta }: { row: Projection; delta: 'gains' | 'loses' }) {
  const color =
    row.severity === 'high'
      ? 'var(--bad)'
      : row.severity === 'caution'
        ? 'var(--warn)'
        : 'var(--ink-body)'
  const arrow = delta === 'gains' ? '+' : '−'
  return (
    <div
      style={{ color }}
      className="flex justify-between items-baseline gap-3 text-[12px] font-mono"
    >
      <span>
        <strong className="text-ink">{row.node}</strong>{' '}
        <span className="text-ink-3">({arrow}1 VM)</span>
      </span>
      <span>
        RAM {row.plannedRamPct.toFixed(0)}%{' '}
        <span className="text-ink-3">(was {row.currentRamPct.toFixed(0)}%)</span>
      </span>
    </div>
  )
}

function OfflineConfirmBody({
  reason,
  target,
  onCancel,
  onContinue,
}: {
  reason: string
  target: string
  onCancel: () => void
  onContinue: () => void
}) {
  return (
    <>
      <div className="mb-5 p-3.5 rounded-[10px] bg-[rgba(184,101,15,0.06)] border border-[rgba(184,101,15,0.25)]">
        <div className="text-warn text-xs uppercase tracking-wider mb-1.5 font-mono">
          Live migration unavailable
        </div>
        <p className="text-sm text-ink-2 leading-relaxed">
          Proxmox refused the live migration with the message below.
          Continuing will <strong className="text-ink">stop</strong> the VM,
          migrate it to <strong className="text-ink">{target}</strong>,
          then start it again. The VM will be briefly unreachable during
          the move.
        </p>
      </div>
      <pre className="mb-7 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2 text-[11px] font-mono text-ink whitespace-pre-wrap break-words">
        {reason || '(no reason supplied)'}
      </pre>

      <div className="flex justify-end gap-3">
        <Button variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button onClick={onContinue}>Continue offline</Button>
      </div>
    </>
  )
}

function pct(used: number, total: number): number {
  if (total <= 0) return 0
  return (used / total) * 100
}

function severityOf(ramPct: number): 'ok' | 'caution' | 'high' {
  if (ramPct > 85) return 'high'
  if (ramPct > 60) return 'caution'
  return 'ok'
}
