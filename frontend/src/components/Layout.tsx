import { ReactNode } from 'react'
import { Link, NavLink } from 'react-router-dom'
import nimbusLogo from '@/assets/Nimbus_Logo.png'
import api from '@/api/client'
import { useAuth } from '@/hooks/useAuth'

interface LayoutProps {
  children: ReactNode
  showNav?: boolean
}

const navItems: Array<{ label: string; path: string }> = [
  { label: 'Provision', path: '/' },
  { label: 'My machines', path: '/vms' },
  { label: 'Keys', path: '/keys' },
]

// Admin-only tabs render before the regular nav so Dashboard (where admins
// land after sign-in) sits at the far left.
const adminNavItems: Array<{ label: string; path: string }> = [
  { label: 'Dashboard', path: '/admin' },
  { label: 'Authentication', path: '/settings' },
]

export default function Layout({ children, showNav = true }: LayoutProps) {
  const { user } = useAuth()

  const handleSignOut = async () => {
    try {
      await api.post('/auth/logout')
    } finally {
      window.location.replace('/login')
    }
  }

  return (
    <div className="min-h-screen flex flex-col">
      {showNav && (
        <nav
          className="sticky top-0 z-50 border-b border-line"
          style={{
            backdropFilter: 'blur(20px) saturate(140%)',
            WebkitBackdropFilter: 'blur(20px) saturate(140%)',
            background: 'rgba(255,255,255,0.75)',
          }}
        >
          <div className="max-w-[1200px] mx-auto px-8 py-5 flex items-center justify-between">
            <Link to="/" className="flex items-center cursor-pointer no-underline">
              <img src={nimbusLogo} alt="Nimbus" className="h-8 w-auto" />
            </Link>

            <div className="flex gap-1 items-center">
              {user?.is_admin && adminNavItems.map((item) => (
                <NavLink
                  key={item.path}
                  to={item.path}
                  className={({ isActive }) =>
                    `px-3.5 py-2 rounded-[8px] text-sm font-medium transition-colors no-underline ${
                      isActive
                        ? 'bg-[rgba(27,23,38,0.08)] text-ink'
                        : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}

              {navItems.map((item) => (
                <NavLink
                  key={item.path}
                  to={item.path}
                  end={item.path === '/'}
                  className={({ isActive }) =>
                    `px-3.5 py-2 rounded-[8px] text-sm font-medium transition-colors no-underline ${
                      isActive
                        ? 'bg-[rgba(27,23,38,0.08)] text-ink'
                        : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}

              <div className="w-px h-4 bg-[rgba(20,18,28,0.1)] mx-1.5" />

              {user && (
                <span className="px-2.5 py-2 text-sm text-ink-2 font-mono flex items-center gap-1.5">
                  {user.name}
                  {user.is_admin && (
                    <span className="text-[10px] font-semibold tracking-wider uppercase font-sans text-[#9a5c2e] bg-[rgba(248,175,130,0.15)] border border-[rgba(248,175,130,0.4)] px-1.5 py-px rounded">
                      admin
                    </span>
                  )}
                </span>
              )}

              <button
                onClick={handleSignOut}
                className="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors"
              >
                Sign out
              </button>
            </div>
          </div>
        </nav>
      )}

      <main className="flex-1 max-w-[1200px] mx-auto w-full px-8 py-12 pb-20 animate-fadeIn">
        {children}
      </main>
    </div>
  )
}
