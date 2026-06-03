import { useCallback, useEffect, useMemo, useState } from 'react'
import { getDivergenceSummary, listDivergences } from '@/api/client'
import type {
  DivergenceListParams,
  DivergenceSummary,
  DivergenceType,
  VMDivergence,
} from '@/types'

// Divergences — read-only inventory of mismatches between the Nimbus
// DB and the Proxmox cluster snapshot. Populated by the
// internal/reconciler background loop (EPIC #296, subticket #298).
//
// Two filters: Type (category) and Status (open/resolved/all). Default
// view is open + newest-first since that's what an operator wants to
// see first thing in the morning. Resolution actions ship in
// subticket #300; for now this page is read-only.

const PAGE_SIZE = 100

const TYPE_OPTIONS: { label: string; value: DivergenceType | '' }[] = [
  { label: 'All categories', value: '' },
  { label: 'Orphaned (DB only)', value: 'orphaned' },
  { label: 'External (no Nimbus tag)', value: 'external-unmanaged' },
  { label: 'Tagged orphan (no DB row)', value: 'external-tagged-orphan' },
  { label: 'VMID mismatch (slot recycle)', value: 'vmid-mismatch' },
]

const STATUS_OPTIONS: { label: string; value: 'open' | 'resolved' | 'all' }[] = [
  { label: 'Open', value: 'open' },
  { label: 'Resolved', value: 'resolved' },
  { label: 'All', value: 'all' },
]

// Color tokens follow the spec's category-color mapping.
const TYPE_LABELS: Record<DivergenceType, { label: string; chip: string }> = {
  orphaned: {
    label: 'Orphaned',
    chip: 'text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)]',
  },
  'external-unmanaged': {
    label: 'External',
    chip: 'text-ink-2 bg-[rgba(27,23,38,0.06)] border border-line-2',
  },
  'external-tagged-orphan': {
    label: 'Tagged orphan',
    chip: 'text-bad bg-[rgba(176,38,37,0.10)] border border-[rgba(176,38,37,0.30)]',
  },
  'vmid-mismatch': {
    label: 'VMID mismatch',
    chip: 'text-ink-2 bg-[rgba(27,23,38,0.06)] border border-line-2',
  },
}

export default function Divergences() {
  const [rows, setRows] = useState<VMDivergence[]>([])
  const [total, setTotal] = useState(0)
  const [summary, setSummary] = useState<DivergenceSummary | null>(null)
  const [typeFilter, setTypeFilter] = useState<DivergenceType | ''>('')
  const [statusFilter, setStatusFilter] = useState<'open' | 'resolved' | 'all'>('open')
  const [offset, setOffset] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<number | null>(null)

  const reload = useCallback(() => {
    setLoading(true)
    const params: DivergenceListParams = {
      type: typeFilter || undefined,
      status: statusFilter,
      limit: PAGE_SIZE,
      offset,
    }
    Promise.all([listDivergences(params), getDivergenceSummary()])
      .then(([list, sum]) => {
        setRows(list.divergences)
        setTotal(list.total)
        setSummary(sum)
        setError(null)
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'failed'))
      .finally(() => setLoading(false))
  }, [typeFilter, statusFilter, offset])

  useEffect(() => {
    reload()
  }, [reload])

  // Summary widget polls every 10s. The list reload is on-filter-change
  // only — operators triggering the page refresh can use the button.
  useEffect(() => {
    const handle = setInterval(() => {
      getDivergenceSummary()
        .then(setSummary)
        .catch(() => {
          /* silent — header refresh failure shouldn't surface */
        })
    }, 10_000)
    return () => clearInterval(handle)
  }, [])

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const currentPage = Math.floor(offset / PAGE_SIZE) + 1

  return (
    <div className="flex flex-col gap-5">
      <div>
        <div className="eyebrow">Cluster integrity</div>
        <h2 className="text-2xl mt-1">Divergences</h2>
        <p className="text-sm text-ink-2 mt-2 leading-relaxed">
          Mismatches between the Nimbus DB and the live Proxmox cluster, detected by the
          background reconciler. Open rows want admin attention; resolved ones are kept for
          history (auto-cleared by the reconciler when the underlying cause vanishes).
        </p>
      </div>

      <SummaryWidget summary={summary} />

      <div className="flex flex-wrap items-center gap-2">
        <select
          value={typeFilter}
          onChange={(e) => {
            setTypeFilter(e.target.value as DivergenceType | '')
            setOffset(0)
            setExpanded(null)
          }}
          className="px-3 py-1.5 rounded-md border border-line-2 bg-white/85 text-sm text-ink cursor-pointer focus:border-ink focus:bg-white outline-none"
        >
          {TYPE_OPTIONS.map((opt) => (
            <option key={opt.label} value={opt.value}>
              {opt.label}
            </option>
          ))}
        </select>
        <select
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value as 'open' | 'resolved' | 'all')
            setOffset(0)
            setExpanded(null)
          }}
          className="px-3 py-1.5 rounded-md border border-line-2 bg-white/85 text-sm text-ink cursor-pointer focus:border-ink focus:bg-white outline-none"
        >
          {STATUS_OPTIONS.map((opt) => (
            <option key={opt.label} value={opt.value}>
              {opt.label}
            </option>
          ))}
        </select>
        <button
          type="button"
          onClick={reload}
          className="px-3 py-1.5 rounded-md border border-line-2 bg-white/85 text-sm text-ink cursor-pointer hover:border-ink"
        >
          Refresh
        </button>
      </div>

      {error && <div className="text-bad text-sm">Failed to load: {error}</div>}

      {loading && rows.length === 0 && (
        <div className="text-ink-3 font-mono text-sm">Loading…</div>
      )}

      {!loading && !error && rows.length === 0 && (
        <div className="glass p-8 text-center">
          <div className="eyebrow">No divergences</div>
          <p className="text-sm text-ink-2 mt-2">
            {statusFilter === 'open'
              ? 'The DB and the Proxmox cluster are in sync.'
              : 'No rows match the current filter.'}
          </p>
        </div>
      )}

      {rows.length > 0 && (
        <div className="glass overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line">
                <th className={headerCell}>Type</th>
                <th className={headerCell}>VMID</th>
                <th className={headerCell}>Node</th>
                <th className={headerCell}>Hostname</th>
                <th className={headerCell}>Detected</th>
                <th className={`${headerCell} text-right`}>Status</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <DivergenceRow
                  key={row.id}
                  row={row}
                  expanded={expanded === row.id}
                  onToggle={() => setExpanded(expanded === row.id ? null : row.id)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {total > PAGE_SIZE && (
        <div className="flex items-center justify-between text-xs text-ink-3 font-mono">
          <span>
            {offset + 1}–{Math.min(offset + rows.length, total)} of {total}
          </span>
          <div className="flex gap-1.5">
            <button
              type="button"
              disabled={offset === 0}
              onClick={() => {
                setOffset(Math.max(0, offset - PAGE_SIZE))
                setExpanded(null)
              }}
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
              onClick={() => {
                setOffset(offset + PAGE_SIZE)
                setExpanded(null)
              }}
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

// SummaryWidget renders the dashboard-style counter strip the spec
// calls for. Single component on this page so the operator can see
// the open-count breakdown without scrolling. The same data drives
// the dashboard sync-status widget when that lands.
function SummaryWidget({ summary }: { summary: DivergenceSummary | null }) {
  if (!summary) {
    return <div className="glass p-3 text-sm text-ink-3 font-mono">Loading summary…</div>
  }
  const cells: Array<{ label: string; value: number; chipClass: string }> = [
    {
      label: 'Open total',
      value: summary.open,
      chipClass: 'text-ink',
    },
    {
      label: 'Orphaned',
      value: summary.orphaned,
      chipClass: TYPE_LABELS['orphaned'].chip,
    },
    {
      label: 'External',
      value: summary.external_unmanaged,
      chipClass: TYPE_LABELS['external-unmanaged'].chip,
    },
    {
      label: 'Tagged orphan',
      value: summary.external_tagged_orphan,
      chipClass: TYPE_LABELS['external-tagged-orphan'].chip,
    },
    {
      label: 'VMID mismatch',
      value: summary.vmid_mismatch,
      chipClass: TYPE_LABELS['vmid-mismatch'].chip,
    },
  ]
  return (
    <div className="glass px-4 py-3 flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
      {cells.map((cell) => (
        <div key={cell.label} className="flex items-center gap-2">
          <span className="font-mono text-[10px] uppercase tracking-wider text-ink-3">
            {cell.label}
          </span>
          <span className={`font-mono text-base px-2 py-0.5 rounded ${cell.chipClass}`}>
            {cell.value}
          </span>
        </div>
      ))}
      {summary.last_detected_at && (
        <div className="ml-auto flex items-center gap-2 text-ink-3 font-mono text-[11px]">
          last detected {relativeTime(summary.last_detected_at)}
        </div>
      )}
    </div>
  )
}

function DivergenceRow({
  row,
  expanded,
  onToggle,
}: {
  row: VMDivergence
  expanded: boolean
  onToggle: () => void
}) {
  const detected = useMemo(() => {
    try {
      const d = new Date(row.detected_at)
      return d.toLocaleString(undefined, {
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      })
    } catch {
      return row.detected_at
    }
  }, [row.detected_at])
  const typeMeta = TYPE_LABELS[row.type] ?? TYPE_LABELS['external-unmanaged']
  const resolved = !!row.resolved_at

  return (
    <>
      <tr
        onClick={onToggle}
        className={`border-t border-line cursor-pointer hover:bg-[rgba(27,23,38,0.03)] ${
          expanded ? 'bg-[rgba(27,23,38,0.04)]' : ''
        }`}
      >
        <td className="px-3 py-2">
          <span
            className={`font-mono text-[10px] uppercase tracking-wider px-1.5 py-px rounded ${typeMeta.chip}`}
          >
            {typeMeta.label}
          </span>
        </td>
        <td className="px-3 py-2 font-mono text-[12px] text-ink-2">
          {row.vmid || <span className="text-ink-3">—</span>}
        </td>
        <td className="px-3 py-2 text-sm text-ink-2">
          {row.node || <span className="text-ink-3">—</span>}
        </td>
        <td className="px-3 py-2 text-sm">
          {row.hostname || <span className="text-ink-3">—</span>}
        </td>
        <td className="px-3 py-2 font-mono text-[11px] text-ink-2 whitespace-nowrap">
          {detected}
        </td>
        <td className="px-3 py-2 text-right">
          {resolved ? (
            <span className="font-mono text-[10px] uppercase tracking-wider text-good">
              resolved
            </span>
          ) : (
            <span className="font-mono text-[10px] uppercase tracking-wider text-warn">
              open
            </span>
          )}
        </td>
      </tr>
      {expanded && (
        <tr className="border-t border-line bg-[rgba(27,23,38,0.02)]">
          <td colSpan={6} className="px-3 py-3 text-xs">
            <dl className="grid grid-cols-[140px_1fr] gap-x-4 gap-y-1.5 font-mono text-[11px]">
              <dt className="text-ink-3">Nimbus ID</dt>
              <dd className="text-ink-2 break-all">{row.nimbus_id || '—'}</dd>
              <dt className="text-ink-3">Action hint</dt>
              <dd className="text-ink-2">{row.action_hint}</dd>
              <dt className="text-ink-3">First detected</dt>
              <dd className="text-ink-2">{row.first_detected_at}</dd>
              {row.resolved_at && (
                <>
                  <dt className="text-ink-3">Resolved at</dt>
                  <dd className="text-ink-2">{row.resolved_at}</dd>
                  <dt className="text-ink-3">Resolved by</dt>
                  <dd className="text-ink-2">
                    {row.resolved_by || '—'}
                    {row.resolved_action ? ` (${row.resolved_action})` : ''}
                  </dd>
                </>
              )}
              {row.details_json && (
                <>
                  <dt className="text-ink-3">Details</dt>
                  <dd className="text-ink-2 whitespace-pre-wrap break-words">
                    {pretty(row.details_json)}
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

// pretty re-serializes a details JSON string with two-space indent so
// the expanded row reads nicely. Tolerates malformed payloads — the
// reconciler always writes valid JSON, but a hand-edited row shouldn't
// break the page.
function pretty(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

// relativeTime is a tiny humanizer for the "last detected N ago" hint.
// Approximate enough; the absolute ISO timestamp is one cell over on
// the row itself.
function relativeTime(iso: string): string {
  try {
    const ms = Date.now() - new Date(iso).getTime()
    if (ms < 60_000) return 'just now'
    if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`
    if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`
    return `${Math.floor(ms / 86_400_000)}d ago`
  } catch {
    return iso
  }
}
