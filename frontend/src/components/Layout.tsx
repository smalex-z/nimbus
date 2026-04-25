import { ReactNode } from 'react'
import { Link, useLocation } from 'react-router-dom'

interface LayoutProps {
  children: ReactNode
}

const navItems = [
  { label: 'Dashboard', path: '/' },
  { label: 'Settings', path: '/settings' },
]

export default function Layout({ children }: LayoutProps) {
  const location = useLocation()

  return (
    <div className="min-h-screen bg-gray-50 flex flex-col">
      {/* Navigation */}
      <nav className="bg-white border-b border-gray-200 shadow-sm">
        <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
          <div className="flex items-center justify-between h-16">
            <span className="text-xl font-bold text-brand-700">🏠 Homestack</span>
            <div className="flex space-x-4">
              {navItems.map((item) => (
                <Link
                  key={item.path}
                  to={item.path}
                  className={`px-3 py-2 rounded-md text-sm font-medium transition-colors ${
                    location.pathname === item.path
                      ? 'bg-brand-100 text-brand-700'
                      : 'text-gray-600 hover:text-brand-700 hover:bg-gray-100'
                  }`}
                >
                  {item.label}
                </Link>
              ))}
            </div>
          </div>
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 max-w-7xl mx-auto w-full px-4 sm:px-6 lg:px-8 py-8">
        {children}
      </main>

      <footer className="border-t border-gray-200 bg-white py-4">
        <p className="text-center text-sm text-gray-500">
          Powered by{' '}
          <a
            href="https://github.com/smalex-z/homestack"
            className="text-brand-600 hover:underline"
            target="_blank"
            rel="noreferrer"
          >
            Homestack
          </a>
        </p>
      </footer>
    </div>
  )
}
