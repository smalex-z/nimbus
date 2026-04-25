import type { VMStatus } from '@/types'

type NodeStatus = 'online' | 'offline' | 'unknown'
type Status = VMStatus | NodeStatus

const statusConfig: Record<Status, { text: string; dot: string; label: string }> = {
  running: { text: 'text-good', dot: 'bg-good', label: 'RUNNING' },
  online: { text: 'text-good', dot: 'bg-good', label: 'ONLINE' },
  failed: { text: 'text-bad', dot: 'bg-bad', label: 'FAILED' },
  offline: { text: 'text-bad', dot: 'bg-bad', label: 'OFFLINE' },
  provisioning: { text: 'text-warn', dot: 'bg-warn', label: 'PROVISIONING' },
  unknown: { text: 'text-ink-3', dot: 'bg-ink-3', label: 'UNKNOWN' },
}

export default function StatusBadge({ status }: { status: Status }) {
  const cfg = statusConfig[status] ?? statusConfig.unknown
  return (
    <span className={`inline-flex items-center gap-1.5 font-mono text-xs ${cfg.text}`}>
      <span className={`w-1.5 h-1.5 rounded-full ${cfg.dot}`} />
      {cfg.label}
    </span>
  )
}
