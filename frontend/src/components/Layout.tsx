import { ReactNode } from 'react'
import { Link, NavLink } from 'react-router-dom'

interface LayoutProps {
  children: ReactNode
  showNav?: boolean
}

const navItems: Array<{ label: string; path: string }> = [
  { label: 'Provision', path: '/' },
  { label: 'My machines', path: '/vms' },
  { label: 'Nodes', path: '/nodes' },
  { label: 'Admin', path: '/admin' },
]

export default function Layout({ children, showNav = true }: LayoutProps) {
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
          <div className="max-w-[1200px] mx-auto px-8 py-4 flex items-center justify-between">
            <Link to="/" className="flex items-center gap-2.5 cursor-pointer">
              <div className="brand-mark" />
              <span className="font-display font-semibold text-xl tracking-tight">
                Nimbus
              </span>
            </Link>

            <div className="flex gap-1 items-center">
              {navItems.map((item) => (
                <NavLink
                  key={item.path}
                  to={item.path}
                  end={item.path === '/'}
                  className={({ isActive }) =>
                    `px-3.5 py-2 rounded-[8px] text-sm font-medium transition-colors ${
                      isActive
                        ? 'bg-[rgba(27,23,38,0.08)] text-ink'
                        : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}
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
