import { useState } from 'react'

interface KeyFileUploadProps {
  maxBytes: number
  sizeError: string
  onLoad: (text: string) => void
  /**
   * Inspect the file's text content and return an error string to reject the
   * upload, or null to accept. SSH keys are usually extensionless
   * (`id_ed25519`, `id_rsa`, `authorized_keys`), so the file picker doesn't
   * filter by extension — any real "wrong file" check has to happen on the
   * actual bytes here.
   */
  validate?: (text: string) => string | null
  /** Visible label under the icon. Defaults to "Browse". */
  buttonLabel?: string
}

/**
 * Vertical "Browse" affordance designed to sit on the right side of a
 * textarea. Stretches to match the height of its flex parent. Icon at the
 * top, label below; on success the filename appears beneath the button so
 * the user can see the file took.
 */
export default function KeyFileUpload({
  maxBytes,
  sizeError,
  onLoad,
  validate,
  buttonLabel = 'Browse',
}: KeyFileUploadProps) {
  const [fileName, setFileName] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const handleFile = async (file: File) => {
    setError(null)
    setFileName(null)
    if (file.size > maxBytes) {
      setError(sizeError)
      return
    }
    let text: string
    try {
      text = (await file.text()).trim()
    } catch {
      setError('Could not read file.')
      return
    }
    if (!text) {
      setError('File is empty.')
      return
    }
    // Reject obvious binaries — SSH keys are always text. A NUL byte in the
    // payload is a strong signal we're looking at a binary (DER key, image,
    // archive, etc.).
    if (text.includes('\0')) {
      setError("That doesn't look like a text file.")
      return
    }
    if (validate) {
      const reason = validate(text)
      if (reason) {
        setError(reason)
        return
      }
    }
    onLoad(text)
    setFileName(file.name)
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
