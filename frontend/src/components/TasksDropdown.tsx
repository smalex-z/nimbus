import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  listOperations,
  isTerminalOperationState,
  type Operation,
  type OperationState,
} from '@/api/client'

// TasksDropdown — header chip + popover that surfaces in-flight
// background operations (migrate, provision, ...). Lives next to the
// user dropdown so the operator can re-attach to a long-running task
// from any page.
//
// Design notes:
//  - Polls /api/operations every 4 s when the popover is closed and
//    every 2 s when it's open. Fast enough to feel live, slow enough
//    not to hammer the API for a passive surface.
//  - Non-admins see only their own ops (backend filters by actor_id).
//    Admin sees the cluster-wide view, which is the whole point of
//    putting this in the toolbar — bulk migrations + provision are
//    typically admin-driven.
//  - Click a row to deep-link to the relevant page (today: nodes
//    page for migrate). Future: open the original modal in
//    re-attached state. v1 just nudges the operator to where the
//    target lives.

const POLL_FAST = 2000
const POLL_SLOW = 4000

export default function TasksDropdown() {
  const [ops, setOps] = useState<Operation[]>([])
  const [open, setOpen] = useState(false)
  const popoverRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | null = null

    const tick = async () => {
      try {
        const res = await listOperations()
        if (!cancelled) setOps(res.operations)
      } catch {
        // Silent — the SPA's auth wrapper already handles 401.
        // Transient network errors just leave the prior list in
        // place; the next tick will catch up.
      }
      if (!cancelled) {
        timer = setTimeout(tick, open ? POLL_FAST : POLL_SLOW)
      }
    }

    void tick()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [open])

  // Click-outside to dismiss the popover.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (!popoverRef.current) return
      if (!popoverRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [open])

  const inflight = ops.filter((o) => !isTerminalOperationState(o.state))
  const count = inflight.length

  return (
    <div className="relative" ref={popoverRef}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-label={`Background tasks (${count} running)`}
        className="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors flex items-center gap-1.5 cursor-pointer"
      >
        <svg
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <circle cx="12" cy="12" r="10" />
          <polyline points="12 6 12 12 16 14" />
        </svg>
        {count > 0 && (
          <span className="text-[10px] font-semibold tracking-wider uppercase font-sans text-[#9a5c2e] bg-[rgba(248,175,130,0.15)] border border-[rgba(248,175,130,0.4)] px-1.5 py-px rounded">
            {count}
          </span>
        )}
      </button>

      {open && (
        <div
          className="absolute right-0 top-full mt-2 w-[360px] z-50 rounded-[12px] border border-line shadow-lg bg-white"
          style={{
            backdropFilter: 'blur(20px) saturate(140%)',
            WebkitBackdropFilter: 'blur(20px) saturate(140%)',
          }}
        >
          <div className="px-4 py-3 border-b border-line text-[12px] font-semibold uppercase tracking-wider text-ink-2">
            Background tasks
          </div>
          {ops.length === 0 ? (
            <div className="px-4 py-6 text-sm text-ink-3 text-center">
              No tasks. Start a migration or provision to see progress here.
            </div>
          ) : (
            <ul className="max-h-[400px] overflow-y-auto">
              {ops.map((op) => (
                <OpRow key={op.id} op={op} onClose={() => setOpen(false)} />
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

function OpRow({ op, onClose }: { op: Operation; onClose: () => void }) {
  const stateLabel = labelForState(op.state)
  const dotColor = colorForState(op.state)
  const ago = relativeTime(op.last_heartbeat_at)
  // Deep-link target. Migrate rows take the operator to the Admin
  // page with ?op=<id> so the Admin mount-time handler can open
  // MigrateVMModal in re-attach mode. Provision rows go to the
  // Provision page where the existing ReattachBanner detects the
  // in-flight op on its own. Unknown types fall back to the dropdown
  // (no nav).
  const href = (() => {
    if (op.type === 'vm.migrate') return `/admin?op=${op.id}`
    if (op.type === 'vm.provision') return '/provision'
    return null
  })()

  const inner = (
    <div className="flex items-start gap-2">
      <span
        className="w-2 h-2 rounded-full mt-1.5"
        style={{ background: dotColor }}
        aria-hidden="true"
      />
      <div className="flex-1 min-w-0">
        <div className="flex items-center justify-between gap-2">
          <span className="font-mono text-[13px] truncate">
            {labelForType(op.type)}
            {op.target_label ? ` ${op.target_label}` : ''}
          </span>
          <span className="text-[11px] text-ink-3 whitespace-nowrap">
            {stateLabel}
          </span>
        </div>
        {op.message && (
          <div className="text-[12px] text-ink-2 truncate">{op.message}</div>
        )}
        <div className="text-[11px] text-ink-3 mt-0.5">{ago}</div>
      </div>
    </div>
  )

  return (
    <li className="border-b border-line last:border-b-0 hover:bg-[rgba(27,23,38,0.03)]">
      {href ? (
        <Link
          to={href}
          onClick={onClose}
          className="block px-4 py-3 no-underline text-ink"
        >
          {inner}
        </Link>
      ) : (
        <div className="px-4 py-3">{inner}</div>
      )}
    </li>
  )
}

function labelForType(type: string): string {
  switch (type) {
    case 'vm.migrate':
      return 'Migrate'
    case 'vm.provision':
      return 'Provision'
    default:
      return type
  }
}

function labelForState(state: OperationState): string {
  switch (state) {
    case 'queued':
      return 'queued'
    case 'running':
      return 'running'
    case 'succeeded':
      return 'done'
    case 'failed':
      return 'failed'
    case 'cancelled':
      return 'cancelled'
  }
}

function colorForState(state: OperationState): string {
  switch (state) {
    case 'queued':
    case 'running':
      return 'var(--accent, #f8af82)'
    case 'succeeded':
      return 'var(--ok, #2f8f55)'
    case 'failed':
      return 'var(--bad, #b83a3a)'
    case 'cancelled':
      return 'var(--ink-3, #888)'
  }
}

function relativeTime(rfc3339: string): string {
  const t = Date.parse(rfc3339)
  if (Number.isNaN(t)) return ''
  const diffSec = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (diffSec < 60) return `${diffSec}s ago`
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`
  return `${Math.floor(diffSec / 86400)}d ago`
}
