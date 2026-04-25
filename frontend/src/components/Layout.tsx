import { ReactNode } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import api from '@/api/client'

interface LayoutProps {
  children: ReactNode
}

const NAV_ITEMS = [
  { label: 'Dashboard', path: '/' },
  { label: 'Settings', path: '/settings' },
]

export default function Layout({ children }: LayoutProps) {
  const location = useLocation()
  const navigate = useNavigate()

  const handleSignOut = async () => {
    try {
      await api.post('/auth/logout')
    } finally {
      navigate('/login')
    }
  }

  return (
    <div style={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      <nav
        style={{
          background: 'rgba(252, 251, 250, 0.85)',
          backdropFilter: 'blur(12px)',
          WebkitBackdropFilter: 'blur(12px)',
          borderBottom: '1px solid rgba(20, 18, 28, 0.07)',
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
          <Link to="/" style={{ textDecoration: 'none', display: 'flex', alignItems: 'center', gap: 10 }}>
            <div className="brand-mark" />
            <span
              style={{
                fontFamily: 'var(--font-display)',
                fontSize: 17,
                fontWeight: 400,
                color: 'var(--ink)',
                letterSpacing: '-0.02em',
              }}
            >
              Nimbus
            </span>
          </Link>

          <div style={{ display: 'flex', alignItems: 'center', gap: 2 }}>
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

            <div style={{ width: 1, height: 16, background: 'rgba(20,18,28,0.1)', margin: '0 6px' }} />

            <button
              onClick={handleSignOut}
              style={{
                padding: '6px 12px',
                borderRadius: 8,
                fontSize: 13,
                fontWeight: 500,
                background: 'transparent',
                border: 'none',
                color: 'var(--ink-mute)',
                cursor: 'pointer',
                transition: 'background 0.15s, color 0.15s',
              }}
              onMouseEnter={(e) => {
                e.currentTarget.style.background = 'rgba(20,18,28,0.06)'
                e.currentTarget.style.color = 'var(--ink)'
              }}
              onMouseLeave={(e) => {
                e.currentTarget.style.background = 'transparent'
                e.currentTarget.style.color = 'var(--ink-mute)'
              }}
            >
              Sign out
            </button>
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
          borderTop: '1px solid rgba(20, 18, 28, 0.07)',
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
