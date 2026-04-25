import { HTMLAttributes, ReactNode } from 'react'

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  children: ReactNode
  strong?: boolean
}

// Card is the frosted glass surface used everywhere in the Nimbus UI. The
// `strong` variant boosts the backdrop blur — matches `.glass-strong` in the
// mockup, used for modals.
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
