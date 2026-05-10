// NodeImpactPanel — shared "show what this action does to RAM% per
// node" preview, used by every flow that moves VMs around (single-VM
// migrate, drain, multi-VM migrate). Lifted from per-modal copies in
// MigrateVMModal + NodeActionModals so the visual vocabulary stays
// consistent — operators recognize the panel across surfaces.
//
// Severity coloring matches the backend's >85 (high) / >60 (caution)
// thresholds. The label slot lets the calling modal frame the panel
// (e.g. "Node impact" for migrate, "Aggregate impact" for drain) so
// the same component reads naturally in both contexts.

export interface NodeProjectionRow {
  node: string
  // currentRamPct / plannedRamPct are 0-100 percent values. Severity
  // applies to the planned value.
  currentRamPct: number
  plannedRamPct: number
  severity: 'ok' | 'caution' | 'high'
  // Optional VM-count delta. When undefined, the row shows only the
  // RAM line; when set, it reads "+N VM(s)" or "−N" alongside.
  // Drain uses {currentVMCount, plannedVMCount}; migrate uses ±1.
  vmDelta?: number
}

export interface NodeImpactPanelProps {
  rows: NodeProjectionRow[]
  // label is the eyebrow above the rows. Defaults to "Node impact"
  // (the migrate caller's wording); drain passes "Aggregate impact".
  label?: string
}

export default function NodeImpactPanel({ rows, label = 'Node impact' }: NodeImpactPanelProps) {
  if (rows.length === 0) return null
  return (
    <div className="mt-5 mb-3 p-3.5 rounded-[10px] bg-[rgba(20,18,28,0.03)] border border-line-2">
      <div className="text-[11px] uppercase tracking-wider text-ink-3 mb-2 font-mono">
        {label}
      </div>
      <div className="flex flex-col gap-1.5">
        {rows.map((row) => (
          <ImpactRow key={row.node} row={row} />
        ))}
      </div>
    </div>
  )
}

function ImpactRow({ row }: { row: NodeProjectionRow }) {
  // Project's CSS variable is named --err (not --bad — Tailwind has
  // a `bad` color name but no matching CSS var, which silently
  // rendered as default ink black before this fix). --warn / --ink-body
  // exist; --err is the red used elsewhere in the app for the same
  // "danger" meaning.
  const color =
    row.severity === 'high'
      ? 'var(--err)'
      : row.severity === 'caution'
        ? 'var(--warn)'
        : 'var(--ink-body)'
  const deltaLabel = (() => {
    if (row.vmDelta === undefined || row.vmDelta === 0) return null
    const sign = row.vmDelta > 0 ? '+' : '−'
    const n = Math.abs(row.vmDelta)
    return `${sign}${n} VM${n === 1 ? '' : 's'}`
  })()
  return (
    <div
      style={{ color }}
      className="flex justify-between items-baseline gap-3 text-[12px] font-mono"
    >
      <span>
        <strong className="text-ink">{row.node}</strong>
        {deltaLabel && <span className="text-ink-3"> ({deltaLabel})</span>}
      </span>
      <span>
        RAM {row.plannedRamPct.toFixed(0)}%{' '}
        <span className="text-ink-3">(was {row.currentRamPct.toFixed(0)}%)</span>
      </span>
    </div>
  )
}

// pct + severityOf live in @/lib/nodeImpact so react-refresh stays
// happy (component files should export components only). Callers
// importing helpers should pull them from there.
