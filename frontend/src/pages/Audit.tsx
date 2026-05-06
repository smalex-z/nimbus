import { useCallback, useEffect, useMemo, useState } from 'react'
import { listAuditEvents } from '@/api/client'
import type { AuditEvent } from '@/types'

// Audit — read-only cluster activity log surfaced under
// Infrastructure → Audit. The write side is the audit.Service that
// every mutation handler calls inline; this page is just the
// inspection surface.
//
// Filters land as URL query params on the wire (action_prefix, since,
// until, actor_id, limit, offset). The UI keeps state local for v1 so
// reload-and-share-a-link isn't a thing yet — small follow-up to mirror
// state in the URL.
//
// Pagination is offset-based with a fixed page size. Cursor-based would
// scale better past 100k rows, but the daily reaper keeps the table at
// roughly 90 days × N events/day so offset is fine for the operator's
// "show me the last hour" question that dominates this page.

const PAGE_SIZE = 100

// ACTION_CATEGORIES groups action prefixes for the filter pills. Each
// pill matches with a `LIKE prefix%` query; "All" passes empty
// action_prefix. New backend actions land without a frontend change —
// they show up under whichever prefix-pill matches them, or under "All".
const ACTION_CATEGORIES = [
  { label: 'All', prefix: '' },
  { label: 'VMs', prefix: 'vm.' },
  { label: 'Nodes', prefix: 'node.' },
  { label: 'Auth', prefix: 'auth.' },
  { label: 'Users', prefix: 'user.' },
  { label: 'Settings', prefix: 'settings.' },
] as const

export default function Audit() {
  const [events, setEvents] = useState<AuditEvent[]>([])
  const [total, setTotal] = useState(0)
  const [actionPrefix, setActionPrefix] = useState('')
  const [offset, setOffset] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<number | null>(null)

  const reload = useCallback(() => {
    setLoading(true)
    listAuditEvents({
      action_prefix: actionPrefix || undefined,
      limit: PAGE_SIZE,
      offset,
    })
      .then((res) => {
        setEvents(res.events)
        setTotal(res.total)
        setError(null)
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
      .finally(() => setLoading(false))
  }, [actionPrefix, offset])

  useEffect(() => { reload() }, [reload])

  const onPickCategory = (prefix: string) => {
    setActionPrefix(prefix)
    setOffset(0)
    setExpanded(null)
  }

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const currentPage = Math.floor(offset / PAGE_SIZE) + 1

  return (
    <div className="flex flex-col gap-5">
      <div>
        <div className="eyebrow">Cluster activity</div>
        <h2 className="text-2xl mt-1">Audit log</h2>
        <p className="text-sm text-ink-2 mt-2 leading-relaxed">
          Every state-changing action recorded with actor, IP, and outcome. Auto-pruned after the configured
          retention window (<code className="font-mono">NIMBUS_AUDIT_RETENTION_DAYS</code>; default 90).
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-1.5">
        {ACTION_CATEGORIES.map((cat) => {
          const active = cat.prefix === actionPrefix
          return (
            <button
              key={cat.label}
              type="button"
              onClick={() => onPickCategory(cat.prefix)}
              className={`font-mono text-[11px] uppercase tracking-wider px-2.5 py-1 rounded-md border cursor-pointer transition-colors ${
                active
                  ? 'bg-ink text-white border-ink'
                  : 'bg-transparent border-line-2 text-ink-2 hover:border-ink-3 hover:text-ink'
              }`}
            >
              {cat.label}
            </button>
          )
        })}
      </div>

      {error && <div className="text-bad text-sm">Failed to load: {error}</div>}

      {loading && events.length === 0 && (
        <div className="text-ink-3 font-mono text-sm">Loading…</div>
      )}

      {!loading && !error && events.length === 0 && (
        <div className="glass p-8 text-center">
          <div className="eyebrow">No events</div>
          <p className="text-sm text-ink-2 mt-2">
            Either nothing's happened recently or the filter is too narrow.
          </p>
        </div>
      )}

      {events.length > 0 && (
        <div className="glass overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line">
                <th className={headerCell}>When</th>
                <th className={headerCell}>Actor</th>
                <th className={headerCell}>Action</th>
                <th className={headerCell}>Target</th>
                <th className={`${headerCell} text-right`}>OK</th>
              </tr>
            </thead>
            <tbody>
              {events.map((evt) => (
                <EventRow
                  key={evt.id}
                  event={evt}
                  expanded={expanded === evt.id}
                  onToggle={() => setExpanded(expanded === evt.id ? null : evt.id)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {total > PAGE_SIZE && (
        <div className="flex items-center justify-between text-xs text-ink-3 font-mono">
          <span>
            {offset + 1}–{Math.min(offset + events.length, total)} of {total}
          </span>
          <div className="flex gap-1.5">
            <button
              type="button"
              disabled={offset === 0}
              onClick={() => { setOffset(Math.max(0, offset - PAGE_SIZE)); setExpanded(null) }}
              className="px-3 py-1 rounded-md border border-line-2 disabled:opacity-40 hover:border-ink-3 cursor-pointer disabled:cursor-default"
            >
              ← Prev
            </button>
            <span className="px-3 py-1">
              {currentPage} / {totalPages}
            </span>
            <button
              type="button"
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => { setOffset(offset + PAGE_SIZE); setExpanded(null) }}
              className="px-3 py-1 rounded-md border border-line-2 disabled:opacity-40 hover:border-ink-3 cursor-pointer disabled:cursor-default"
            >
              Next →
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

const headerCell =
  'text-left font-mono text-[10px] uppercase tracking-wider text-ink-3 px-3 py-2 font-medium'

// EventRow renders one log row with an expandable detail panel. Click
// the row to toggle the panel open — shows the IP, request id, full
// error message, and the structured details JSON. Each row is one DB
// row, so the indices stay stable across pagination.
function EventRow({ event, expanded, onToggle }: { event: AuditEvent; expanded: boolean; onToggle: () => void }) {
  const formattedTime = useMemo(() => {
    try {
      const d = new Date(event.created_at)
      return d.toLocaleString(undefined, {
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      })
    } catch {
      return event.created_at
    }
  }, [event.created_at])
  const target = event.target_label || event.target_id || ''
  const targetType = event.target_type ? `${event.target_type}` : ''
  return (
    <>
      <tr
        onClick={onToggle}
        className={`border-t border-line cursor-pointer hover:bg-[rgba(27,23,38,0.03)] ${
          expanded ? 'bg-[rgba(27,23,38,0.04)]' : ''
        }`}
      >
        <td className="px-3 py-2 font-mono text-[11px] text-ink-2 whitespace-nowrap">{formattedTime}</td>
        <td className="px-3 py-2">
          {event.actor_email ? (
            <div className="flex items-center gap-1.5">
              <span className="text-sm">{event.actor_email}</span>
              {event.actor_admin && (
                <span className="font-mono text-[9px] uppercase tracking-wider text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-px rounded">
                  Admin
                </span>
              )}
            </div>
          ) : (
            <span className="text-ink-3 font-mono text-[11px]">—</span>
          )}
        </td>
        <td className="px-3 py-2">
          <span className="font-mono text-[12px]">{event.action}</span>
        </td>
        <td className="px-3 py-2 text-sm text-ink-2">
          {target ? (
            <span title={targetType}>
              <span className="font-mono text-[11px] text-ink-3">{targetType ? targetType + ': ' : ''}</span>
              {target}
            </span>
          ) : (
            <span className="text-ink-3">—</span>
          )}
        </td>
        <td className="px-3 py-2 text-right">
          {event.success ? (
            <span className="font-mono text-[11px] uppercase tracking-wider text-good">ok</span>
          ) : (
            <span className="font-mono text-[11px] uppercase tracking-wider text-bad">fail</span>
          )}
        </td>
      </tr>
      {expanded && (
        <tr className="border-t border-line bg-[rgba(27,23,38,0.02)]">
          <td colSpan={5} className="px-3 py-3 text-xs">
            <dl className="grid grid-cols-[120px_1fr] gap-x-4 gap-y-1.5 font-mono text-[11px]">
              <dt className="text-ink-3">Event id</dt>
              <dd>{event.id}</dd>
              {event.ip_address && (
                <>
                  <dt className="text-ink-3">IP</dt>
                  <dd>{event.ip_address}</dd>
                </>
              )}
              {event.request_id && (
                <>
                  <dt className="text-ink-3">Request id</dt>
                  <dd>{event.request_id}</dd>
                </>
              )}
              {event.error_msg && (
                <>
                  <dt className="text-ink-3">Error</dt>
                  <dd className="text-bad whitespace-pre-wrap break-words">{event.error_msg}</dd>
                </>
              )}
              {event.details_json && (
                <>
                  <dt className="text-ink-3">Details</dt>
                  <dd>
                    <pre className="whitespace-pre-wrap break-words text-ink-2">
                      {prettyJSON(event.details_json)}
                    </pre>
                  </dd>
                </>
              )}
            </dl>
          </td>
        </tr>
      )}
    </>
  )
}

// prettyJSON pretty-prints the stringified details payload. Falls back
// to the raw string when parsing fails so a malformed value (shouldn't
// happen — backend always marshals — but defensive) still renders.
function prettyJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}
