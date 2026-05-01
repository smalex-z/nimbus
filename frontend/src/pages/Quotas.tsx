import { useEffect, useMemo, useState } from 'react'
import {
  getClusterStats,
  getQuotaSettings,
  listClusterVMs,
  listIPs,
  listNodes,
  listUsers,
  saveQuotaSettings,
  setUserQuota,
} from '@/api/client'
import type {
  ClusterStats,
  ClusterVM,
  IPAllocation,
  NodeView,
} from '@/types'
import type { QuotaSettingsView, UserManagementView } from '@/api/client'

// Quotas — admin-only sysadmin landing page. Two halves:
//
//  1. System overview — read-only at-a-glance stats fanned out across
//     existing endpoints (/api/nodes, /api/cluster/stats, /api/ips,
//     /api/cluster/vms, /api/users). No new backend endpoint; we just
//     compose what's already there. Cheap because /api/nodes and the
//     cluster snapshots are cached server-side, and we render whatever
//     loads first rather than blocking on the slowest fetch.
//
//  2. Quota controls — editable workspace defaults for VMs + GPU jobs,
//     plus a per-user override table. The override table is the same
//     user list /users uses (richer shape now includes effective +
//     override fields), filtered to non-admins because admins bypass
//     the gate entirely.
//
// Today the page is admin-only. /admin already covers cluster
// observability for fleet operators; /quotas is meant as the *single*
// place a sysadmin lands when asking "what does this Nimbus look like
// at a glance, and where are the knobs?"
export default function Quotas() {
  // Each fetch is independent; surface its own loaded state so a slow
  // /api/cluster/vms doesn't black out the rest of the page.
  const [nodes, setNodes] = useState<NodeView[] | null>(null)
  const [stats, setStats] = useState<ClusterStats | null>(null)
  const [ips, setIPs] = useState<IPAllocation[] | null>(null)
  const [vms, setVMs] = useState<ClusterVM[] | null>(null)
  const [users, setUsers] = useState<UserManagementView[] | null>(null)
  const [defaults, setDefaults] = useState<QuotaSettingsView | null>(null)

  const [error, setError] = useState<string | null>(null)
  const [tick, setTick] = useState(0)

  const reload = () => setTick((t) => t + 1)

  useEffect(() => {
    let cancelled = false
    const ok = <T,>(set: (v: T) => void) => (v: T) => { if (!cancelled) set(v) }
    const fail = (label: string) => (e: unknown) => {
      if (!cancelled) setError((prev) => prev ?? `${label}: ${e instanceof Error ? e.message : String(e)}`)
    }
    listNodes().then(ok(setNodes)).catch(fail('nodes'))
    getClusterStats().then(ok(setStats)).catch(fail('cluster stats'))
    listIPs().then(ok(setIPs)).catch(fail('ip pool'))
    listClusterVMs().then(ok(setVMs)).catch(fail('cluster vms'))
    listUsers().then(ok(setUsers)).catch(fail('users'))
    getQuotaSettings().then(ok(setDefaults)).catch(fail('quotas'))
    return () => { cancelled = true }
  }, [tick])

  // Aggregate NodeView rows into the overview's headline numbers.
  // Memoized so the cards don't recompute on unrelated state changes.
  const fleet = useMemo(() => {
    if (!nodes) return null
    const online = nodes.filter((n) => n.status === 'online')
    const offline = nodes.length - online.length
    let cpuUsed = 0
    let cpuTotal = 0
    let memUsed = 0
    let memTotal = 0
    for (const n of online) {
      cpuUsed += (n.cpu ?? 0) * n.max_cpu
      cpuTotal += n.max_cpu
      memUsed += n.mem_used
      memTotal += n.mem_total
    }
    return { online: online.length, offline, cpuUsed, cpuTotal, memUsed, memTotal }
  }, [nodes])

  const ipUsage = useMemo(() => {
    if (!ips) return null
    let used = 0
    for (const ip of ips) if (ip.status !== 'free') used++
    return { used, total: ips.length }
  }, [ips])

  const vmCounts = useMemo(() => {
    if (!vms) return null
    let running = 0
    let stopped = 0
    for (const vm of vms) {
      if (vm.status === 'running') running++
      else stopped++
    }
    return { running, stopped, total: vms.length }
  }, [vms])

  const userCounts = useMemo(() => {
    if (!users) return null
    let admins = 0
    let suspended = 0
    for (const u of users) {
      if (u.is_admin) admins++
      if (u.suspended) suspended++
    }
    return { admins, suspended, total: users.length }
  }, [users])

  // Override table only shows non-admin, non-suspended users — admins
  // bypass the gate entirely (showing them confuses the data) and
  // suspended users can't sign in to consume quota anyway.
  const overrideTargets = useMemo(() => {
    if (!users) return []
    return users.filter((u) => !u.is_admin && !u.suspended)
  }, [users])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>
          Quotas
        </h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          System overview and per-user quota controls. Members hit the workspace
          defaults unless they have an override; admins bypass quotas entirely.
        </p>
      </div>

      {error && (
        <p style={{ margin: 0, fontSize: 13, color: 'var(--err)' }}>{error}</p>
      )}

      <div>
        <div className="eyebrow" style={{ marginBottom: 10 }}>System overview</div>
        <div style={{ display: 'grid', gap: 12, gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))' }}>
          <StatCard
            label="Cluster nodes"
            value={fleet ? `${fleet.online}` : '—'}
            sub={fleet ? (fleet.offline > 0 ? `${fleet.offline} offline` : 'all online') : 'loading…'}
          />
          <StatCard
            label="CPU cores in use"
            value={fleet ? `${fleet.cpuUsed.toFixed(1)} / ${fleet.cpuTotal}` : '—'}
            sub={fleet && fleet.cpuTotal > 0 ? `${pct(fleet.cpuUsed, fleet.cpuTotal)}% utilised` : 'across online nodes'}
          />
          <StatCard
            label="RAM in use"
            value={fleet ? `${formatBytes(fleet.memUsed)} / ${formatBytes(fleet.memTotal)}` : '—'}
            sub={fleet && fleet.memTotal > 0 ? `${pct(fleet.memUsed, fleet.memTotal)}% utilised` : 'across online nodes'}
          />
          <StatCard
            label="Storage"
            value={stats ? `${formatBytes(stats.storage_used)} / ${formatBytes(stats.storage_total)}` : '—'}
            sub={stats && stats.storage_total > 0 ? `${pct(stats.storage_used, stats.storage_total)}% utilised` : 'shared + local pools'}
          />
          <StatCard
            label="IP pool"
            value={ipUsage ? `${ipUsage.used} / ${ipUsage.total}` : '—'}
            sub={ipUsage && ipUsage.total > 0 ? `${pct(ipUsage.used, ipUsage.total)}% allocated` : 'reserved + allocated'}
          />
          <StatCard
            label="VMs"
            value={vmCounts ? `${vmCounts.total}` : '—'}
            sub={vmCounts ? `${vmCounts.running} running · ${vmCounts.stopped} stopped` : 'cluster-wide'}
          />
          <StatCard
            label="Users"
            value={userCounts ? `${userCounts.total}` : '—'}
            sub={userCounts ? `${userCounts.admins} admin · ${userCounts.suspended} suspended` : 'all accounts'}
          />
        </div>
      </div>

      <DefaultsCard defaults={defaults} onSaved={(next) => { setDefaults(next); reload() }} />

      <OverridesCard
        users={overrideTargets}
        defaults={defaults}
        onSaved={reload}
      />
    </div>
  )
}

function StatCard({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="glass" style={{ padding: '16px 18px', display: 'flex', flexDirection: 'column', gap: 4 }}>
      <span style={{ fontSize: 11, color: 'var(--ink-mute)', textTransform: 'uppercase', letterSpacing: '0.06em' }}>
        {label}
      </span>
      <span style={{ fontSize: 22, fontWeight: 500, color: 'var(--ink)', fontFamily: 'Geist Mono, monospace' }}>
        {value}
      </span>
      {sub && <span style={{ fontSize: 12, color: 'var(--ink-mute)' }}>{sub}</span>}
    </div>
  )
}

// DefaultsCard — workspace VM + GPU caps in two narrow inputs. Saves
// independently from the overrides table; pressing Save persists both
// numbers in one PUT, so the admin can't end up with a half-saved
// pair if they navigated away mid-edit.
function DefaultsCard({
  defaults,
  onSaved,
}: {
  defaults: QuotaSettingsView | null
  onSaved: (next: QuotaSettingsView) => void
}) {
  const [vms, setVMs] = useState<string>('')
  const [jobs, setJobs] = useState<string>('')
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (defaults) {
      setVMs(String(defaults.member_max_vms))
      setJobs(String(defaults.member_max_active_jobs))
    }
  }, [defaults])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setSaved(false)
    setBusy(true)
    try {
      const next = await saveQuotaSettings({
        member_max_vms: Number(vms),
        member_max_active_jobs: Number(jobs),
      })
      onSaved(next)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div>
      <div className="eyebrow" style={{ marginBottom: 10 }}>Workspace defaults</div>
      <form onSubmit={submit} className="glass" style={{ padding: '24px 28px', display: 'flex', flexDirection: 'column', gap: 14 }}>
        <p style={{ margin: 0, fontSize: 12, color: 'var(--ink-mute)', lineHeight: 1.55 }}>
          Caps applied to non-admin users without a per-user override.
          Zero is allowed and means &quot;members can&apos;t provision / submit
          at all&quot;. Admins are unaffected.
        </p>
        <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
          <div className="n-field" style={{ flex: '0 0 200px' }}>
            <label className="n-label" htmlFor="quota-vms">VMs per member</label>
            <input
              id="quota-vms"
              className="n-input"
              type="number"
              min={0}
              value={vms}
              disabled={busy || defaults === null}
              onChange={(e) => setVMs(e.target.value)}
            />
          </div>
          <div className="n-field" style={{ flex: '0 0 200px' }}>
            <label className="n-label" htmlFor="quota-jobs">Active GPU jobs per member</label>
            <input
              id="quota-jobs"
              className="n-input"
              type="number"
              min={0}
              value={jobs}
              disabled={busy || defaults === null}
              onChange={(e) => setJobs(e.target.value)}
            />
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button type="submit" className="n-btn n-btn-primary" disabled={busy || defaults === null} style={{ minWidth: 100 }}>
            {busy ? 'Saving…' : 'Save defaults'}
          </button>
          {saved && <span style={{ fontSize: 13, color: 'var(--ok)' }}>Saved.</span>}
          {error && <span style={{ fontSize: 13, color: 'var(--err)' }}>{error}</span>}
        </div>
      </form>
    </div>
  )
}

// OverridesCard — table of non-admin, non-suspended users with their
// effective caps and per-user override controls. Saves are
// independent per row + dimension (VM / GPU); a clear-to-default
// button resets just that dimension. We don't show admins because
// the gate doesn't apply, and don't show suspended because they
// can't consume quota anyway.
function OverridesCard({
  users,
  defaults,
  onSaved,
}: {
  users: UserManagementView[]
  defaults: QuotaSettingsView | null
  onSaved: () => void
}) {
  return (
    <div>
      <div className="eyebrow" style={{ marginBottom: 10 }}>Per-user overrides</div>
      <div className="glass" style={{ padding: '18px 22px' }}>
        {users.length === 0 ? (
          <p style={{ margin: 0, fontSize: 13, color: 'var(--ink-mute)' }}>
            No active members yet. New users inherit the workspace defaults
            until you override here.
          </p>
        ) : (
          <div style={{ overflowX: 'auto', margin: '0 -8px' }}>
            <table className="w-full text-left" style={{ fontSize: 13, borderCollapse: 'collapse' }}>
              <thead>
                <tr style={{ color: 'var(--ink-mute)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
                  <th style={{ padding: '8px 8px', fontWeight: 500 }}>User</th>
                  <th style={{ padding: '8px 8px', fontWeight: 500 }}>VMs</th>
                  <th style={{ padding: '8px 8px', fontWeight: 500 }}>GPU jobs</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id} style={{ borderTop: '1px solid var(--line)' }}>
                    <td style={{ padding: '10px 8px' }}>
                      <div style={{ color: 'var(--ink)', fontWeight: 500 }}>{u.name || '—'}</div>
                      <div style={{ color: 'var(--ink-body)', fontFamily: 'Geist Mono, monospace', fontSize: 11 }}>
                        {u.email}
                      </div>
                    </td>
                    <td style={{ padding: '10px 8px', verticalAlign: 'top' }}>
                      <QuotaCell
                        userID={u.id}
                        dimension="vm"
                        override={u.vm_quota_override}
                        defaultValue={defaults?.member_max_vms ?? 0}
                        effective={u.effective_vm_quota}
                        onSaved={onSaved}
                      />
                    </td>
                    <td style={{ padding: '10px 8px', verticalAlign: 'top' }}>
                      <QuotaCell
                        userID={u.id}
                        dimension="gpu"
                        override={u.gpu_job_quota_override}
                        defaultValue={defaults?.member_max_active_jobs ?? 0}
                        effective={u.effective_gpu_job_quota}
                        onSaved={onSaved}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}

// QuotaCell — one (user, dimension) override widget. Three states:
//   - inheriting default — shows "default · N", typing flips it to
//     edit mode with a Save button.
//   - overridden — shows "override · N" + an inline edit input that
//     saves on blur/enter, plus a "↺ default" link to clear the override.
function QuotaCell({
  userID,
  dimension,
  override,
  defaultValue,
  effective,
  onSaved,
}: {
  userID: number
  dimension: 'vm' | 'gpu'
  override: number | null
  defaultValue: number
  effective: number
  onSaved: () => void
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<string>(String(override ?? defaultValue))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setDraft(String(override ?? defaultValue))
    setError(null)
  }, [override, defaultValue])

  const save = async () => {
    const n = Number(draft)
    if (!Number.isInteger(n) || n < 0) {
      setError('non-negative integer')
      return
    }
    setBusy(true)
    setError(null)
    try {
      await setUserQuota(userID, dimension === 'vm' ? { vm_quota_override: n } : { gpu_job_quota_override: n })
      setEditing(false)
      onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'save failed')
    } finally {
      setBusy(false)
    }
  }

  const clear = async () => {
    setBusy(true)
    setError(null)
    try {
      await setUserQuota(userID, dimension === 'vm' ? { vm_quota_override: null } : { gpu_job_quota_override: null })
      setEditing(false)
      onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'clear failed')
    } finally {
      setBusy(false)
    }
  }

  const isOverridden = override !== null

  if (!editing) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span
          style={{
            fontSize: 12,
            fontFamily: 'Geist Mono, monospace',
            padding: '3px 8px',
            borderRadius: 4,
            border: '1px solid var(--line)',
            background: isOverridden ? 'rgba(20,18,28,0.05)' : 'transparent',
            color: 'var(--ink)',
            cursor: 'pointer',
          }}
          onClick={() => setEditing(true)}
          title={isOverridden ? `override (default: ${defaultValue})` : 'inheriting workspace default — click to override'}
        >
          {isOverridden ? `override · ${effective}` : `default · ${effective}`}
        </span>
        {error && <span style={{ fontSize: 11, color: 'var(--err)' }}>{error}</span>}
      </div>
    )
  }
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
      <input
        type="number"
        min={0}
        autoFocus
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') void save()
          if (e.key === 'Escape') setEditing(false)
        }}
        disabled={busy}
        style={{
          width: 64,
          height: 26,
          fontSize: 12,
          padding: '0 8px',
          borderRadius: 6,
          border: '1px solid var(--line-strong)',
          background: 'var(--surface)',
          color: 'var(--ink)',
          outline: 'none',
        }}
      />
      <button
        type="button"
        className="n-btn"
        onClick={save}
        disabled={busy}
        style={{ height: 26, fontSize: 11, padding: '0 8px' }}
      >
        Save
      </button>
      {isOverridden && (
        <button
          type="button"
          className="n-btn-ghost"
          onClick={clear}
          disabled={busy}
          style={{
            height: 26,
            fontSize: 11,
            padding: '0 6px',
            color: 'var(--ink-mute)',
            background: 'transparent',
            border: 'none',
            cursor: 'pointer',
          }}
          title={`Clear override and revert to workspace default (${defaultValue})`}
        >
          ↺ default
        </button>
      )}
      <button
        type="button"
        className="n-btn-ghost"
        onClick={() => setEditing(false)}
        disabled={busy}
        style={{
          height: 26,
          fontSize: 11,
          padding: '0 6px',
          color: 'var(--ink-mute)',
          background: 'transparent',
          border: 'none',
          cursor: 'pointer',
        }}
      >
        Cancel
      </button>
      {error && <span style={{ fontSize: 11, color: 'var(--err)' }}>{error}</span>}
    </div>
  )
}

function formatBytes(n: number): string {
  if (!n || n < 0) return '0 B'
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}

function pct(used: number, total: number): string {
  if (total <= 0) return '0'
  const r = (used / total) * 100
  if (r >= 10) return r.toFixed(0)
  return r.toFixed(1)
}
