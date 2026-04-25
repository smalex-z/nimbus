import { ButtonHTMLAttributes, ReactNode } from 'react'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'ghost' | 'danger'
  size?: 'small' | 'default' | 'large'
  children: ReactNode
}

const VARIANTS: Record<string, string> = {
  primary: 'bg-ink text-white hover:-translate-y-px hover:shadow-btn-primary border-transparent',
  ghost: 'bg-white/85 border-[rgba(20,18,28,0.13)] text-ink hover:bg-white hover:border-[rgba(20,18,28,0.2)]',
  danger: 'bg-[rgba(184,58,58,0.08)] border-[rgba(184,58,58,0.25)] text-bad hover:bg-[rgba(184,58,58,0.15)]',
}

const SIZES: Record<string, string> = {
  small: 'px-3 py-1.5 text-xs',
  default: 'px-[22px] py-3 text-sm',
  large: 'px-7 py-4 text-[15px]',
}

export default function Button({
  variant = 'primary',
  size = 'default',
  className = '',
  children,
  ...props
}: ButtonProps) {
  return (
    <button
      className={`inline-flex items-center justify-center gap-2 rounded border font-medium transition-all duration-150 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed ${VARIANTS[variant]} ${SIZES[size]} ${className}`}
      {...props}
    >
      {children}
    </button>
  )
}
