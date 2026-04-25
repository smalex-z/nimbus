import { ReactNode } from 'react'

interface RadioCardProps {
  title: string
  description: ReactNode
  selected: boolean
  onClick: () => void
}

// RadioCard is a card-styled radio entry — used for "Bring your own key" /
// "Generate one for me" in the Provision form. Only one in a group should be
// `selected` at a time; the parent component manages mutex state.
export default function RadioCard({ title, description, selected, onClick }: RadioCardProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`flex items-start gap-3 p-3.5 rounded-[10px] border transition-all text-left w-full cursor-pointer ${
        selected
          ? 'border-ink bg-white'
          : 'border-line-2 bg-white/70 hover:bg-white'
      }`}
    >
      <span
        className={`w-4 h-4 rounded-full border-[1.5px] mt-0.5 flex-shrink-0 relative ${
          selected ? 'border-ink' : 'border-ink-3'
        }`}
      >
        {selected && (
          <span className="absolute inset-[3px] rounded-full bg-ink" />
        )}
      </span>
      <span>
        <span className="block text-sm font-medium">{title}</span>
        <span className="block text-xs text-ink-3 mt-0.5 leading-relaxed">{description}</span>
      </span>
    </button>
  )
}
