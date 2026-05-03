import { ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '@/hooks/useAuth'

// RequireAdmin gates admin-only pages. Non-admins are bounced back to the
// main console root so they can never land on /admin, /authentication, or
// /infrastructure via URL.
export default function RequireAdmin({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  if (loading) return null
  if (!user) return <Navigate to="/login" replace />
  if (!user.is_admin) return <Navigate to="/" replace />
  return <>{children}</>
}
