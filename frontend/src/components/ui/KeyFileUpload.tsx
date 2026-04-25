import { useState } from 'react'

interface KeyFileUploadProps {
  accept: string
  maxBytes: number
  sizeError: string
  onLoad: (text: string) => void
  /** Visible label under the icon. Defaults to "Browse". */
  buttonLabel?: string
}

/**
 * Vertical "Browse" affordance designed to sit on the right side of a
 * textarea. Stretches to match the height of its flex parent. Icon at the
 * top, label below; on success the filename replaces the label so the user
 * can see the file took.
 */
export default function KeyFileUpload({
  accept,
  maxBytes,
  sizeError,
  onLoad,
  buttonLabel = 'Browse',
}: KeyFileUploadProps) {
  const [fileName, setFileName] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const handleFile = async (file: File) => {
    setError(null)
    if (file.size > maxBytes) {
      setError(sizeError)
      return
    }
    try {
      const text = (await file.text()).trim()
      if (!text) {
        setError('File is empty.')
        return
      }
      onLoad(text)
      setFileName(file.name)
    } catch {
      setError('Could not read file.')
    }
  }

  return (
    <div className="flex flex-col items-stretch gap-1">
      <label
        title={fileName ?? buttonLabel}
        className="flex flex-col items-center justify-center gap-1.5 self-stretch flex-1 min-w-[84px] px-3 py-3 rounded-[10px] border border-line-2 bg-white/85 text-ink cursor-pointer hover:border-ink hover:bg-white transition-colors"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="w-5 h-5 text-ink-2"
          aria-hidden
        >
          <path d="M12 17V4" />
          <path d="m6 10 6-6 6 6" />
          <path d="M5 19h14" />
        </svg>
        <span className="text-[12px] font-medium leading-none">{buttonLabel}</span>
        <input
          type="file"
          accept={accept}
          className="hidden"
          onChange={(e) => {
            const file = e.target.files?.[0]
            if (file) handleFile(file)
            e.target.value = ''
          }}
        />
      </label>
      {fileName && !error && (
        <span className="text-[11px] text-ink-3 font-mono truncate text-center" title={fileName}>
          {fileName}
        </span>
      )}
      {error && <span className="text-[11px] text-bad text-center">{error}</span>}
    </div>
  )
}
