import { InputHTMLAttributes, ReactNode, forwardRef } from 'react'

interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'prefix'> {
  label?: string
  hint?: string
  error?: string
  prefix?: ReactNode
  suffix?: ReactNode
}

const baseInput =
  'w-full px-3.5 py-3 rounded-[10px] bg-white/85 font-sans text-sm text-ink border border-line-2 transition-all duration-150 outline-none focus:border-ink focus:bg-white placeholder:text-ink-3 disabled:opacity-60'

const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ label, hint, error, prefix, suffix, className = '', ...props }, ref) => {
    const fieldId = props.id ?? props.name
    return (
      <div className="flex flex-col gap-2">
        {label && (
          <label htmlFor={fieldId} className="text-[13px] font-medium text-ink">
            {label}
          </label>
        )}
        {prefix || suffix ? (
          <div className="flex items-stretch rounded-[10px] border border-line-2 bg-white/85 overflow-hidden">
            {prefix && (
              <div className="flex items-center px-3.5 text-ink-3 font-mono text-xs border-r border-line bg-[rgba(20,18,28,0.025)]">
                {prefix}
              </div>
            )}
            <input
              ref={ref}
              id={fieldId}
              className={`flex-1 px-3.5 py-3 bg-transparent border-0 outline-none font-sans text-sm text-ink placeholder:text-ink-3 ${className}`}
              {...props}
            />
            {suffix && (
              <div className="flex items-center px-3.5 text-ink-3 font-mono text-xs border-l border-line bg-[rgba(20,18,28,0.025)]">
                {suffix}
              </div>
            )}
          </div>
        ) : (
          <input
            ref={ref}
            id={fieldId}
            className={`${baseInput} ${
              error ? 'border-bad focus:border-bad' : ''
            } ${className}`}
            {...props}
          />
        )}
        {hint && !error && <p className="text-xs text-ink-3 leading-relaxed">{hint}</p>}
        {error && <p className="text-xs text-bad">{error}</p>}
      </div>
    )
  },
)

Input.displayName = 'Input'

export default Input
