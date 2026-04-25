import { ButtonHTMLAttributes, ReactNode } from 'react'

type Variant = 'primary' | 'ghost' | 'danger'
type Size = 'default' | 'large' | 'small'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  children: ReactNode
}

const VARIANTS: Record<Variant, string> = {
  primary: 'bg-ink text-white hover:-translate-y-px hover:shadow-btn-primary border-transparent',
  ghost:
    'bg-white/85 border-line-2 text-ink hover:bg-white hover:border-[rgba(20,18,28,0.2)]',
  danger:
    'bg-[rgba(184,58,58,0.08)] border-[rgba(184,58,58,0.25)] text-bad hover:bg-[rgba(184,58,58,0.15)]',
}

const SIZES: Record<Size, string> = {
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
  const base =
    'inline-flex items-center justify-center gap-2 rounded-[10px] font-sans font-medium border cursor-pointer transition-all duration-150 ease-out leading-none disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:transform-none'
  return (
    <button
      className={`${base} ${VARIANTS[variant]} ${SIZES[size]} ${className}`}
      {...props}
    >
      {children}
    </button>
  )
}
