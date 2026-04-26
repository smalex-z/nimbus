import { useEffect, ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '@/hooks/useAuth'

// RequireVerified wraps the in-app routes and forwards unverified non-admin
// users to the access-code form. Admins always pass.
export default function RequireVerified({ children }: { children: ReactNode }) {
  const { user, verified, loading } = useAuth()
  const navigate = useNavigate()

  useEffect(() => {
    if (loading || !user) return
    if (user.is_admin) return
    if (!verified) navigate('/verify', { replace: true })
  }, [user, verified, loading, navigate])

  if (loading || !user) return null
  if (!user.is_admin && !verified) return null

  return <>{children}</>
}
