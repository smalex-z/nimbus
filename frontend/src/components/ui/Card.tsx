import { HTMLAttributes, ReactNode } from 'react'

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  children: ReactNode
  strong?: boolean
}

// Card is the frosted glass surface used throughout the Nimbus UI.
export default function Card({
  children,
  strong = false,
  className = '',
  ...rest
}: CardProps) {
  const glassClass = strong ? 'glass glass-strong' : 'glass'
  return (
    <div className={`${glassClass} ${className}`} {...rest}>
      {children}
    </div>
  )
}
