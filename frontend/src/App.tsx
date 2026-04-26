import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider } from '@/context/AuthContext'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAdmin from '@/components/RequireAdmin'
import RequireAuth from '@/components/RequireAuth'
import RequireVerified from '@/components/RequireVerified'
import Admin from '@/pages/Admin'
import GopherTunnels from '@/pages/GopherTunnels'
import GPU from '@/pages/GPU'
import Keys from '@/pages/Keys'
import Provision from '@/pages/Provision'
import MyVMs from '@/pages/MyVMs'
import S3 from '@/pages/S3'
import Settings from '@/pages/Settings'
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
                      <Route path="/settings" element={<RequireAdmin><Settings /></RequireAdmin>} />
                      <Route path="/gophers" element={<RequireAdmin><GopherTunnels /></RequireAdmin>} />
                      <Route path="/s3" element={<RequireAdmin><S3 /></RequireAdmin>} />
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
