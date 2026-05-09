import { useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import Button from '@/components/ui/Button'
import Card from '@/components/ui/Card'
import { adminMigrateVM, listNodes } from '@/api/client'
import type { NodeView } from '@/types'

// MultiMigrateModal — bulk-select counterpart to MigrateVMModal.
//
// UX deltas vs the single-VM modal:
//
//   - No per-VM scoring. Bulk selections span tiers, sources, current
//     nodes — there's no shared placement preview that's useful. The
//     operator picks one target manually; capacity gating happens
//     server-side (Proxmox refuses if RAM/disk insufficient) and per-
//     VM failures show up as failed operations in the Tasks dropdown.
//
//   - Async-only. Each selected VM fires its own POST → operation row
//     → Tasks dropdown entry. The modal closes immediately after
//     dispatch. There's no "wait for all to complete" view — that's
//     what the dropdown is for, and blocking on N VMs would defeat the
//     close-tab-and-come-back framework.
//
//   - allow_offline=true for every dispatch. Bulk migrate is an admin
//     "I don't care if these get stopped" operation; surfacing N
//     confirm-offline prompts in series would be terrible UX. Single-
//     VM migrations keep their two-step UX for the careful case.

type Props = {
  selectedVMs: Array<{ id: number; hostname: string; node: string }>
  onClose: () => void
  // onDispatched fires after every selected VM has had its migrate
  // request sent. Caller typically clears the selection set.
  onDispatched: () => void
}

export default function MultiMigrateModal({ selectedVMs, onClose, onDispatched }: Props) {
  const [target, setTarget] = useState('')
  const [nodes, setNodes] = useState<NodeView[]>([])
  const [loadErr, setLoadErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [progress, setProgress] = useState({ done: 0, total: selectedVMs.length })

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

  // Source nodes the selection currently sits on — surfaced as info
  // text so the operator sees where the moves are coming from.
  const sourceSummary = useMemo(() => {
    const counts: Record<string, number> = {}
    selectedVMs.forEach((vm) => {
      counts[vm.node] = (counts[vm.node] ?? 0) + 1
    })
    return Object.entries(counts)
      .map(([node, n]) => `${node} (${n})`)
      .join(', ')
  }, [selectedVMs])

  // Target candidates: every online node. Don't filter out source
  // nodes — a partial overlap is fine (some VMs already there will
  // 400 server-side as same-node, which surfaces as a failed op the
  // operator can ignore).
  const candidates = nodes.filter((n) => n.status === 'online')

  const submit = async () => {
    if (!target) return
    setBusy(true)
    setError(null)
    setProgress({ done: 0, total: selectedVMs.length })

    // Fire all migrates in parallel. Each one creates its own op row
    // server-side; we don't need to track the resulting promises
    // beyond aggregating "how many dispatched OK." Per-VM progress
    // shows in the Tasks dropdown.
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
    setBusy(false)
    if (failed > 0) {
      setError(
        `${failed} of ${selectedVMs.length} dispatch(es) failed. The Tasks panel shows per-VM status.`,
      )
      // Don't auto-close on partial failure — operator should see
      // which dispatches failed before dismissing.
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
      aria-label="Migrate selected machines"
    >
      <Card
        strong
        className="w-full max-w-[600px] p-9"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Migrate {selectedVMs.length} machines</div>
        <h3 className="text-2xl mt-1 mb-4">Bulk migrate</h3>

        <p className="text-sm text-ink-2 mb-4 leading-relaxed">
          {selectedVMs.length} VM{selectedVMs.length === 1 ? '' : 's'} from{' '}
          <span className="font-mono">{sourceSummary}</span> will move to the
          target node below. Each VM dispatches as its own background
          task — watch progress in the Tasks panel. Live migration is
          attempted first; on rejection (snapshots, local CD/DVD, etc.)
          the bulk path automatically falls back to stop → migrate →
          start (no per-VM confirmation prompt).
        </p>

        <label className="block eyebrow text-[10.5px] mb-1.5">Target node</label>
        <select
          className="w-full font-sans text-sm text-ink rounded-[10px] bg-white/85 border border-line-2 px-3.5 py-2 focus:outline-none focus:ring-2 focus:ring-accent/30"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          disabled={busy || candidates.length === 0}
        >
          <option value="">Pick a node…</option>
          {candidates.map((n) => (
            <option key={n.name} value={n.name}>
              {n.name} ({Math.round((n.mem_used / Math.max(1, n.mem_total)) * 100)}% RAM)
            </option>
          ))}
        </select>

        {loadErr && (
          <p className="text-[12px] text-bad mt-1.5">{loadErr}</p>
        )}

        {busy && (
          <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(27,23,38,0.04)] border border-line-2 text-sm text-ink-2">
            Dispatching {progress.done}/{progress.total}… You can close
            this modal once all are dispatched — each migration runs in
            the background and reports to the Tasks panel.
          </div>
        )}

        {error && (
          <div className="mt-5 mb-5 p-3.5 rounded-[10px] bg-[rgba(184,58,58,0.06)] border border-[rgba(184,58,58,0.2)] text-bad text-sm">
            {error}
          </div>
        )}

        <div className="flex justify-end gap-3 mt-7">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy || !target}>
            {busy ? 'Dispatching…' : `Migrate ${selectedVMs.length} VMs`}
          </Button>
        </div>
      </Card>
    </div>,
    document.body,
  )
}
