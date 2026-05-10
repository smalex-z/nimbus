// Helpers shared by every consumer of NodeImpactPanel (single-VM
// migrate, drain plan, multi-VM migrate). Extracted from the panel
// component itself so react-refresh can hot-update the component
// cleanly — exporting non-component values from a component file
// breaks fast refresh.

export function pct(used: number, total: number): number {
  if (total <= 0) return 0
  return (used / total) * 100
}

export function severityOf(ramPct: number): 'ok' | 'caution' | 'high' {
  if (ramPct > 85) return 'high'
  if (ramPct > 60) return 'caution'
  return 'ok'
}
