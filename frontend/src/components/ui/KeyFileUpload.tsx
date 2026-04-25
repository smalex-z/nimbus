import { useState } from 'react'

interface KeyFileUploadProps {
  accept: string
  buttonLabel: string
  maxBytes: number
  sizeError: string
  onLoad: (text: string) => void
}

export default function KeyFileUpload({
  accept,
  buttonLabel,
  maxBytes,
  sizeError,
  onLoad,
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
    <div className="flex items-center gap-3">
      <label className="inline-flex items-center gap-2 px-3 py-1.5 rounded-[8px] border border-line-2 bg-white/85 text-[12px] text-ink cursor-pointer hover:border-ink transition-colors">
        <span>{buttonLabel}</span>
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
        <span className="text-xs text-ink-3 font-mono truncate">Loaded {fileName}</span>
      )}
      {error && <span className="text-xs text-bad">{error}</span>}
    </div>
  )
}
