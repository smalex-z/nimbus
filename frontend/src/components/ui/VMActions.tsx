import { useState } from 'react'
import NavDropdown from '@/components/ui/NavDropdown'
import type { VMLifecycleOp } from '@/api/client'

// VMActions renders the per-row power-control surface used in both the admin
// cluster table and the user's My Machines page.
//
// Inline button: Start when status==='stopped', Shutdown when running. The
// "…" menu carries Restart, Force stop, and Remove; Remove is disabled for
// rows nimbus didn't create (separation-of-powers — those VMs belong to
// another instance or a hand-built workload).
//
// Callers supply onLifecycle (handles start/shutdown/stop/reboot) and
// onRemove (the destructive trash flow). Either may be omitted to hide the
// inline button or the menu Remove item respectively. busy disables every
// affordance while a request is in flight.

export type VMActionsStatus = 'running' | 'stopped' | 'paused' | 'unknown'

interface VMActionsProps {
  hostname: string
  status: VMActionsStatus
  // canRemove is the "this row is owned by us" gate. NIMBUS rows pass true;
  // FOREIGN/EXTERNAL pass false → Remove is rendered but disabled with a
  // tooltip explaining why.
  canRemove: boolean
  onLifecycle: (op: VMLifecycleOp) => Promise<void> | void
  onRemove?: () => void
  busy?: boolean
}

export default function VMActions({
  hostname,
  status,
  canRemove,
  onLifecycle,
  onRemove,
  busy = false,
}: VMActionsProps) {
  const [running, setRunning] = useState(false)
  const isBusy = busy || running

  const fire = async (op: VMLifecycleOp) => {
    if (isBusy) return
    setRunning(true)
    try {
      await onLifecycle(op)
    } finally {
      setRunning(false)
    }
  }

  const isOn = status === 'running' || status === 'paused'

  return (
    <div className="flex gap-1.5">
      {/* Inline toggle: Start when off, Shutdown when on. */}
      <button
        type="button"
        onClick={() => fire(isOn ? 'shutdown' : 'start')}
        disabled={isBusy}
        className="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
        title={isOn ? `Shutdown ${hostname}` : `Start ${hostname}`}
        aria-label={isOn ? `Shutdown ${hostname}` : `Start ${hostname}`}
      >
        {isOn ? <ShutdownIcon /> : <PlayIcon />}
      </button>

      {/* … menu — click-only so brushing the cursor through a dense table
          doesn't pop every row's panel. */}
      <NavDropdown
        placement="bottom-end"
        triggerOn="click"
        triggerClassName="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink hover:border-ink transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
        trigger={<MoreIcon />}
      >
        <MenuItem
          label="Restart"
          disabled={isBusy || !isOn}
          disabledReason={!isOn ? 'VM is not running' : undefined}
          onClick={() => fire('reboot')}
        />
        <MenuItem
          label="Force stop"
          disabled={isBusy || !isOn}
          disabledReason={!isOn ? 'VM is already off' : undefined}
          onClick={() => fire('stop')}
        />
        <div className="my-1 border-t border-line" />
        <MenuItem
          label="Remove"
          danger
          disabled={isBusy || !canRemove || !onRemove}
          disabledReason={
            !canRemove
              ? 'This VM was not created by nimbus — manage it through Proxmox.'
              : undefined
          }
          onClick={() => onRemove?.()}
        />
      </NavDropdown>
    </div>
  )
}

function MenuItem({
  label,
  onClick,
  disabled,
  disabledReason,
  danger,
}: {
  label: string
  onClick: () => void
  disabled?: boolean
  disabledReason?: string
  danger?: boolean
}) {
  const base =
    'block w-full px-3 py-1.5 text-sm text-left transition-colors cursor-pointer'
  const cls = disabled
    ? `${base} text-ink-3 cursor-not-allowed`
    : danger
      ? `${base} text-bad hover:bg-[rgba(184,58,58,0.06)]`
      : `${base} text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink`
  return (
    <button
      type="button"
      role="menuitem"
      className={cls}
      onClick={() => {
        if (!disabled) onClick()
      }}
      disabled={disabled}
      title={disabled ? disabledReason : undefined}
    >
      {label}
    </button>
  )
}

function PlayIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polygon points="6 4 20 12 6 20 6 4" />
    </svg>
  )
}

function ShutdownIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
      <line x1="12" y1="2" x2="12" y2="12" />
    </svg>
  )
}

function MoreIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <circle cx="5" cy="12" r="1.6" />
      <circle cx="12" cy="12" r="1.6" />
      <circle cx="19" cy="12" r="1.6" />
    </svg>
  )
}
