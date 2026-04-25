import { ReactNode } from 'react'

interface NimbusFooterProps {
  left?: ReactNode
  right?: ReactNode
}

export function NimbusFooter({ left, right }: NimbusFooterProps) {
  return (
    <div
      style={{
        padding: '12px 32px',
        borderTop: '1px solid var(--hairline)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        fontSize: 11,
        color: 'var(--ink-mute)',
        fontFamily: 'var(--font-mono)',
        letterSpacing: '0.02em',
      }}
    >
      <span>{left}</span>
      <span>{right}</span>
    </div>
  )
}
