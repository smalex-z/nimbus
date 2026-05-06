import { ReactNode } from 'react'
import { Link, NavLink, useLocation } from 'react-router-dom'
import nimbusLogo from '@/assets/Nimbus_Logo.png'
import api from '@/api/client'
import { useAuth } from '@/hooks/useAuth'
import NavDropdown from '@/components/ui/NavDropdown'

interface LayoutProps {
  children: ReactNode
  showNav?: boolean
}

const navLinkClass = ({ isActive }: { isActive: boolean }) =>
  `px-3.5 py-2 rounded-[8px] text-sm font-medium transition-colors no-underline ${
    isActive
      ? 'bg-[rgba(27,23,38,0.08)] text-ink'
      : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
  }`

// Dropdown items: 13px, icon on the left, full row tappable. Active
// state mirrors the top-level nav so the user sees where they are.
const dropdownItemClass = ({ isActive }: { isActive: boolean }) =>
  `flex items-center gap-2.5 w-full px-3 py-1.5 rounded-md text-[13px] no-underline transition-colors text-left cursor-pointer ${
    isActive
      ? 'bg-[rgba(27,23,38,0.08)] text-ink'
      : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
  }`

export default function Layout({ children, showNav = true }: LayoutProps) {
  const { user } = useAuth()
  const location = useLocation()

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
              {/* Top-level nav stays minimal: high-frequency operational
                  surfaces only. Everything else (Authentication,
                  Infrastructure, S3, GPU, Keys, Account) lives in the
                  dropdown so the bar doesn't sprawl as more capabilities
                  land. Dashboard + Quotas are admin-only; Provision +
                  My machines are universal. */}
              {user?.is_admin && (
                <NavLink to="/admin" className={navLinkClass}>
                  Dashboard
                </NavLink>
              )}
              <NavLink to="/" end className={navLinkClass}>
                Provision
              </NavLink>
              <NavLink to="/vms" className={navLinkClass}>
                My machines
              </NavLink>
              {/* Nodes is the cluster-lifecycle surface — promoted to
                  the top bar (it's where the Proxmox-binding chip used
                  to live; the chip itself moved into the Nodes page
                  header where the rest of the cluster context already
                  lives). */}
              {user?.is_admin && (
                <NavLink to="/nodes" className={navLinkClass}>
                  Nodes
                </NavLink>
              )}
              {user?.is_admin && (
                <NavLink to="/quotas" className={navLinkClass}>
                  Quotas
                </NavLink>
              )}

              {user && <div className="w-px h-4 bg-[rgba(20,18,28,0.1)] mx-1.5" />}

              {user?.is_admin ? (
                <NavDropdown
                  placement="bottom-end"
                  triggerClassName="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors flex items-center gap-1.5 cursor-pointer"
                  trigger={
                    <>
                      <span className="font-mono">{user.name}</span>
                      <span className="text-[10px] font-semibold tracking-wider uppercase font-sans text-[#9a5c2e] bg-[rgba(248,175,130,0.15)] border border-[rgba(248,175,130,0.4)] px-1.5 py-px rounded">
                        admin
                      </span>
                      <span className="text-xl text-ink-2 leading-none ml-0.5" aria-hidden="true">▾</span>
                    </>
                  }
                >
                  {/* Two-section dropdown: Workspace (admin policy +
                      cluster surfaces) and Account (personal). Section
                      labels make the grouping explicit at a glance. */}
                  <SectionLabel>Workspace</SectionLabel>
                  <NavLink to="/authentication" className={dropdownItemClass}>
                    <ShieldIcon /><span>Authentication</span>
                  </NavLink>
                  <NavLink to="/infrastructure" className={dropdownItemClass}>
                    <ServerIcon /><span>Infrastructure</span>
                  </NavLink>
                  <NavLink to="/s3" className={dropdownItemClass}>
                    <DatabaseIcon /><span>S3</span>
                  </NavLink>
                  <NavLink to="/gpu" className={dropdownItemClass}>
                    <CpuIcon /><span>GPU</span><AlphaPill />
                  </NavLink>

                  <div className="my-1 border-t border-line" />

                  <SectionLabel>Account</SectionLabel>
                  <NavLink to="/keys" className={dropdownItemClass}>
                    <KeyIcon /><span>Keys</span>
                  </NavLink>
                  <NavLink to="/buckets" className={dropdownItemClass}>
                    <DatabaseIcon /><span>Buckets</span>
                  </NavLink>
                  <NavLink to="/account" className={dropdownItemClass}>
                    <UserIcon /><span>Account</span>
                  </NavLink>

                  <div className="my-1 border-t border-line" />

                  <SignOutButton onClick={handleSignOut} />
                </NavDropdown>
              ) : user ? (
                <NavDropdown
                  placement="bottom-end"
                  triggerClassName="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors flex items-center gap-1.5 cursor-pointer"
                  trigger={
                    <>
                      <span className="font-mono">{user.name}</span>
                      <span className="text-xl text-ink-2 leading-none ml-0.5" aria-hidden="true">▾</span>
                    </>
                  }
                >
                  <SectionLabel>Account</SectionLabel>
                  <NavLink to="/keys" className={dropdownItemClass}>
                    <KeyIcon /><span>Keys</span>
                  </NavLink>
                  <NavLink to="/buckets" className={dropdownItemClass}>
                    <DatabaseIcon /><span>Buckets</span>
                  </NavLink>
                  <NavLink to="/account" className={dropdownItemClass}>
                    <UserIcon /><span>Account</span>
                  </NavLink>
                  <div className="my-1 border-t border-line" />
                  <SignOutButton onClick={handleSignOut} />
                </NavDropdown>
              ) : null}
            </div>
          </div>
        </nav>
      )}

      {/* Wide-page list. /infrastructure/* has a 220px sidebar next to
          its content; /authentication runs a users table next to a
          360px policy rail; /nodes packs a 4-up card grid + a dense
          management table. All three benefit from the extra horizontal
          breathing room over the 1200px default. */}
      <main
        className={`flex-1 mx-auto w-full px-8 py-12 pb-20 animate-fadeIn ${
          location.pathname.startsWith('/infrastructure')
            || location.pathname.startsWith('/authentication')
            || location.pathname.startsWith('/nodes')
            ? 'max-w-[1440px]'
            : 'max-w-[1200px]'
        }`}
      >
        {children}
      </main>
    </div>
  )
}

// SectionLabel — small uppercase header inside the dropdown. Groups
// items into "Workspace" (admin) and "Account" (personal) so the
// visual structure mirrors the conceptual one.
function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div
      className="px-3 pt-2 pb-1 text-[10px] font-mono uppercase tracking-widest"
      style={{ color: 'var(--ink-mute)' }}
    >
      {children}
    </div>
  )
}

// SignOutButton — destructive variant of a dropdown item. Uses the
// error colour family so it visually separates from the navigation
// items above it.
function SignOutButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex items-center gap-2.5 w-full px-3 py-1.5 rounded-md text-[13px] text-left cursor-pointer transition-colors hover:bg-[rgba(184,55,55,0.08)]"
      style={{ color: 'var(--err)' }}
    >
      <SignOutIcon /><span>Sign out</span>
    </button>
  )
}

// AlphaPill — small uppercase chip for surfaces that aren't yet stable.
function AlphaPill() {
  return (
    <span className="font-mono text-[9px] uppercase tracking-widest text-warn bg-[rgba(184,101,15,0.12)] border border-[rgba(184,101,15,0.25)] px-1.5 py-px rounded ml-auto">
      Alpha
    </span>
  )
}

// --- icons -------------------------------------------------------
// 16px, 1.6px stroke, currentColor. Single source so the dropdown's
// visual rhythm stays consistent without pulling in a UI library.

const iconProps = {
  width: 16,
  height: 16,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.6,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
}

function ShieldIcon() {
  return (
    <svg {...iconProps}>
      <path d="M12 3l8 3v5c0 5-3.5 8.5-8 10-4.5-1.5-8-5-8-10V6l8-3z" />
      <path d="M9 12l2 2 4-4" />
    </svg>
  )
}

function ServerIcon() {
  return (
    <svg {...iconProps}>
      <rect x="3" y="4" width="18" height="7" rx="1.5" />
      <rect x="3" y="13" width="18" height="7" rx="1.5" />
      <line x1="7" y1="7.5" x2="7.01" y2="7.5" />
      <line x1="7" y1="16.5" x2="7.01" y2="16.5" />
    </svg>
  )
}

function DatabaseIcon() {
  return (
    <svg {...iconProps}>
      <ellipse cx="12" cy="5" rx="8" ry="2.5" />
      <path d="M4 5v6c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5V5" />
      <path d="M4 11v6c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5v-6" />
    </svg>
  )
}

function CpuIcon() {
  return (
    <svg {...iconProps}>
      <rect x="5" y="5" width="14" height="14" rx="1.5" />
      <rect x="9" y="9" width="6" height="6" />
      <line x1="9" y1="2" x2="9" y2="5" />
      <line x1="15" y1="2" x2="15" y2="5" />
      <line x1="9" y1="19" x2="9" y2="22" />
      <line x1="15" y1="19" x2="15" y2="22" />
      <line x1="2" y1="9" x2="5" y2="9" />
      <line x1="2" y1="15" x2="5" y2="15" />
      <line x1="19" y1="9" x2="22" y2="9" />
      <line x1="19" y1="15" x2="22" y2="15" />
    </svg>
  )
}

function KeyIcon() {
  return (
    <svg {...iconProps}>
      <circle cx="8" cy="15" r="3.5" />
      <path d="M10.5 12.5L20 3" />
      <path d="M16 7l3 3" />
      <path d="M18 5l2 2" />
    </svg>
  )
}

function UserIcon() {
  return (
    <svg {...iconProps}>
      <circle cx="12" cy="8" r="3.5" />
      <path d="M5 20c0-3.5 3-6 7-6s7 2.5 7 6" />
    </svg>
  )
}

function SignOutIcon() {
  return (
    <svg {...iconProps}>
      <path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4" />
      <path d="M16 17l5-5-5-5" />
      <line x1="21" y1="12" x2="9" y2="12" />
    </svg>
  )
}
