import { useEffect, useState } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router-dom'
import { AuthProvider } from '@/context/AuthContext'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAuth from '@/components/RequireAuth'
import Admin from '@/pages/Admin'
import Keys from '@/pages/Keys'
import Provision from '@/pages/Provision'
import MyVMs from '@/pages/MyVMs'
import Nodes from '@/pages/Nodes'
import Settings from '@/pages/Settings'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'
import OAuthCallback from '@/pages/auth/OAuthCallback'
import Setup from '@/pages/Setup'
import { getSetupStatus } from '@/api/client'

type AppState = 'loading' | 'setup' | 'ready'

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
            path="/*"
            element={
              <RequireAuth>
                <Layout>
                  <Routes>
                    <Route path="/" element={<Provision />} />
                    <Route path="/vms" element={<MyVMs />} />
                    <Route path="/keys" element={<Keys />} />
                    <Route path="/nodes" element={<Nodes />} />
                    <Route path="/settings" element={<Settings />} />
                    <Route path="/admin" element={<Admin />} />
                  </Routes>
                </Layout>
              </RequireAuth>
            }
          />
        </Routes>
      </AuthProvider>
    </BrowserRouter>
  )
}
