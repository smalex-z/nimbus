import { useEffect, ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '@/context/AuthContext'

export default function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  const navigate = useNavigate()

  useEffect(() => {
    if (!loading && !user) {
      navigate('/login', { replace: true })
    }
  }, [user, loading, navigate])

  if (loading || !user) return null

  return <>{children}</>
}
