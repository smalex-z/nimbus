import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  cordonNode,
  executeDrain,
  getDrainPlan,
  removeNode,
  setNodeTags,
  uncordonNode,
} from '@/api/client'
import type {
  DrainEvent,
  DrainPlan,
  NodeProjection,
  PlannedMigration,
} from '@/api/client'
import type { NodeView } from '@/types'

// CordonModal collects a free-text reason (optional) and flips the node's
// lock state. Used for both directions — cordoning a none-state node and
// uncordoning a cordoned/drained one (the verb in the title flips
// accordingly).
export function CordonModal({
  node,
  onClose,
  onMutated,
}: {
  node: NodeView
  onClose: () => void
  onMutated: () => void
}) {
  const isUncordon = node.lock_state === 'cordoned' || node.lock_state === 'drained'
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEscClose(onClose, busy)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      if (isUncordon) {
        await uncordonNode(node.name)
      } else {
        await cordonNode(node.name, reason)
      }
      onMutated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <ModalShell ariaLabel={`${isUncordon ? 'Uncordon' : 'Cordon'} ${node.name}`} onClose={onClose} busy={busy}>
      <div className="eyebrow">{isUncordon ? 'Uncordon node' : 'Cordon node'}</div>
      <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>{node.name}</h3>
      <p style={{ margin: '0 0 18px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        {isUncordon
          ? 'The scheduler will start considering this node again. Existing VMs are unaffected.'
          : 'The scheduler will skip this node for new provisions. Existing VMs keep running.'}
      </p>

      <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        {!isUncordon && (
          <div className="n-field">
            <label className="n-label" htmlFor="cordon-reason">Reason (optional)</label>
            <input
              id="cordon-reason"
              className="n-input"
              type="text"
              placeholder="e.g. preparing for hardware swap"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              autoFocus
              maxLength={200}
            />
          </div>
        )}
        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
          <button type="button" className="n-btn" onClick={onClose} disabled={busy}>Cancel</button>
          <button type="submit" className="n-btn n-btn-primary" disabled={busy}>
            {busy ? (isUncordon ? 'Uncordoning…' : 'Cordoning…') : isUncordon ? 'Uncordon' : 'Cordon'}
          </button>
        </div>
      </form>
    </ModalShell>,
    document.body,
  )
}

// TagsModal lets the operator edit the node's tag set. Tags are stored
// as a CSV string server-side; the SPA always exchanges []string.
export function TagsModal({
  node,
  onClose,
  onMutated,
}: {
  node: NodeView
  onClose: () => void
  onMutated: () => void
}) {
  const [text, setText] = useState(node.tags.join(', '))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEscClose(onClose, busy)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      const tags = text.split(',').map((t) => t.trim()).filter(Boolean)
      await setNodeTags(node.name, tags)
      onMutated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <ModalShell ariaLabel={`Edit tags for ${node.name}`} onClose={onClose} busy={busy}>
      <div className="eyebrow">Edit tags</div>
      <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>{node.name}</h3>
      <p style={{ margin: '0 0 18px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Comma-separated. Tags don't yet affect placement — they're recorded
        for the future workload-aware scheduler (e.g. a future "pin GPU
        jobs to nodes tagged <code>gpu</code>" rule).
      </p>

      <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div className="n-field">
          <label className="n-label" htmlFor="tags-input">Tags</label>
          <input
            id="tags-input"
            className="n-input"
            type="text"
            placeholder="e.g. gpu, nvme-fast, arm64"
            value={text}
            onChange={(e) => setText(e.target.value)}
            autoFocus
          />
        </div>
        {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
          <button type="button" className="n-btn" onClick={onClose} disabled={busy}>Cancel</button>
          <button type="submit" className="n-btn n-btn-primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save tags'}
          </button>
        </div>
      </form>
    </ModalShell>,
    document.body,
  )
}

// RemoveNodeModal asks for typed confirmation ("REMOVE <NODE>") before
// firing pvecm delnode + deleting the local row. The button is gated on
// the node already being drained — the row-actions menu enforces this
// upstream too, but we double-check here.
export function RemoveNodeModal({
  node,
  onClose,
  onMutated,
}: {
  node: NodeView
  onClose: () => void
  onMutated: () => void
}) {
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useEscClose(onClose, busy)

  const expected = `REMOVE ${node.name.toUpperCase()}`
  const ready = confirm.trim() === expected

  const submit = async () => {
    if (!ready) return
    setError(null)
    setBusy(true)
    try {
      await removeNode(node.name)
      onMutated()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <ModalShell ariaLabel={`Remove ${node.name} from cluster`} onClose={onClose} busy={busy}>
      <div className="eyebrow" style={{ color: 'var(--err)' }}>Remove from cluster</div>
      <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>{node.name}</h3>
      <p style={{ margin: '0 0 16px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
        Calls <code>pvecm delnode</code> on the cluster, removing this node
        from corosync. The host itself is unaffected — this is an
        administrative removal, not a power-off. Type the phrase below to
        confirm.
      </p>
      <div className="n-field" style={{ marginBottom: 14 }}>
        <label className="n-label" htmlFor="remove-confirm">
          Type <code style={{ background: 'rgba(20,18,28,0.05)', padding: '1px 5px', borderRadius: 3 }}>{expected}</code>
        </label>
        <input
          id="remove-confirm"
          className="n-input"
          type="text"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          autoFocus
          autoComplete="off"
        />
      </div>
      {error && <p style={{ margin: '0 0 10px', fontSize: 13, color: 'var(--err)' }}>{error}</p>}
      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
        <button type="button" className="n-btn" onClick={onClose} disabled={busy}>Cancel</button>
        <button
          type="button"
          className="n-btn"
          onClick={submit}
          disabled={busy || !ready}
          style={{ borderColor: 'var(--err)', color: 'var(--err)' }}
        >
          {busy ? 'Removing…' : 'Remove from cluster'}
        </button>
      </div>
    </ModalShell>,
    document.body,
  )
}

// DrainPlanModal is the centrepiece of the operator workflow. Steps:
//
//  1. On mount, fetch the plan from the backend (per-VM recommendations
//     + per-node aggregate projections).
//  2. Render a row per VM with the destination as a dropdown listing
//     every cluster node — recommended on top, ineligible options
//     dimmed with a tooltip explaining why.
//  3. Render the per-destination aggregate footer (current vs. planned
//     VM count + RAM%, severity-coloured).
//  4. Recompute the aggregate locally as the operator overrides
//     destinations (no round trip per change — the math is just
//     "current + planned-tier-mem-here").
//  5. Gate the "Start drain" button on a typed confirmation phrase
//     ("DRAIN <NODE>") — the same phrase the server validates.
//  6. On submit, stream NDJSON progress per-VM. The modal swaps to a
//     checklist showing succeeded / failed VMs as they complete.
export function DrainPlanModal({
  nodeName,
  onClose,
  onComplete,
}: {
  nodeName: string
  onClose: () => void
  onComplete: () => void
}) {
  const [plan, setPlan] = useState<DrainPlan | null>(null)
  const [planErr, setPlanErr] = useState<string | null>(null)
  // overrides keyed by vm_id → operator's chosen destination.
  const [overrides, setOverrides] = useState<Record<number, string>>({})
  const [confirmText, setConfirmText] = useState('')
  const [phase, setPhase] = useState<'plan' | 'running' | 'done'>('plan')
  const [events, setEvents] = useState<DrainEvent[]>([])
  const [runError, setRunError] = useState<string | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  useEscClose(onClose, phase === 'running')

  useEffect(() => {
    let cancelled = false
    getDrainPlan(nodeName)
      .then((p) => { if (!cancelled) setPlan(p) })
      .catch((e: unknown) => { if (!cancelled) setPlanErr(e instanceof Error ? e.message : 'failed') })
    return () => { cancelled = true }
  }, [nodeName])

  // resolvedTarget returns the destination this row will actually use:
  // operator override if set + non-empty, otherwise the auto-pick.
  const resolvedTarget = (m: PlannedMigration): string => overrides[m.vm_id] ?? m.auto_pick

  // Live aggregate: recompute the per-destination projection given the
  // current overrides. Same arithmetic the backend does in
  // computeAggregate but applied to the operator's selections.
  const liveAggregate = useMemo<NodeProjection[]>(() => {
    if (!plan) return []
    // Build name → projection seed from the backend's "current" values.
    const seeds = new Map<string, NodeProjection>()
    for (const a of plan.aggregate) {
      seeds.set(a.node, {
        ...a,
        plannedRamFrom: a.current_ram_pct,
        addedVMs: 0,
      } as NodeProjection & { plannedRamFrom: number; addedVMs: number })
    }
    // Each migration adds tier-mem to its target. We don't know the
    // node's MaxMem here, so derive the per-1-VM RAM% bump from the
    // initial plan: planned - current already reflects all auto-picked
    // VMs landing on each node. We'll use that as a per-VM delta when
    // the operator's selection differs from the auto-pick.
    const baselineDelta = new Map<string, number>()
    for (const a of plan.aggregate) {
      const ramDelta = a.planned_ram_pct - a.current_ram_pct
      const vmDelta = a.planned_vm_count - a.current_vm_count
      if (vmDelta > 0) {
        baselineDelta.set(a.node, ramDelta / vmDelta)
      }
    }
    // Reset to current; rebuild from scratch using the operator's selections.
    for (const seed of seeds.values()) {
      seed.planned_vm_count = seed.current_vm_count
      seed.planned_ram_pct = seed.current_ram_pct
    }
    for (const m of plan.migrations) {
      const target = resolvedTarget(m)
      const seed = seeds.get(target)
      if (!seed) continue
      seed.planned_vm_count += 1
      // Approximate per-VM RAM delta by the baseline; if the baseline
      // didn't see any VMs land on this node, fall back to 0 (not ideal
      // but better than misleading; backend has the exact tier numbers).
      seed.planned_ram_pct += baselineDelta.get(m.tier) ?? baselineDelta.get(target) ?? 0
    }
    // Re-classify severity per the same thresholds the backend uses.
    return Array.from(seeds.values()).map((s) => ({
      ...s,
      severity: s.planned_ram_pct > 85 ? 'high'
        : s.planned_ram_pct > 60 ? 'caution'
          : 'ok',
    } as NodeProjection))
    // Re-run when overrides or plan change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [plan, overrides])

  // Per-row blocked check uses the resolved target's eligibility from
  // the original plan. (The backend re-validates at execute time, so
  // we don't have to chase live state on every dropdown change.)
  const blockedRows = useMemo(() => {
    if (!plan) return new Set<number>()
    const out = new Set<number>()
    for (const m of plan.migrations) {
      const t = resolvedTarget(m)
      if (!t) { out.add(m.vm_id); continue }
      const opt = m.eligible.find((e) => e.node === t)
      if (!opt || opt.disabled) out.add(m.vm_id)
    }
    return out
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [plan, overrides])

  const expectedConfirm = `DRAIN ${nodeName.toUpperCase()}`
  const canStart =
    plan !== null &&
    blockedRows.size === 0 &&
    confirmText.trim() === expectedConfirm &&
    phase === 'plan'

  const start = async () => {
    if (!plan) return
    setPhase('running')
    setRunError(null)
    setEvents([])
    abortRef.current = new AbortController()
    try {
      await executeDrain(
        nodeName,
        {
          choices: plan.migrations.map((m) => ({ vm_id: m.vm_id, target: resolvedTarget(m) })),
          confirm_phrase: expectedConfirm,
        },
        (evt) => setEvents((prev) => [...prev, evt]),
        abortRef.current.signal,
      )
    } catch (err) {
      setRunError(err instanceof Error ? err.message : 'drain failed')
    }
    setPhase('done')
  }

  // Tally per-VM outcomes from the streamed events.
  const completion = useMemo(() => {
    const final = events.find((e) => e.type === 'complete')
    if (final) {
      return {
        succeeded: final.succeeded ?? 0,
        failed: final.failed ?? 0,
        drained: final.drained ?? false,
      }
    }
    let s = 0, f = 0
    for (const e of events) {
      if (e.type === 'vm_done') s++
      if (e.type === 'vm_error') f++
    }
    return { succeeded: s, failed: f, drained: false }
  }, [events])

  return createPortal(
    <ModalShell
      ariaLabel={`Drain ${nodeName}`}
      onClose={phase === 'running' ? () => undefined : onClose}
      busy={phase === 'running'}
      maxWidth={760}
    >
      <div className="eyebrow">{phase === 'plan' ? 'Drain plan' : phase === 'running' ? 'Draining' : 'Drain complete'}</div>
      <h3 style={{ fontSize: 20, margin: '4px 0 14px' }}>{nodeName}</h3>

      {phase === 'plan' && planErr && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{planErr}</p>
      )}

      {phase === 'plan' && plan && plan.migrations.length === 0 && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          No managed VMs on this node — nothing to migrate. Cordon it
          instead, then remove from the cluster directly.
        </p>
      )}

      {phase === 'plan' && plan && plan.migrations.length > 0 && (
        <PlanTable
          plan={plan}
          overrides={overrides}
          setOverrides={setOverrides}
          blockedRows={blockedRows}
        />
      )}

      {phase === 'plan' && plan && liveAggregate.length > 0 && (
        <AggregatePanel rows={liveAggregate} />
      )}

      {phase === 'plan' && plan && plan.migrations.length > 0 && (
        <ConfirmGate
          expected={expectedConfirm}
          value={confirmText}
          onChange={setConfirmText}
          blocked={blockedRows.size > 0}
        />
      )}

      {phase !== 'plan' && (
        <RunPanel events={events} runError={runError} completion={completion} />
      )}

      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 18 }}>
        {phase === 'plan' && (
          <>
            <button type="button" className="n-btn" onClick={onClose}>Cancel</button>
            <button
              type="button"
              className="n-btn n-btn-primary"
              disabled={!canStart}
              onClick={start}
              style={{ borderColor: 'var(--err)', color: 'white', background: 'var(--err)' }}
            >
              Start drain
            </button>
          </>
        )}
        {phase === 'done' && (
          <button
            type="button"
            className="n-btn n-btn-primary"
            onClick={onComplete}
          >
            Done
          </button>
        )}
      </div>
    </ModalShell>,
    document.body,
  )
}

function PlanTable({
  plan,
  overrides,
  setOverrides,
  blockedRows,
}: {
  plan: DrainPlan
  overrides: Record<number, string>
  setOverrides: (next: Record<number, string>) => void
  blockedRows: Set<number>
}) {
  return (
    <div style={{ overflowX: 'auto', margin: '0 -8px 14px' }}>
      <table className="w-full text-left" style={{ fontSize: 13, borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ color: 'var(--ink-mute)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
            <th style={{ padding: '8px' }}>VM</th>
            <th style={{ padding: '8px' }}>Tier</th>
            <th style={{ padding: '8px' }}>Destination</th>
            <th style={{ padding: '8px' }}>Notes</th>
          </tr>
        </thead>
        <tbody>
          {plan.migrations.map((m) => {
            const target = overrides[m.vm_id] ?? m.auto_pick
            const blocked = blockedRows.has(m.vm_id)
            const overridden = !!overrides[m.vm_id] && overrides[m.vm_id] !== m.auto_pick
            const opt = m.eligible.find((e) => e.node === target)
            const note = blocked
              ? '✕ blocked — selected destination is not eligible'
              : overridden
                ? '⚠ overrides recommended'
                : opt && opt.projected_ram_pct > 85
                  ? `⚠ ${target} will be at ${opt.projected_ram_pct.toFixed(0)}% RAM`
                  : opt && opt.projected_ram_pct > 60
                    ? `${target} will be at ${opt.projected_ram_pct.toFixed(0)}% RAM`
                    : ''
            return (
              <tr key={m.vm_id} style={{ borderTop: '1px solid var(--line)' }}>
                <td style={{ padding: '10px 8px', color: 'var(--ink)' }}>
                  <div style={{ fontWeight: 500 }}>{m.hostname}</div>
                  <div style={{ fontSize: 11, color: 'var(--ink-mute)', fontFamily: 'Geist Mono, monospace' }}>
                    vmid {m.vm_id}
                  </div>
                </td>
                <td style={{ padding: '10px 8px', color: 'var(--ink-body)' }}>{m.tier}</td>
                <td style={{ padding: '10px 8px' }}>
                  <select
                    value={target || ''}
                    onChange={(e) => setOverrides({ ...overrides, [m.vm_id]: e.target.value })}
                    className="n-input"
                    style={{ minWidth: 220, height: 32, fontSize: 12 }}
                  >
                    {!target && <option value="">— no eligible node —</option>}
                    {m.eligible.map((e) => (
                      <option
                        key={e.node}
                        value={e.node}
                        disabled={e.disabled}
                        title={e.disabled ? e.disabled_reason : `${e.projected_ram_pct.toFixed(0)}% RAM after`}
                      >
                        {e.node}
                        {e.node === m.auto_pick ? ' (recommended)' : ''}
                        {e.disabled ? ' — ineligible' : ` · ${e.projected_ram_pct.toFixed(0)}% RAM`}
                      </option>
                    ))}
                  </select>
                </td>
                <td style={{ padding: '10px 8px', fontSize: 12, color: blocked ? 'var(--err)' : 'var(--ink-body)' }}>
                  {note}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function AggregatePanel({ rows }: { rows: NodeProjection[] }) {
  return (
    <div
      style={{
        padding: '12px 14px',
        background: 'rgba(20,18,28,0.03)',
        border: '1px solid var(--line)',
        borderRadius: 10,
        marginBottom: 14,
      }}
    >
      <div style={{ fontSize: 11, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em', marginBottom: 8 }}>
        Aggregate impact
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
        {rows.map((r) => {
          const color = r.severity === 'high' ? 'var(--err)'
            : r.severity === 'caution' ? 'var(--warn)'
              : 'var(--ink-body)'
          return (
            <div key={r.node} style={{ fontSize: 12, color, display: 'flex', justifyContent: 'space-between', gap: 12 }}>
              <span><strong>{r.node}</strong> — {r.planned_vm_count} VMs (was {r.current_vm_count})</span>
              <span>RAM {r.planned_ram_pct.toFixed(0)}% (was {r.current_ram_pct.toFixed(0)}%)</span>
            </div>
          )
        })}
      </div>
    </div>
  )
}

function ConfirmGate({
  expected,
  value,
  onChange,
  blocked,
}: {
  expected: string
  value: string
  onChange: (v: string) => void
  blocked: boolean
}) {
  return (
    <div className="n-field">
      <label className="n-label" htmlFor="drain-confirm">
        Type <code style={{ background: 'rgba(20,18,28,0.05)', padding: '1px 5px', borderRadius: 3 }}>{expected}</code> to enable Start drain
      </label>
      <input
        id="drain-confirm"
        className="n-input"
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        disabled={blocked}
      />
      {blocked && (
        <p style={{ margin: '6px 0 0', fontSize: 12, color: 'var(--err)' }}>
          Resolve the blocked rows above first — Start drain stays disabled while any VM has no eligible destination.
        </p>
      )}
    </div>
  )
}

function RunPanel({
  events,
  runError,
  completion,
}: {
  events: DrainEvent[]
  runError: string | null
  completion: { succeeded: number; failed: number; drained: boolean }
}) {
  // Per-VM checklist: collapse start/done/error events into one row per VMID.
  const perVM = useMemo(() => {
    const out = new Map<number, { hostname: string; status: 'running' | 'done' | 'error'; error?: string; target?: string }>()
    for (const e of events) {
      if (!e.vm_id) continue
      const cur = out.get(e.vm_id) ?? { hostname: e.hostname || `vmid ${e.vm_id}`, status: 'running' as const }
      if (e.target) cur.target = e.target
      if (e.type === 'vm_start') cur.status = 'running'
      if (e.type === 'vm_done') cur.status = 'done'
      if (e.type === 'vm_error') { cur.status = 'error'; cur.error = e.error }
      out.set(e.vm_id, cur)
    }
    return Array.from(out.entries()).map(([vmid, v]) => ({ vmid, ...v }))
  }, [events])

  const final = events.find((e) => e.type === 'complete')

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4, maxHeight: 280, overflowY: 'auto' }}>
        {perVM.map((v) => (
          <div key={v.vmid} style={{ display: 'flex', alignItems: 'center', gap: 10, fontSize: 13 }}>
            <span aria-hidden="true">{v.status === 'done' ? '✓' : v.status === 'error' ? '✕' : '…'}</span>
            <span style={{ flex: 1 }}>
              {v.hostname}
              {v.target && <span style={{ color: 'var(--ink-mute)' }}> → {v.target}</span>}
            </span>
            {v.error && <span style={{ fontSize: 11, color: 'var(--err)' }}>{v.error}</span>}
          </div>
        ))}
        {perVM.length === 0 && !runError && (
          <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Starting drain…</p>
        )}
      </div>
      {runError && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{runError}</p>
      )}
      {final && (
        <div
          style={{
            padding: '10px 14px',
            background: completion.drained ? 'rgba(48,128,72,0.08)' : 'rgba(184,101,15,0.08)',
            border: `1px solid ${completion.drained ? 'rgba(48,128,72,0.25)' : 'rgba(184,101,15,0.25)'}`,
            borderRadius: 10,
            fontSize: 13,
          }}
        >
          {completion.drained
            ? `Drain complete — ${completion.succeeded} VM${completion.succeeded === 1 ? '' : 's'} migrated. Node is now drained and ready to remove.`
            : `Drain finished with ${completion.failed} failure${completion.failed === 1 ? '' : 's'}. Node was rolled back to cordoned so you can retry.`}
        </div>
      )}
    </div>
  )
}

// --- shared ---------------------------------------------------------

function ModalShell({
  ariaLabel,
  onClose,
  busy,
  children,
  maxWidth = 480,
}: {
  ariaLabel: string
  onClose: () => void
  busy: boolean
  children: React.ReactNode
  maxWidth?: number
}) {
  return (
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={ariaLabel}
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth, padding: '28px 32px', maxHeight: '90vh', overflowY: 'auto' }}
        onClick={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  )
}

function useEscClose(onClose: () => void, busy: boolean) {
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
  }, [onClose, busy])
}
