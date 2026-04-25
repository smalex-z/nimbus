import { BrowserRouter, Route, Routes } from 'react-router-dom'
import Layout from '@/components/Layout'
import Dashboard from '@/pages/Dashboard'
import Settings from '@/pages/Settings'
import SignIn from '@/pages/auth/SignIn'
import SignUp from '@/pages/auth/SignUp'

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        {/* Auth pages — no nav wrapper */}
        <Route path="/login" element={<SignIn />} />
        <Route path="/signup" element={<SignUp />} />

        {/* App pages — with nav */}
        <Route
          path="/*"
          element={
            <Layout>
              <Routes>
                <Route path="/" element={<Dashboard />} />
                <Route path="/settings" element={<Settings />} />
              </Routes>
            </Layout>
          }
        />
      </Routes>
    </BrowserRouter>
  )
}
