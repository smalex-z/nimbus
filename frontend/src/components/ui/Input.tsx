import { InputHTMLAttributes, forwardRef } from 'react'

interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string
  error?: string
}

const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ label, error, className = '', ...props }, ref) => {
    return (
      <div className="n-field">
        {label && <label className="n-label">{label}</label>}
        <input
          ref={ref}
          className={`n-input ${error ? 'n-input-error' : ''} ${className}`}
          {...props}
        />
        {error && <p className="n-error">{error}</p>}
      </div>
    )
  },
)

Input.displayName = 'Input'

export default Input
