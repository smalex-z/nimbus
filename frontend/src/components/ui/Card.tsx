import { ReactNode } from 'react'

interface CardProps {
  title?: string
  children: ReactNode
  className?: string
}

export default function Card({ title, children, className = '' }: CardProps) {
  return (
    <div className={`n-card ${className}`}>
      {title && (
        <div
          style={{
            padding: '16px 24px',
            borderBottom: '1px solid var(--hairline)',
          }}
        >
          <h3
            style={{
              margin: 0,
              fontSize: 14,
              fontWeight: 600,
              color: 'var(--ink)',
              fontFamily: 'var(--font-sans)',
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
