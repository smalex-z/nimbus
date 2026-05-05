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
import ApiDocs from '@/pages/ApiDocs'
import Authentication from '@/pages/Authentication'
import Email from '@/pages/Email'
import InfrastructureLayout from '@/components/InfrastructureLayout'
import GopherTunnels from '@/pages/GopherTunnels'
import GPUPlane from '@/pages/GPUPlane'
import Keys from '@/pages/Keys'
import Network from '@/pages/Network'
import Nodes from '@/pages/Nodes'
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
                      <Route path="/quotas" element={<RequireAdmin><Quotas /></RequireAdmin>} />
                      <Route path="/account" element={<Account />} />
                      {/* /authentication owns user table + sign-in providers
                          + access code + passwordless toggle. */}
                      <Route path="/authentication" element={<RequireAdmin><Authentication /></RequireAdmin>} />
                      {/* /nodes is the cluster lifecycle surface — admin-
                          only. Lock state (cordon/drain/drained), tag
                          editing, drain orchestration, remove-from-cluster
                          all live here. */}
                      <Route path="/nodes" element={<RequireAdmin><Nodes /></RequireAdmin>} />
                      {/* /s3 and /gpu are full operational pages reached
                          from the dropdown — they used to live under
                          /infrastructure/* but the page bodies don't
                          benefit from the sidebar so they sit at the top
                          level alongside other primary surfaces. /gpu
                          unifies the old /gpu (jobs/inference) and
                          /infrastructure/gpu-hosts (pairing) into one
                          stacked page (admin sees both halves). */}
                      <Route path="/s3" element={<RequireAdmin><S3 /></RequireAdmin>} />
                      <Route path="/gpu" element={<GPUPlane />} />
                      {/* /infrastructure hosts the backend-services
                          sidebar (Email, Gopher Tunnels, VM network).
                          S3 and GPU used to live here too but were
                          promoted to top-level routes per the dropdown
                          IA. Old standalone routes — /email, /gophers,
                          /network — redirect to their /infrastructure/*
                          counterparts so bookmarks still resolve. */}
                      <Route path="/infrastructure" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/infrastructure/email" element={<RequireAdmin><InfrastructureLayout><Email /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/gopher" element={<RequireAdmin><InfrastructureLayout><GopherTunnels /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/network" element={<RequireAdmin><InfrastructureLayout><Network /></InfrastructureLayout></RequireAdmin>} />
                      <Route path="/infrastructure/api-docs" element={<RequireAdmin><InfrastructureLayout><ApiDocs /></InfrastructureLayout></RequireAdmin>} />
                      {/* Bookmark redirects. /users + /settings/sign-in
                          → /authentication. /settings/* was the prior
                          name of /infrastructure/*. /infrastructure/s3
                          and /infrastructure/gpu-hosts moved out to
                          /s3 and /gpu respectively. */}
                      <Route path="/email" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/gophers" element={<Navigate to="/infrastructure/gopher" replace />} />
                      <Route path="/network" element={<Navigate to="/infrastructure/network" replace />} />
                      <Route path="/gpu-host" element={<Navigate to="/gpu" replace />} />
                      <Route path="/users" element={<Navigate to="/authentication" replace />} />
                      <Route path="/settings" element={<Navigate to="/infrastructure" replace />} />
                      <Route path="/settings/sign-in" element={<Navigate to="/authentication" replace />} />
                      <Route path="/settings/email" element={<Navigate to="/infrastructure/email" replace />} />
                      <Route path="/settings/gopher" element={<Navigate to="/infrastructure/gopher" replace />} />
                      <Route path="/settings/network" element={<Navigate to="/infrastructure/network" replace />} />
                      <Route path="/settings/s3" element={<Navigate to="/s3" replace />} />
                      <Route path="/settings/gpu-hosts" element={<Navigate to="/gpu" replace />} />
                      <Route path="/infrastructure/s3" element={<Navigate to="/s3" replace />} />
                      <Route path="/infrastructure/gpu-hosts" element={<Navigate to="/gpu" replace />} />
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
