type BrandSize = 'sm' | 'md' | 'lg'

const SIZES: Record<BrandSize, { mark: number; font: number }> = {
  sm: { mark: 22, font: 15 },
  md: { mark: 28, font: 18 },
  lg: { mark: 36, font: 22 },
}

interface NimbusBrandProps {
  size?: BrandSize
  subtitle?: string
}

export function NimbusBrand({ size = 'md', subtitle }: NimbusBrandProps) {
  const s = SIZES[size]
  const r = Math.round(s.mark * 0.29)

  return (
    <div
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 10,
        fontFamily: 'var(--font-display)',
        fontWeight: 500,
        fontSize: s.font,
        color: 'var(--ink)',
        letterSpacing: '-0.01em',
        textDecoration: 'none',
      }}
    >
      <span
        style={{
          display: 'inline-block',
          width: s.mark,
          height: s.mark,
          borderRadius: r,
          flexShrink: 0,
          background: `
            radial-gradient(circle at 30% 30%, var(--blob-peach), transparent 55%),
            radial-gradient(circle at 70% 65%, var(--blob-lavender), transparent 55%),
            radial-gradient(circle at 60% 30%, var(--blob-pink), transparent 60%),
            var(--ink)
          `,
        }}
      />
      <span style={{ display: 'inline-flex', flexDirection: 'column', lineHeight: 1.1 }}>
        <span>Nimbus</span>
        {subtitle && (
          <span
            style={{
              fontFamily: 'var(--font-mono)',
              fontSize: 10,
              color: 'var(--ink-mute)',
              fontWeight: 400,
              letterSpacing: '0.05em',
              textTransform: 'uppercase',
              marginTop: 2,
            }}
          >
            {subtitle}
          </span>
        )}
      </span>
    </div>
  )
}
