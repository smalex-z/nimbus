import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider } from '@/context/AuthContext'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAdmin from '@/components/RequireAdmin'
import RequireAuth from '@/components/RequireAuth'
import RequireVerified from '@/components/RequireVerified'
import Account from '@/pages/Account'
import Admin from '@/pages/Admin'
import Authentication from '@/pages/Authentication'
import Email from '@/pages/Email'
import InfrastructureLayout from '@/components/InfrastructureLayout'
import GopherTunnels from '@/pages/GopherTunnels'
import GPU from '@/pages/GPU'
import GPUHost from '@/pages/GPUHost'
import Keys from '@/pages/Keys'
import Network from '@/pages/Network'
import Provision from '@/pages/Provision'
import MyVMs from '@/pages/MyVMs'
import Quotas from '@/pages/Quotas'
import S3 from '@/pages/S3'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'
import OAuthCallback from '@/pages/auth/OAuthCallback'
import Setup from '@/pages/Setup'
import Verify from '@/pages/Verify'
import { useAuth } from '@/hooks/useAuth'
import { getSetupStatus } from '@/api/client'

type AppState = 'loading' | 'setup' | 'ready'

// VerifyGate keeps verified users (and admins) out of the verify page —
// they have nothing to do there.
function VerifyGate() {
  const { user, verified, loading } = useAuth()
  if (loading) return null
  if (!user) return <Navigate to="/login" replace />
  if (user.is_admin || verified) return <Navigate to="/" replace />
  return <Verify />
}

export default function App() {
  const [state, setState] = useState<AppState>('loading')

  useEffect(() => {
    getSetupStatus()
      .then((s) => {
        setState(!s.configured || s.needs_admin_setup ? 'setup' : 'ready')
      })
      .catch(() => setState('setup'))
  }, [])

  if (state === 'loading') {
    return (
      <div className="min-h-screen grid place-items-center">
        <Background />
        <div className="brand-mark brand-mark-lg animate-pulse" />
      </div>
    )
  }

  if (state === 'setup') {
    return <Setup />
  }

  return (
    <BrowserRouter>
      <AuthProvider>
        <Background />
        <Routes>
          <Route path="/login" element={<SignIn />} />
          <Route path="/signup" element={<SignUp />} />
          <Route path="/auth/callback" element={<OAuthCallback />} />
          <Route
            path="/verify"
            element={
              <RequireAuth>
                <VerifyGate />
              </RequireAuth>
            }
          />
          <Route
            path="/*"
            element={
              <RequireAuth>
                <RequireVerified>
                  <Layout>
                    <Routes>
                      <Route path="/" element={<Provision />} />
                      <Route path="/vms" element={<MyVMs />} />
                      <Route path="/keys" element={<Keys />} />
                      <Route path="/gpu" element={<GPU />} />
                      <Route path="/quotas" element={<RequireAdmin><Quotas /></RequireAdmin>} />
                      <Route path="/account" element={<Account />} />
                      {/* /authentication is the top-level admin page that
                          owns the user table + sign-in providers + access
                          code + passwordless toggle. */}
                      <Route path="/authentication" element={<RequireAdmin><Authentication /></RequireAdmin>} />
                      {/* /infrastructure hosts the cluster + backend-services
                          sidebar (Email, Gopher Tunnels, VM network, S3,
                          GPU hosts). Old standalone routes — /email,
                          /gophers, /network, /s3, /gpu-host — redirect to
                          their /infrastructure/* counterparts so existing
                          bookmarks still resolve. */}
                      <Route path="/infrastructure" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/infrastructure/email" element={<RequireAdmin><InfrastructureLayout><Email /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/gopher" element={<RequireAdmin><InfrastructureLayout><GopherTunnels /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/network" element={<RequireAdmin><InfrastructureLayout><Network /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/s3" element={<RequireAdmin><InfrastructureLayout><S3 /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/gpu-hosts" element={<RequireAdmin><InfrastructureLayout><GPUHost /></InfrastructureLayout></RequireAdmin>} />
                      {/* Bookmark redirects. /users + /settings/sign-in
                          both used to host the user table; both now land
                          on /authentication. /settings/* was the prior
                          name of /infrastructure/*. */}
                      <Route path="/email" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/gophers" element={<Navigate to="/infrastructure/gopher" replace />} />
                      <Route path="/network" element={<Navigate to="/infrastructure/network" replace />} />
                      <Route path="/s3" element={<Navigate to="/infrastructure/s3" replace />} />
                      <Route path="/gpu-host" element={<Navigate to="/infrastructure/gpu-hosts" replace />} />
                      <Route path="/users" element={<Navigate to="/authentication" replace />} />
                      <Route path="/settings" element={<Navigate to="/infrastructure" replace />} />
                      <Route path="/settings/sign-in" element={<Navigate to="/authentication" replace />} />
                      <Route path="/settings/email" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/settings/gopher" element={<Navigate to="/infrastructure/gopher" replace />} />
                      <Route path="/settings/network" element={<Navigate to="/infrastructure/network" replace />} />
                      <Route path="/settings/s3" element={<Navigate to="/infrastructure/s3" replace />} />
                      <Route path="/settings/gpu-hosts" element={<Navigate to="/infrastructure/gpu-hosts" replace />} />
                      <Route path="/admin" element={<RequireAdmin><Admin /></RequireAdmin>} />
                    </Routes>
                  </Layout>
                </RequireVerified>
              </RequireAuth>
            }
          />
        </Routes>
      </AuthProvider>
    </BrowserRouter>
  )
}
