import { BrowserRouter, Route, Routes } from 'react-router-dom'
import { AuthProvider } from '@/context/AuthContext'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAuth from '@/components/RequireAuth'
import RequireAdminClaim from '@/components/RequireAdminClaim'
import Dashboard from '@/pages/Dashboard'
import Settings from '@/pages/Settings'
import Claim from '@/pages/admin/Claim'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'
import OAuthCallback from '@/pages/auth/OAuthCallback'

export default function App() {
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
                  {/* First-run admin claim page — inside auth, outside admin-claim gate */}
                  <Route path="/claim" element={<Claim />} />

                  {/* Normal app — only accessible once admin is claimed */}
                  <Route
                    path="/*"
                    element={
                      <RequireAdminClaim>
                        <Layout>
                          <Routes>
                            <Route path="/" element={<Dashboard />} />
                            <Route path="/settings" element={<Settings />} />
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
