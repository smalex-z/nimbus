import { useEffect, useState } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router-dom'
import { AuthProvider } from '@/context/AuthContext'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAuth from '@/components/RequireAuth'
import RequireAdminClaim from '@/components/RequireAdminClaim'
import Provision from '@/pages/Provision'
import MyVMs from '@/pages/MyVMs'
import Nodes from '@/pages/Nodes'
import Claim from '@/pages/admin/Claim'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'
import OAuthCallback from '@/pages/auth/OAuthCallback'
import Setup from '@/pages/Setup'
import { getSetupStatus } from '@/api/client'

type AppState = 'loading' | 'setup' | 'ready'

export default function App() {
  const [state, setState] = useState<AppState>('loading')
  const [skipToAdmin, setSkipToAdmin] = useState(false)

  useEffect(() => {
    getSetupStatus()
      .then((s) => {
        if (!s.configured || s.needs_admin_setup) {
          setSkipToAdmin(s.configured && s.needs_admin_setup)
          setState('setup')
        } else {
          setState('ready')
        }
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
    return <Setup initialStep={skipToAdmin ? 'admin' : 'proxmox'} />
  }

  return (
    <BrowserRouter>
      <AuthProvider>
        <Background />
        <Routes>
          {/* Auth pages — no nav wrapper */}
          <Route path="/login" element={<SignIn />} />
          <Route path="/signup" element={<SignUp />} />
          <Route path="/auth/callback" element={<OAuthCallback />} />

          {/* App pages — auth-gated */}
          <Route
            path="/*"
            element={
              <RequireAuth>
                <Routes>
                  {/* First-run admin claim — inside auth, outside admin-claim gate */}
                  <Route path="/claim" element={<Claim />} />

                  {/* Main app — only accessible once admin is claimed */}
                  <Route
                    path="/*"
                    element={
                      <RequireAdminClaim>
                        <Layout>
                          <Routes>
                            <Route path="/" element={<Provision />} />
                            <Route path="/vms" element={<MyVMs />} />
                            <Route path="/nodes" element={<Nodes />} />
                          </Routes>
                        </Layout>
                      </RequireAdminClaim>
                    }
                  />
                </Routes>
              </RequireAuth>
            }
          />
        </Routes>
      </AuthProvider>
    </BrowserRouter>
  )
}
