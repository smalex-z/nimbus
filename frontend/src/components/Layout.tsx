import { ReactNode } from 'react'
import { Link, useLocation } from 'react-router-dom'
import { NimbusBrand } from './nimbus'

interface LayoutProps {
  children: ReactNode
}

const NAV_ITEMS = [
  { label: 'Dashboard', path: '/' },
  { label: 'Settings', path: '/settings' },
]

export default function Layout({ children }: LayoutProps) {
  const location = useLocation()

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        flexDirection: 'column',
        background: 'linear-gradient(180deg, var(--bg-top) 0%, var(--bg-bot) 100%)',
      }}
    >
      <nav
        style={{
          background: 'rgba(252, 251, 250, 0.85)',
          backdropFilter: 'blur(12px)',
          WebkitBackdropFilter: 'blur(12px)',
          borderBottom: '1px solid var(--hairline)',
          position: 'sticky',
          top: 0,
          zIndex: 50,
        }}
      >
        <div
          style={{
            maxWidth: 1100,
            margin: '0 auto',
            padding: '0 24px',
            height: 54,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <Link to="/" style={{ textDecoration: 'none' }}>
            <NimbusBrand size="sm" />
          </Link>
          <div style={{ display: 'flex', gap: 2 }}>
            {NAV_ITEMS.map((item) => {
              const active = location.pathname === item.path
              return (
                <Link
                  key={item.path}
                  to={item.path}
                  style={{
                    padding: '6px 12px',
                    borderRadius: 8,
                    fontSize: 13,
                    fontWeight: 500,
                    fontFamily: 'var(--font-sans)',
                    textDecoration: 'none',
                    transition: 'background 0.15s, color 0.15s',
                    background: active ? 'rgba(20,18,28,0.06)' : 'transparent',
                    color: active ? 'var(--ink)' : 'var(--ink-mute)',
                  }}
                >
                  {item.label}
                </Link>
              )
            })}
          </div>
        </div>
      </nav>

      <main
        style={{
          flex: 1,
          maxWidth: 1100,
          margin: '0 auto',
          width: '100%',
          padding: '32px 24px',
        }}
      >
        {children}
      </main>

      <footer
        style={{
          borderTop: '1px solid var(--hairline)',
          padding: '14px 24px',
          textAlign: 'center',
          fontSize: 11,
          color: 'var(--ink-mute)',
          fontFamily: 'var(--font-mono)',
          letterSpacing: '0.02em',
        }}
      >
        nimbus · self-hosted vm provisioning
      </footer>
    </div>
  )
}
