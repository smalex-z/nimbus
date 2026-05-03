import { useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  deleteUser,
  listUsers,
  promoteUser,
  setUserSuspended,
} from '@/api/client'
import type { UserManagementView } from '@/api/client'
import NavDropdown from '@/components/ui/NavDropdown'
import { useAuth } from '@/hooks/useAuth'

// UsersTable renders every account the admin can see. Sorted server-side
// by created_at desc, so the most recent sign-up shows first. Verified
// status follows the live policy (admin / authorized domain / org / code
// match) so the column reflects what would happen on the user's next
// request, not a snapshot at sign-up.
//
// Lives in /authentication alongside the OAuth providers + access code
// + passwordless toggle — the four surfaces all govern who can sign in,
// so they share a page.

type PendingAction =
  | { kind: 'promote'; user: UserManagementView }
  | { kind: 'delete'; user: UserManagementView }
  | null

// Filter dimensions — each is a dropdown with an "any" sentinel that
// disables filtering on that axis. Combinations AND together: pick
// "Members" + "Unverified" + "Password-only" to find members who
// haven't verified and have no OAuth, the typical straggler set.
type RoleFilter = 'any' | 'admin' | 'member'
type StatusFilter = 'any' | 'active' | 'suspended'
type VerifiedFilter = 'any' | 'verified' | 'unverified'
type ProviderFilter = 'any' | 'password-only' | 'has-oauth'

interface UserFilters {
  role: RoleFilter
  status: StatusFilter
  verified: VerifiedFilter
  provider: ProviderFilter
  search: string
}

const DEFAULT_FILTERS: UserFilters = {
  role: 'any',
  status: 'any',
  verified: 'any',
  provider: 'any',
  search: '',
}

function rowMatchesFilters(u: UserManagementView, f: UserFilters): boolean {
  if (f.role === 'admin' && !u.is_admin) return false
  if (f.role === 'member' && u.is_admin) return false
  if (f.status === 'active' && u.suspended) return false
  if (f.status === 'suspended' && !u.suspended) return false
  if (f.verified === 'verified' && !u.verified) return false
  if (f.verified === 'unverified' && u.verified) return false
  if (f.provider === 'password-only') {
    const onlyPassword = u.providers.length === 1 && u.providers[0] === 'password'
    if (!onlyPassword) return false
  }
  if (f.provider === 'has-oauth') {
    const hasOAuth = u.providers.includes('google') || u.providers.includes('github')
    if (!hasOAuth) return false
  }
  if (f.search) {
    const q = f.search.toLowerCase()
    if (!u.name.toLowerCase().includes(q) && !u.email.toLowerCase().includes(q)) return false
  }
  return true
}

function filtersAreActive(f: UserFilters): boolean {
  return (
    f.role !== 'any' ||
    f.status !== 'any' ||
    f.verified !== 'any' ||
    f.provider !== 'any' ||
    f.search.trim() !== ''
  )
}

// onMutated bubbles every successful row-action up to the parent so the
// PasswordlessPanel's straggler count stays in sync — suspending a row
// here changes whether the OAuth-only toggle is reachable, and we don't
// want the user to have to refresh to see that. refreshTick is the
// inbound counterpart: when another panel mutates, the parent bumps
// the tick and we re-fetch.
export default function UsersTable({
  refreshTick,
  onMutated,
}: {
  refreshTick: number
  onMutated: () => void
}) {
  const { user: me } = useAuth()
  const [rows, setRows] = useState<UserManagementView[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction>(null)
  const [filters, setFilters] = useState<UserFilters>(DEFAULT_FILTERS)

  useEffect(() => {
    listUsers()
      .then(setRows)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
  }, [refreshTick])

  const filteredRows = useMemo(() => {
    if (!rows) return null
    return rows.filter((u) => rowMatchesFilters(u, filters))
  }, [rows, filters])

  const filteredCount = filteredRows?.length ?? 0
  const totalCount = rows?.length ?? 0
  const filtersActive = filtersAreActive(filters)

  return (
    <div className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <span style={{ fontSize: 15, fontWeight: 600, color: 'var(--ink)' }}>
          Accounts
        </span>
        {rows !== null && (
          <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>
            {filtersActive ? `${filteredCount} of ${totalCount}` : `${totalCount} ${totalCount === 1 ? 'user' : 'users'}`}
          </span>
        )}
      </div>

      {rows !== null && rows.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
          {/* Filter row: four compact dropdowns then a flex-growing
              search input as the last column. flex-wrap lets the search
              drop to its own line on narrow viewports rather than
              squeezing everything. Combinations AND across axes; "Any …"
              disables that axis. */}
          <FilterSelect
            ariaLabel="Role"
            value={filters.role}
            onChange={(v) => setFilters((f) => ({ ...f, role: v as RoleFilter }))}
            options={[
              { value: 'any', label: 'Any role' },
              { value: 'admin', label: 'Admins' },
              { value: 'member', label: 'Members' },
            ]}
          />
          <FilterSelect
            ariaLabel="Status"
            value={filters.status}
            onChange={(v) => setFilters((f) => ({ ...f, status: v as StatusFilter }))}
            options={[
              { value: 'any', label: 'Any status' },
              { value: 'active', label: 'Active' },
              { value: 'suspended', label: 'Suspended' },
            ]}
          />
          <FilterSelect
            ariaLabel="Verification"
            value={filters.verified}
            onChange={(v) => setFilters((f) => ({ ...f, verified: v as VerifiedFilter }))}
            options={[
              { value: 'any', label: 'Any verification' },
              { value: 'verified', label: 'Verified' },
              { value: 'unverified', label: 'Unverified' },
            ]}
          />
          <FilterSelect
            ariaLabel="Sign-in"
            value={filters.provider}
            onChange={(v) => setFilters((f) => ({ ...f, provider: v as ProviderFilter }))}
            options={[
              { value: 'any', label: 'Any sign-in' },
              { value: 'has-oauth', label: 'OAuth-linked' },
              { value: 'password-only', label: 'Password-only' },
            ]}
          />
          <input
            type="search"
            placeholder="Search by name or email…"
            value={filters.search}
            onChange={(e) => setFilters((f) => ({ ...f, search: e.target.value }))}
            style={{
              flex: 1,
              minWidth: 160,
              height: 28,
              fontSize: 12,
              padding: '0 10px',
              borderRadius: 6,
              border: '1px solid var(--line-strong)',
              background: 'var(--surface)',
              color: 'var(--ink)',
              outline: 'none',
            }}
          />
          {filtersActive && (
            <button
              type="button"
              onClick={() => setFilters(DEFAULT_FILTERS)}
              className="n-btn-ghost"
              style={{
                fontSize: 11,
                fontFamily: 'Geist Mono, monospace',
                textTransform: 'uppercase',
                letterSpacing: '0.06em',
                padding: '4px 8px',
                height: 28,
                cursor: 'pointer',
                color: 'var(--ink-mute)',
              }}
            >
              Clear
            </button>
          )}
        </div>
      )}

      {rows === null && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>Loading…</p>
      )}
      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}
      {rows !== null && rows.length === 0 && !error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          No accounts yet.
        </p>
      )}
      {rows !== null && rows.length > 0 && filteredRows !== null && filteredRows.length === 0 && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
          No accounts match the active filters.
        </p>
      )}
      {filteredRows !== null && filteredRows.length > 0 && (
        <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
          <table className="w-full text-left" style={{ fontSize: 13, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ color: 'var(--ink-mute)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
                {/* Name + email share one column — the email reads as a
                    sub-line under the name. Saves a table column on a
                    page that already has filters + status pills + a
                    Sign-in chip group competing for width. */}
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Name / Email</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Joined</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Sign-in</th>
                <th style={{ padding: '8px 8px', fontWeight: 500 }}>Status</th>
                <th style={{ padding: '8px 8px', fontWeight: 500, width: 1 }} aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {filteredRows.map((u) => (
                <tr
                  key={u.id}
                  style={{ borderTop: '1px solid var(--line)', opacity: u.suspended ? 0.55 : 1 }}
                >
                  <td style={{ padding: '10px 8px' }}>
                    <div style={{ color: 'var(--ink)', fontWeight: 500 }}>
                      {u.name || <span style={{ color: 'var(--ink-mute)' }}>—</span>}
                    </div>
                    <div
                      style={{
                        color: 'var(--ink-body)',
                        fontFamily: 'Geist Mono, monospace',
                        fontSize: 11,
                        marginTop: 2,
                      }}
                    >
                      {u.email}
                    </div>
                  </td>
                  <td style={{ padding: '10px 8px', color: 'var(--ink-body)', whiteSpace: 'nowrap' }}>
                    {formatJoined(u.created_at)}
                  </td>
                  <td style={{ padding: '10px 8px' }}>
                    <ProviderChips providers={u.providers} />
                  </td>
                  <td style={{ padding: '10px 8px' }}>
                    <UserStatusPills user={u} />
                  </td>
                  <td style={{ padding: '10px 8px', textAlign: 'right' }}>
                    <UserRowActions
                      user={u}
                      isSelf={me?.id === u.id}
                      onPromote={() => setPending({ kind: 'promote', user: u })}
                      onDelete={() => setPending({ kind: 'delete', user: u })}
                      onSuspend={async () => {
                        try {
                          await setUserSuspended(u.id, !u.suspended)
                          onMutated()
                        } catch (err) {
                          setError(err instanceof Error ? err.message : 'failed')
                        }
                      }}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {pending?.kind === 'promote' && (
        <PromoteUserModal
          user={pending.user}
          onClose={() => setPending(null)}
          onPromoted={() => {
            setPending(null)
            onMutated()
          }}
        />
      )}
      {pending?.kind === 'delete' && (
        <DeleteUserModal
          user={pending.user}
          onClose={() => setPending(null)}
          onDeleted={() => {
            setPending(null)
            onMutated()
          }}
        />
      )}
    </div>
  )
}

// UserRowActions shows the per-row "..." menu. The trigger button is
// always rendered (so column widths don't jump when self-row vs not),
// but its menu is empty-only when the row is the requester themselves —
// admins can't promote or delete themselves through this UI.
function UserRowActions({
  user,
  isSelf,
  onPromote,
  onDelete,
  onSuspend,
}: {
  user: UserManagementView
  isSelf: boolean
  onPromote: () => void
  onDelete: () => void
  onSuspend: () => void
}) {
  if (isSelf) {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  // NavDropdown's open state is internal — clicks on items inside its panel
  // are ignored by its document-mousedown close handler (panel.contains
  // returns true). For an *action* menu we want the panel to dismiss as
  // soon as the user picks something, so we synthesize a mousedown on
  // document before invoking the handler. The synthetic event's target is
  // outside the panel, which trips NavDropdown's existing close path.
  // Without this the menu stays mounted at z-1000 and visibly floats
  // above the modal backdrop.
  const dismissAndDo = (fn: () => void) => () => {
    document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    fn()
  }
  return (
    <NavDropdown
      placement="bottom-end"
      triggerOn="click"
      triggerClassName="inline-flex items-center justify-center w-7 h-7 rounded-md border border-line-2 bg-white/85 text-ink-2 hover:border-ink hover:text-ink transition-colors"
      panelClassName="rounded-lg border border-line bg-white py-1 min-w-[180px] shadow-lg"
      trigger={<MoreIcon />}
    >
      {!user.is_admin && (
        <button
          type="button"
          onClick={dismissAndDo(onPromote)}
          className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
        >
          Promote to admin
        </button>
      )}
      {user.is_admin && (
        <span className="block w-full text-left px-3 py-1.5 text-[13px] text-ink-3 cursor-default">
          Already an admin
        </span>
      )}
      <button
        type="button"
        onClick={dismissAndDo(onSuspend)}
        className="block w-full text-left px-3 py-1.5 text-[13px] text-ink hover:bg-[rgba(27,23,38,0.05)] cursor-pointer"
      >
        {user.suspended ? 'Unsuspend' : 'Suspend'}
      </button>
      <div className="my-1 border-t border-line" />
      <button
        type="button"
        onClick={dismissAndDo(onDelete)}
        className="block w-full text-left px-3 py-1.5 text-[13px] text-bad hover:bg-[rgba(184,55,55,0.06)] cursor-pointer"
      >
        Delete user…
      </button>
    </NavDropdown>
  )
}

// PromoteUserModal collects the requesting admin's password and POSTs
// to /api/users/:id/promote. The password input is the same gate the
// backend enforces — the UI doesn't pre-validate it (would leak whether
// the password is right against the wrong account); the server returns
// 401 on mismatch and we surface that.
function PromoteUserModal({
  user,
  onClose,
  onPromoted,
}: {
  user: UserManagementView
  onClose: () => void
  onPromoted: () => void
}) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

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

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await promoteUser(user.id, password)
      onPromoted()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Promote failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={`Promote ${user.name || user.email} to admin`}
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 460, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow">Promote to admin</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
          {user.name || user.email}
        </h3>
        <p style={{ margin: '0 0 18px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Grants full admin access — cluster observability, settings, and
          user management. Confirm with your password.
        </p>

        <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="n-field">
            <label className="n-label" htmlFor="promote-password">Your password</label>
            <input
              id="promote-password"
              className="n-input"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              autoFocus
            />
          </div>
          {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
          <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
            <button type="button" className="n-btn" onClick={onClose} disabled={busy}>
              Cancel
            </button>
            <button type="submit" className="n-btn n-btn-primary" disabled={busy || !password}>
              {busy ? 'Promoting…' : 'Promote'}
            </button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// DeleteUserModal is the destructive flow. The VM-disposition radio
// always shows even when the user has no VMs — it's the only safety
// step on this action and we want the operator to consciously choose,
// not have it auto-skipped because the count happened to be zero this
// minute. Dropping their VMs is *strictly* more destructive than
// dropping just the user record, so the default selection is "transfer".
function DeleteUserModal({
  user,
  onClose,
  onDeleted,
}: {
  user: UserManagementView
  onClose: () => void
  onDeleted: () => void
}) {
  const [vmAction, setVmAction] = useState<'transfer' | 'delete'>('transfer')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

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

  const submit = async () => {
    setError(null)
    setBusy(true)
    try {
      await deleteUser(user.id, vmAction)
      onDeleted()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Delete failed')
    } finally {
      setBusy(false)
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[1010] grid place-items-center p-4"
      style={{ background: 'rgba(20,18,28,0.45)', backdropFilter: 'blur(8px)' }}
      role="dialog"
      aria-modal="true"
      aria-label={`Delete ${user.name || user.email}`}
      onClick={busy ? undefined : onClose}
    >
      <div
        className="glass"
        style={{ width: '100%', maxWidth: 480, padding: '28px 32px' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="eyebrow" style={{ color: 'var(--err)' }}>Delete user</div>
        <h3 style={{ fontSize: 20, margin: '4px 0 6px' }}>
          {user.name || user.email}
        </h3>
        <p style={{ margin: '0 0 16px', fontSize: 13, color: 'var(--ink-body)', lineHeight: 1.55 }}>
          Removes this user, their sessions, and (per the option below) their
          VMs and SSH keys. This cannot be undone.
        </p>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 10, marginBottom: 18 }}>
          <DispositionOption
            checked={vmAction === 'transfer'}
            onSelect={() => setVmAction('transfer')}
            title="Take ownership of their VMs"
            description="VMs and SSH keys are reparented to your account. They keep running; you'll see them on My machines."
          />
          <DispositionOption
            checked={vmAction === 'delete'}
            onSelect={() => setVmAction('delete')}
            title="Delete their VMs"
            description="VMs are destroyed on Proxmox and their SSH keys + GPU job history are removed. Slow if they own many VMs."
          />
        </div>

        {error && <p style={{ margin: '0 0 10px', fontSize: 13, color: 'var(--err)' }}>{error}</p>}

        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button type="button" className="n-btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className="n-btn"
            onClick={submit}
            disabled={busy}
            style={{ borderColor: 'var(--err)', color: 'var(--err)' }}
          >
            {busy
              ? 'Deleting…'
              : vmAction === 'transfer'
                ? 'Delete user, take their VMs'
                : 'Delete user and their VMs'}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function DispositionOption({
  checked,
  onSelect,
  title,
  description,
}: {
  checked: boolean
  onSelect: () => void
  title: string
  description: string
}) {
  return (
    <label
      style={{
        display: 'flex',
        gap: 10,
        alignItems: 'flex-start',
        padding: '12px 14px',
        border: `1px solid ${checked ? 'var(--ink)' : 'var(--line)'}`,
        background: checked ? 'rgba(20,18,28,0.04)' : 'rgba(20,18,28,0.02)',
        borderRadius: 10,
        cursor: 'pointer',
      }}
    >
      <input
        type="radio"
        name="vm-action"
        checked={checked}
        onChange={onSelect}
        style={{ marginTop: 3 }}
      />
      <span>
        <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'var(--ink)' }}>
          {title}
        </span>
        <span style={{ display: 'block', fontSize: 12, color: 'var(--ink-body)', lineHeight: 1.5, marginTop: 2 }}>
          {description}
        </span>
      </span>
    </label>
  )
}

function ProviderChips({ providers }: { providers: string[] }) {
  if (!providers || providers.length === 0) {
    return <span style={{ fontSize: 11, color: 'var(--ink-mute)' }}>—</span>
  }
  return (
    <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}>
      {providers.map((p) => (
        <span
          key={p}
          style={{
            fontSize: 10,
            fontFamily: 'Geist Mono, monospace',
            textTransform: 'uppercase',
            letterSpacing: '0.06em',
            padding: '2px 6px',
            borderRadius: 4,
            background: 'rgba(20,18,28,0.05)',
            border: '1px solid var(--line)',
            color: 'var(--ink-body)',
          }}
        >
          {p}
        </span>
      ))}
    </span>
  )
}

function UserStatusPills({ user }: { user: UserManagementView }) {
  return (
    <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
      {user.is_admin ? (
        <span
          className="font-mono"
          style={{
            fontSize: 10,
            fontWeight: 600,
            textTransform: 'uppercase',
            letterSpacing: '0.06em',
            padding: '2px 6px',
            borderRadius: 4,
            color: '#9a5c2e',
            background: 'rgba(248,175,130,0.15)',
            border: '1px solid rgba(248,175,130,0.4)',
          }}
        >
          admin
        </span>
      ) : null}
      {user.suspended ? (
        // suspended dominates the status column — verified state is
        // moot when the user can't sign in.
        <span
          className="font-mono"
          style={{
            fontSize: 10,
            fontWeight: 600,
            textTransform: 'uppercase',
            letterSpacing: '0.06em',
            padding: '2px 6px',
            borderRadius: 4,
            color: 'var(--err)',
            background: 'rgba(184,55,55,0.08)',
            border: '1px solid rgba(184,55,55,0.25)',
          }}
        >
          suspended
        </span>
      ) : user.verified ? (
        <span className="n-pill n-pill-ok" style={{ fontSize: 10 }}>
          <span className="n-pill-dot" />
          verified
        </span>
      ) : (
        <span
          className="n-pill"
          style={{
            fontSize: 10,
            color: 'var(--ink-mute)',
            background: 'rgba(20,18,28,0.04)',
            border: '1px solid var(--line)',
          }}
        >
          unverified
        </span>
      )}
    </span>
  )
}

// formatJoined renders a relative-ish "joined" string. Recent times show
// as "today" / "Xd ago"; anything older falls back to a short date.
function formatJoined(iso: string): string {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return '—'
  const ms = Date.now() - t
  const days = Math.floor(ms / 86_400_000)
  if (days < 1) return 'today'
  if (days < 30) return `${days}d ago`
  return new Date(t).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
}

// FilterSelect is a small, dense dropdown for the row of axis filters
// above the user table. Native <select> for accessibility (keyboard
// open, screen-reader announce) styled inline. The "Any …" option
// always sits at the top of every list — picking it disables that
// axis.
//
// Active state is signalled by a stronger border and a faint tint,
// not by flipping the whole control to a dark fill. The dark-fill
// approach broke on browsers that don't fully honour `appearance:
// none` for selects (Firefox/Win, some Chromium configs render the
// OS-native "filled select" chrome — a striped/zigzag pattern —
// over our background, looking horrific). Keeping the surface white
// dodges the issue entirely.
function FilterSelect({
  ariaLabel,
  value,
  onChange,
  options,
}: {
  ariaLabel: string
  value: string
  onChange: (next: string) => void
  options: { value: string; label: string }[]
}) {
  const isDefault = options[0]?.value === value
  return (
    <select
      aria-label={ariaLabel}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      style={{
        height: 28,
        fontSize: 12,
        padding: '0 24px 0 10px',
        borderRadius: 6,
        border: `1px solid ${isDefault ? 'var(--line-strong)' : 'var(--ink)'}`,
        backgroundColor: isDefault ? 'var(--surface)' : 'rgba(20, 18, 28, 0.05)',
        color: 'var(--ink)',
        fontWeight: isDefault ? 400 : 500,
        cursor: 'pointer',
        appearance: 'none',
        WebkitAppearance: 'none',
        MozAppearance: 'none',
        // Inline caret so the dropdown reads as a select without the
        // OS-native chrome. Always dark — we never invert.
        backgroundImage: `url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='10' height='6' viewBox='0 0 10 6'><path fill='none' stroke='%2363606E' stroke-width='1.5' stroke-linecap='round' stroke-linejoin='round' d='M1 1l4 4 4-4'/></svg>")`,
        backgroundRepeat: 'no-repeat',
        backgroundPosition: 'right 8px center',
      }}
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

// MoreIcon — three solid horizontal dots. Replaces the ⋯ unicode glyph
// that was rendering blurry: at small sizes the OS-supplied font fell
// back to a hinted glyph that read as a fuzzy bar more than three
// distinct dots. Solid SVG circles render crisply at every zoom.
function MoreIcon() {
  return (
    <svg width={16} height={16} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <circle cx={5} cy={12} r={1.75} />
      <circle cx={12} cy={12} r={1.75} />
      <circle cx={19} cy={12} r={1.75} />
    </svg>
  )
}
