import { ReactNode } from 'react'
import { NavLink } from 'react-router-dom'

// InfrastructureLayout wraps every /infrastructure/* subroute with a
// left sidebar of category links. The sidebar is the page; the right
// column slots in the matching subpage's content (Email, Gopher Tunnels,
// VM network).
//
// Each entry routes to its own URL (e.g. /infrastructure/email) so the
// browser back button + bookmarks work the way an admin expects.
// The sidebar item highlights when its URL prefix matches —
// NavLink's isActive is exact by default; we use end={false} so the
// match also catches the index path.
//
// S3 and GPU used to live here too, but they're full operational
// surfaces (not config) — they were promoted to top-level routes (/s3,
// /gpu) reachable from the user dropdown so they don't share the
// sidebar's narrow content column.

interface InfrastructureLayoutProps {
  children: ReactNode
}

interface NavItem {
  label: string
  to: string
  badge?: 'preview' | 'alpha'
}

export default function InfrastructureLayout({ children }: InfrastructureLayoutProps) {
  const items: NavItem[] = [
    { label: 'Email', to: '/infrastructure/email', badge: 'preview' },
    { label: 'Gopher Tunnels', to: '/infrastructure/gopher' },
    { label: 'VM network', to: '/infrastructure/network' },
  ]

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 28 }}>
      <div>
        <h1 className="n-display" style={{ fontSize: 28, margin: '0 0 6px' }}>Infrastructure</h1>
        <p style={{ margin: 0, fontSize: 14, color: 'var(--ink-body)' }}>
          Backend services and cluster configuration. Pick a category on the left.
        </p>
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-[220px_1fr] gap-6 items-start">
        <nav
          className="glass"
          style={{
            padding: '14px 10px',
            display: 'flex',
            flexDirection: 'column',
            gap: 2,
            position: 'sticky',
            // The sticky navbar measures ~73px (py-5 padding + h-8 logo +
            // 1px border). 100 leaves a clean ~27px gap so the sidebar
            // doesn't appear glued to the bottom of the navbar after the
            // page scrolls into the sticky range.
            top: 100,
            alignSelf: 'start',
          }}
        >
          {items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                `block px-3 py-2 rounded-md text-[13px] no-underline transition-colors cursor-pointer ${
                  isActive
                    ? 'bg-[rgba(27,23,38,0.08)] text-ink font-medium'
                    : 'text-ink-2 hover:bg-[rgba(27,23,38,0.04)] hover:text-ink'
                }`
              }
            >
              <span className="inline-flex items-center gap-1.5">
                {item.label}
                {item.badge === 'preview' && <PreviewPill />}
                {item.badge === 'alpha' && <AlphaPill />}
              </span>
            </NavLink>
          ))}
        </nav>
        <div>{children}</div>
      </div>
    </div>
  )
}

function PreviewPill() {
  return (
    <span
      className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-px rounded"
      title="Preview — config saves but the send pipeline ships in a follow-up release"
    >
      Preview
    </span>
  )
}

function AlphaPill() {
  return (
    <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-px rounded">
      Alpha
    </span>
  )
}
