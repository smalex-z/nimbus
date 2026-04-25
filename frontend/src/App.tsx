import { BrowserRouter, Route, Routes } from 'react-router-dom'
import Background from '@/components/Background'
import Layout from '@/components/Layout'
import RequireAuth from '@/components/RequireAuth'
import Dashboard from '@/pages/Dashboard'
import Settings from '@/pages/Settings'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'

export default function App() {
  return (
    <BrowserRouter>
      <Background />
      <Routes>
        {/* Auth pages — no nav wrapper */}
        <Route path="/login" element={<SignIn />} />
        <Route path="/signup" element={<SignUp />} />

        {/* App pages — auth-gated, with nav */}
        <Route
          path="/*"
          element={
            <RequireAuth>
              <Layout>
                <Routes>
                  <Route path="/" element={<Dashboard />} />
                  <Route path="/settings" element={<Settings />} />
                </Routes>
              </Layout>
            </RequireAuth>
          }
        />
      </Routes>
    </BrowserRouter>
  )
}
