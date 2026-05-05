import { useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import {
  adminMigrateVM,
  getMigratePlan,
  listNodes,
  OnlineMigrationFailedError,
} from '@/api/client'
import type { MigratePlan, MigratePlanEligibleTarget } from '@/api/client'
import { TIERS, type NodeView, type TierName } from '@/types'

// MigrateVMModal — admin-only flow for moving a VM between cluster nodes.
//
// Visually matches the drain plan's per-row dropdown + aggregate impact
// panel. The destination dropdown is populated from the backend
// migrate-plan endpoint: each option carries the same nodescore the
// provision flow uses, plus the projected RAM% after this single VM
// lands there. The auto_pick is marked "(recommended)" so the operator
// can take Nimbus's preferred placement at a glance.
//
// Why a backend plan rather than client-side scoring: the score
// algorithm bakes in templates-present, lock state, disk gates, and
// soft CPU projection — replicating that in TypeScript would either
// drift or duplicate. The endpoint is fast (one Proxmox cluster walk +
// DB read), so fetching on modal open is cheap.
//
// Two-step UX matching the backend's online → offline confirmation:
// the first POST tries a live migration when the VM is running; on
// rejection the server responds 409 with the upstream Proxmox reason
// and the modal swaps to a confirm-offline view. Stopped VMs go
// straight to offline mode.

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
  severity: 'ok' | 'caution' | 'high'
}

export default function MigrateVMModal({ vm, onClose, onMigrated }: Props) {
  const [view, setView] = useState<View>('picker')
  const [target, setTarget] = useState('')
  const [plan, setPlan] = useState<MigratePlan | null>(null)
  const [nodes, setNodes] = useState<NodeView[] | null>(null)
  const [loadErr, setLoadErr] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [offlineReason, setOfflineReason] = useState('')

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

  // Fetch plan + node telemetry in parallel. The plan drives the
  // dropdown (per-target score + projected RAM%); listNodes gives us
  // the source node's current RAM% so the impact panel can render
  // "source loses this VM" alongside "target gains it" symmetrically.
  useEffect(() => {
    let cancelled = false
    Promise.all([getMigratePlan(vm.id), listNodes()])
      .then(([p, n]) => {
        if (cancelled) return
        setPlan(p)
        setNodes(n)
        if (p.auto_pick) setTarget(p.auto_pick)
      })
      .catch((e: unknown) => {
        if (!cancelled) setLoadErr(e instanceof Error ? e.message : 'failed to load plan')
      })
    return () => { cancelled = true }
  }, [vm.id])

  const tierBytes = TIERS[vm.tier].memMB * 1024 * 1024

  // Source projection: current vs. "after this VM leaves." The plan
  // doesn't include the source (it's not a migration target), so we
  // pull it from listNodes and subtract the tier bytes locally.
  const sourceProjection = useMemo<Projection | null>(() => {
    if (!nodes) return null
    const src = nodes.find((n) => n.name === vm.node)
    if (!src) return null
    const currentRamPct = pct(src.mem_allocated, src.mem_total)
    const plannedAlloc = Math.max(0, src.mem_allocated - tierBytes)
    const plannedRamPct = pct(plannedAlloc, src.mem_total)
    return {
      node: src.name,
      currentRamPct,
      plannedRamPct,
      severity: severityOf(plannedRamPct),
    }
  }, [nodes, vm.node, tierBytes])

  // Target projection: lifted from the plan's eligible entry for the
  // selected target. The plan computes projected_ram_pct on the same
  // committedMem basis the source projection uses (so the two stay
  // comparable), with no plannedAdd accumulation — accurate for a
  // single-VM move.
  const targetProjection = useMemo<Projection | null>(() => {
    if (!plan || !nodes || !target) return null
    const opt = plan.eligible.find((e) => e.node === target)
    if (!opt) return null
    const node = nodes.find((n) => n.name === target)
    if (!node) return null
    return {
      node: target,
      currentRamPct: pct(node.mem_allocated, node.mem_total),
      plannedRamPct: opt.projected_ram_pct,
      severity: severityOf(opt.projected_ram_pct),
    }
  }, [plan, nodes, target])

  const targetOpt: MigratePlanEligibleTarget | null = useMemo(() => {
    if (!plan || !target) return null
    return plan.eligible.find((e) => e.node === target) ?? null
  }, [plan, target])

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
            plan={plan}
            loadErr={loadErr}
            sourceProjection={sourceProjection}
            targetProjection={targetProjection}
            targetOpt={targetOpt}
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
  plan,
  loadErr,
  sourceProjection,
  targetProjection,
  targetOpt,
  view,
  error,
  onCancel,
  onMigrate,
}: {
  sourceNode: string
  tier: TierName
  target: string
  onTargetChange: (v: string) => void
  plan: MigratePlan | null
  loadErr: string | null
  sourceProjection: Projection | null
  targetProjection: Projection | null
  targetOpt: MigratePlanEligibleTarget | null
  view: View
  error: string | null
  onCancel: () => void
  onMigrate: () => void
}) {
  const busy = view === 'busy'
  const tierMem = `${(TIERS[tier].memMB / 1024).toFixed(0)} GiB`
  const planLoaded = plan !== null
  const eligible = plan?.eligible ?? []
  const usable = eligible.filter((e) => !e.disabled)
  // Disable the Migrate button when the operator picked a disabled
  // option (rare — auto_pick steers them toward usable rows — but
  // possible if every node is ineligible and they pick anyway).
  const targetUsable = targetOpt !== null && !targetOpt.disabled

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
        disabled={busy || !planLoaded || usable.length === 0}
        className="w-full mb-1 px-3 py-2.5 rounded-[10px] border border-line-2 bg-white/85 text-sm text-ink focus:outline-none focus:border-ink disabled:opacity-50"
      >
        <option value="">
          {!planLoaded
            ? 'Loading…'
            : usable.length === 0
              ? 'No eligible target nodes'
              : 'Choose target node…'}
        </option>
        {eligible.map((e) => {
          const isWinner = plan?.auto_pick === e.node
          const ramTag = `${e.projected_ram_pct.toFixed(0)}% RAM`
          const recommended = isWinner ? ' (recommended)' : ''
          const ineligible = e.disabled ? ' — ineligible' : ''
          return (
            <option
              key={e.node}
              value={e.node}
              disabled={e.disabled}
              title={e.disabled ? e.disabled_reason : undefined}
            >
              {e.node}
              {recommended}
              {e.disabled ? ineligible : ` · ${ramTag} after · score ${e.score.toFixed(2)}`}
            </option>
          )
        })}
      </select>
      {planLoaded && usable.length === 0 && (
        <p className="text-[12px] text-ink-3 mb-5 mt-1.5">
          Every other node is ineligible — bring one online or remove a
          cordon before retrying.
        </p>
      )}
      {loadErr && (
        <p className="text-[12px] text-bad mb-5 mt-1.5">{loadErr}</p>
      )}

      {target && targetOpt?.disabled && targetOpt.disabled_reason && (
        <div className="mt-4 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-[12px]">
          {target} is ineligible: {targetOpt.disabled_reason}
        </div>
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
        <Button onClick={onMigrate} disabled={busy || !target || !targetUsable}>
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
