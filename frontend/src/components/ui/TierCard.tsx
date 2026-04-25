import type { TierName } from '@/types'

interface TierCardProps {
  name: TierName
  cpu: number
  memMB: number
  diskGB: number
  selected: boolean
  locked?: boolean
  onClick: () => void
}

const formatMem = (mb: number) => (mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`)

export default function TierCard({
  name,
  cpu,
  memMB,
  diskGB,
  selected,
  locked = false,
  onClick,
}: TierCardProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`w-full text-left p-4 rounded-xl border transition-all duration-150 relative cursor-pointer ${
        selected
          ? 'border-ink bg-white shadow-[0_0_0_3px_rgba(20,18,28,0.06)]'
          : 'border-line-2 bg-white/70 hover:border-ink-3 hover:bg-white'
      }`}
    >
      {locked && (
        <span className="absolute top-2.5 right-3 text-[9px] font-mono bg-ink text-[#FFF8F2] px-1.5 py-0.5 rounded tracking-wider">
          APPROVAL
        </span>
      )}
      <div className="font-display text-[17px] font-semibold capitalize">{name}</div>
      <div className="font-mono text-[11px] text-ink-3 mt-1 tracking-wide">
        {cpu} vCPU · {formatMem(memMB)} RAM · {diskGB} GB
      </div>
    </button>
  )
}
