import { useState } from 'react'
import { copyToClipboard } from '@/hooks/useClipboard'

interface CopyButtonProps {
  value: string
  label?: string
  className?: string
}

export default function CopyButton({ value, label = 'COPY', className = '' }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)

  const handleClick = async () => {
    const ok = await copyToClipboard(value)
    if (ok) {
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1400)
    }
  }

  return (
    <button
      type="button"
      onClick={handleClick}
      className={`bg-transparent border border-line-2 rounded-md px-2.5 py-1 cursor-pointer font-mono text-[10px] tracking-wide transition-all flex-shrink-0 ${
        copied
          ? 'bg-ink text-[#FFF8F2] border-ink'
          : 'text-ink-2 hover:bg-ink hover:text-[#FFF8F2] hover:border-ink'
      } ${className}`}
    >
      {copied ? 'COPIED' : label}
    </button>
  )
}
