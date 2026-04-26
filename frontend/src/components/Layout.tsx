import { ReactNode, useEffect, useState } from 'react'
import { Link, NavLink, useLocation } from 'react-router-dom'
import nimbusLogo from '@/assets/Nimbus_Logo.png'
import api, { getGPUInference, getS3Storage } from '@/api/client'
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

// Smaller, indented items shown under the Control Panel section header.
const controlPanelItemClass = ({ isActive }: { isActive: boolean }) =>
  `block w-full pl-5 pr-3 py-1 text-xs no-underline transition-colors text-left cursor-pointer ${
    isActive
      ? 'bg-[rgba(27,23,38,0.08)] text-ink'
      : 'text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink'
  }`

export default function Layout({ children, showNav = true }: LayoutProps) {
  const { user } = useAuth()
  const location = useLocation()
  // Whether an S3 storage row exists (any status). Promotes the S3 link
  // from the Control Panel dropdown to a top-level navbar item — the
  // page is only useful day-to-day after the storage VM is deployed.
  // Refetched on route change so the navbar updates as soon as the user
  // navigates away from /s3 post-deploy or post-delete.
  const [s3Deployed, setS3Deployed] = useState(false)

  useEffect(() => {
    if (!user?.is_admin) {
      setS3Deployed(false)
      return
    }
    let cancelled = false
    getS3Storage()
      .then((row) => {
        if (!cancelled) setS3Deployed(row !== null)
      })
      .catch(() => {
        // Network blip or transient 500 — leave the navbar where it
        // was. Errors are not user-actionable from the navbar.
      })
    return () => {
      cancelled = true
    }
  }, [user?.is_admin, location.pathname])

  // gpuPlaneEnabled gates the top-level "GPU" tab. We only render it once
  // an admin has paired a GX10 — pre-pairing the tab would lead users to a
  // page that has nothing to show. Polled lazily so a fresh pairing is
  // reflected in the nav within a minute without a page refresh.
  const [gpuPlaneEnabled, setGpuPlaneEnabled] = useState(false)
  useEffect(() => {
    if (!user) return
    let cancelled = false
    const tick = () => {
      getGPUInference()
        .then((s) => { if (!cancelled) setGpuPlaneEnabled(s.enabled) })
        .catch(() => { /* keep last known state on error */ })
    }
    tick()
    const id = setInterval(tick, 60_000)
    return () => { cancelled = true; clearInterval(id) }
  }, [user])

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
              {user?.is_admin && s3Deployed && (
                <NavLink to="/s3" className={navLinkClass}>
                  S3
                </NavLink>
              )}
              {gpuPlaneEnabled && (
                <NavLink to="/gpu" className={navLinkClass}>
                  GPU
                </NavLink>
              )}
              {!user?.is_admin && (
                <NavLink to="/keys" className={navLinkClass}>
                  Keys
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
                  <div className="px-3 pt-2 pb-1 text-[11px] uppercase tracking-wider text-ink-3 font-semibold">
                    Control Panel
                  </div>
                  <NavLink to="/settings" className={controlPanelItemClass} style={{ cursor: 'pointer' }}>
                    Authentication
                  </NavLink>
                  <NavLink to="/gophers" className={controlPanelItemClass} style={{ cursor: 'pointer' }}>
                    Gopher Tunnels
                  </NavLink>
                  {/* When S3 is deployed the link is promoted to a top-level
                      navbar item; hiding the dropdown entry avoids duplication. */}
                  {!s3Deployed && (
                    <NavLink to="/s3" className={controlPanelItemClass} style={{ cursor: 'pointer' }}>
                      S3 Storage
                    </NavLink>
                  )}
                  <NavLink to="/gpu-host" className={controlPanelItemClass} style={{ cursor: 'pointer' }}>
                    GX10 GPU
                  </NavLink>
                  <NavLink to="/keys" className={controlPanelItemClass} style={{ cursor: 'pointer' }}>
                    Keys
                  </NavLink>

                  <div className="my-1 border-t border-line" />

                  <button
                    type="button"
                    onClick={handleSignOut}
                    style={{ cursor: 'pointer' }}
                    className="block w-full px-3 py-1.5 text-sm text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors text-left cursor-pointer"
                  >
                    Sign out
                  </button>
                </NavDropdown>
              ) : user ? (
                <>
                  <span className="px-2.5 py-2 text-sm text-ink-2 font-mono">
                    {user.name}
                  </span>
                  <button
                    onClick={handleSignOut}
                    className="px-3.5 py-2 rounded-[8px] text-sm font-medium text-ink-2 hover:bg-[rgba(27,23,38,0.05)] hover:text-ink transition-colors"
                  >
                    Sign out
                  </button>
                </>
              ) : null}
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
