import { ButtonHTMLAttributes, ReactNode } from 'react'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'danger'
  children: ReactNode
}

export default function Button({
  variant = 'primary',
  className = '',
  children,
  ...props
}: ButtonProps) {
  const variantClass =
    variant === 'primary'
      ? 'n-btn-primary'
      : variant === 'danger'
        ? 'n-btn-secondary'
        : 'n-btn-secondary'

  const dangerStyle =
    variant === 'danger'
      ? {
          color: 'var(--err)',
          borderColor: 'rgba(184,58,58,0.25)',
        }
      : {}

  return (
    <button
      className={`n-btn ${variantClass} ${className}`}
      style={dangerStyle}
      {...props}
    >
      {children}
    </button>
  )
}
