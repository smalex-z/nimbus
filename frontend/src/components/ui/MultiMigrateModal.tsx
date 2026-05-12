import { useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import NodeImpactPanel, {
  type NodeProjectionRow,
} from '@/components/ui/NodeImpactPanel'
import { pct, severityOf } from '@/lib/nodeImpact'
import { adminMigrateVM, listNodes } from '@/api/client'
import { TIERS, type NodeView, type TierName } from '@/types'

// MultiMigrateModal — bulk-select counterpart to MigrateVMModal.
// Mirrors that modal's layout (Card + eyebrow/h3, target select,
// NodeImpactPanel, sticky footer buttons) so operators recognise the
// pattern. Differences from the single-VM modal:
//
//   - No per-VM scoring preview. Bulk selections cross tiers and
//     sources, so showing each VM's optimal landing isn't useful;
//     the operator picks one target manually and the panel shows the
//     aggregate effect (sources lose, target gains).
//
//   - Async-only dispatch. Each selected VM fires its own POST →
//     operation row → Tasks dropdown entry. The modal closes
//     immediately after dispatch — per-VM progress lives in the
//     Tasks panel.
//
//   - allow_offline=true on every dispatch. Bulk migrate is the
//     "I don't care if these get briefly stopped" path; per-VM
//     confirm-offline prompts in series would be terrible UX.

type Props = {
  selectedVMs: Array<{
    id: number
    hostname: string
    node: string
    tier: TierName
  }>
  onClose: () => void
  // onDispatched fires after every selected VM has had its migrate
  // request sent. Caller typically clears the selection set.
  onDispatched: () => void
}

type View = 'picker' | 'busy' | 'error'

export default function MultiMigrateModal({ selectedVMs, onClose, onDispatched }: Props) {
  const [target, setTarget] = useState('')
  const [nodes, setNodes] = useState<NodeView[] | null>(null)
  const [loadErr, setLoadErr] = useState<string | null>(null)
  const [view, setView] = useState<View>('picker')
  const [error, setError] = useState<string | null>(null)
  const [progress, setProgress] = useState({ done: 0, total: selectedVMs.length })

  const busy = view === 'busy'

  useEffect(() => {
    listNodes()
      .then(setNodes)
      .catch((e) => setLoadErr(e instanceof Error ? e.message : 'failed to load nodes'))
  }, [])

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

  // Aggregate the selection by source node — `alpha (3), beta (1)`
  // style — so the operator sees where the moves are coming from.
  // Self-moves (source == target) are excluded from the impact
  // calculation since they're no-ops Proxmox will reject.
  const sourceCounts = useMemo(() => {
    const counts = new Map<string, number>()
    selectedVMs.forEach((vm) => {
      counts.set(vm.node, (counts.get(vm.node) ?? 0) + 1)
    })
    return counts
  }, [selectedVMs])

  // Per-source aggregate RAM that would leave each node if every
  // selected VM there moved to the picked target. Computed on the
  // fly from listNodes (allocated bytes) + each VM's tier mem.
  // Skips when target is unset OR when target == source (no real
  // movement happens for that subset).
  const impactRows = useMemo<NodeProjectionRow[]>(() => {
    if (!target || !nodes) return []
    const byName = new Map(nodes.map((n) => [n.name, n]))

    // Sum the bytes leaving each source (excluding VMs already on target).
    const bytesLeaving = new Map<string, number>()
    let bytesArriving = 0
    let vmsArriving = 0
    selectedVMs.forEach((vm) => {
      if (vm.node === target) return
      const tierBytes = TIERS[vm.tier].memMB * 1024 * 1024
      bytesLeaving.set(vm.node, (bytesLeaving.get(vm.node) ?? 0) + tierBytes)
      bytesArriving += tierBytes
      vmsArriving += 1
    })

    const rows: NodeProjectionRow[] = []
    // Source rows — one per node losing ≥1 VM.
    for (const [sourceNode, bytes] of bytesLeaving) {
      const node = byName.get(sourceNode)
      if (!node) continue
      const currentRamPct = pct(node.mem_allocated, node.mem_total)
      const planned = Math.max(0, node.mem_allocated - bytes)
      const plannedRamPct = pct(planned, node.mem_total)
      rows.push({
        node: sourceNode,
        currentRamPct,
        plannedRamPct,
        severity: severityOf(plannedRamPct),
        vmDelta: -(sourceCounts.get(sourceNode) ?? 0),
      })
    }
    // Target row — only render when at least one VM actually lands.
    if (vmsArriving > 0) {
      const tgt = byName.get(target)
      if (tgt) {
        const currentRamPct = pct(tgt.mem_allocated, tgt.mem_total)
        const planned = tgt.mem_allocated + bytesArriving
        const plannedRamPct = pct(planned, tgt.mem_total)
        rows.push({
          node: target,
          currentRamPct,
          plannedRamPct,
          severity: severityOf(plannedRamPct),
          vmDelta: vmsArriving,
        })
      }
    }
    return rows
  }, [target, nodes, selectedVMs, sourceCounts])

  const candidates = (nodes ?? []).filter((n) => n.status === 'online')

  const submit = async () => {
    if (!target) return
    setView('busy')
    setError(null)
    setProgress({ done: 0, total: selectedVMs.length })

    // Fire all migrates in parallel; per-VM progress shows up in the
    // Tasks dropdown as each operation row lands.
    const results = await Promise.allSettled(
      selectedVMs.map(async (vm) => {
        try {
          await adminMigrateVM(vm.id, target, true)
          setProgress((p) => ({ ...p, done: p.done + 1 }))
        } catch (err) {
          setProgress((p) => ({ ...p, done: p.done + 1 }))
          throw err
        }
      }),
    )

    const failed = results.filter((r) => r.status === 'rejected').length
    if (failed > 0) {
      setError(
        `${failed} of ${selectedVMs.length} dispatch(es) failed. The Tasks panel shows per-VM status.`,
      )
      setView('error')
      return
    }
    onDispatched()
    onClose()
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[60] grid place-items-center p-4 bg-[rgba(20,18,28,0.45)]"
      style={{ backdropFilter: 'blur(8px)' }}
      onClick={() => !busy && onClose()}
      role="dialog"
      aria-modal="true"
      aria-label={`Migrate ${selectedVMs.length} machines`}
    >
      <Card
        strong
        className="w-full max-w-[600px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Migrate machines</div>
        <h3 className="text-2xl mt-1 mb-2">
          Move {selectedVMs.length} machine{selectedVMs.length === 1 ? '' : 's'}
        </h3>
        <p className="text-sm text-ink-2 mb-5 leading-relaxed">
          Each VM dispatches as its own background task — watch progress
          in the Tasks panel. Live migration is attempted first; on
          rejection (snapshots, local devices, etc.) the bulk path
          automatically falls back to stop → migrate → start (no
          per-VM confirmation prompt). Legacy VMs with a per-node
          cloud-init ISO attached have it detached automatically during
          the offline phase.
        </p>

        <div className="flex justify-between items-baseline mb-1.5">
          <label htmlFor="multi-target" className="eyebrow text-[10.5px]">
            Target node
          </label>
          <span className="text-[11px] text-ink-3 font-mono">
            from{' '}
            {Array.from(sourceCounts.entries())
              .map(([n, c]) => `${n} (${c})`)
              .join(', ')}
          </span>
        </div>
        <select
          id="multi-target"
          className="w-full font-sans text-sm text-ink rounded-[10px] bg-white/85 border border-line-2 px-3.5 py-2 focus:outline-none focus:ring-2 focus:ring-accent/30 disabled:opacity-50"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          disabled={busy || candidates.length === 0}
        >
          <option value="">Pick a node…</option>
          {candidates.map((n) => {
            const ramPct = pct(n.mem_allocated, n.mem_total)
            return (
              <option key={n.name} value={n.name}>
                {n.name} · {ramPct.toFixed(0)}% RAM allocated
              </option>
            )
          })}
        </select>

        {loadErr && (
          <p className="text-[12px] text-bad mt-1.5">{loadErr}</p>
        )}

        {/* Same NodeImpactPanel the single-VM modal + drain use.
            Renders after a target is picked and shows per-source-node
            losses + the target's gain. Severity colouring matches
            the rest of the app. */}
        <NodeImpactPanel rows={impactRows} />

        {busy && (
          <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2 text-sm text-ink-2">
            Dispatching {progress.done}/{progress.total}… You can safely
            close this modal — each migration runs in the background and
            reports to the Tasks panel.
          </div>
        )}

        {error && view === 'error' && (
          <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
            {error}
          </div>
        )}

        <div className="flex justify-end gap-3 mt-7">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy || !target}>
            {busy
              ? 'Dispatching…'
              : `Migrate ${selectedVMs.length} VM${selectedVMs.length === 1 ? '' : 's'}`}
          </Button>
        </div>
      </Card>
    </div>,
    document.body,
  )
}
