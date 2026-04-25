import { TextareaHTMLAttributes, forwardRef } from 'react'

interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  label?: string
  hint?: string
  error?: string
  monospace?: boolean
}

const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  ({ label, hint, error, monospace = false, className = '', ...props }, ref) => {
    const fontClass = monospace ? 'font-mono text-xs' : 'font-sans text-sm'
    return (
      <div className="flex flex-col gap-2">
        {label && <label className="text-[13px] font-medium text-ink">{label}</label>}
        <textarea
          ref={ref}
          className={`w-full px-3.5 py-3 rounded-[10px] bg-white/85 ${fontClass} text-ink border border-line-2 transition-all duration-150 outline-none focus:border-ink focus:bg-white placeholder:text-ink-3 resize-y min-h-[90px] ${
            error ? 'border-bad focus:border-bad' : ''
          } ${className}`}
          {...props}
        />
        {hint && !error && <p className="text-xs text-ink-3 leading-relaxed">{hint}</p>}
        {error && <p className="text-xs text-bad">{error}</p>}
      </div>
    )
  },
)

Textarea.displayName = 'Textarea'

export default Textarea
