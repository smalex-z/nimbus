import { ReactNode } from 'react'

interface CardProps {
  title?: string
  children: ReactNode
  strong?: boolean
  className?: string
}

export default function Card({ title, children, strong = false, className = '' }: CardProps) {
  const glassClass = strong ? 'glass glass-strong' : 'glass'
  return (
    <div className={`${glassClass} ${className}`}>
      {title && (
        <div
          style={{
            padding: '14px 24px',
            borderBottom: '1px solid rgba(20, 18, 28, 0.07)',
          }}
        >
          <h3
            style={{
              margin: 0,
              fontSize: 13,
              fontWeight: 600,
              color: 'var(--ink)',
              letterSpacing: '-0.005em',
            }}
          >
            {title}
          </h3>
        </div>
      )}
      <div style={{ padding: '20px 24px' }}>{children}</div>
    </div>
  )
}
