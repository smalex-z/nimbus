import { useEffect, ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '@/context/AuthContext'

export default function RequireAdminClaim({ children }: { children: ReactNode }) {
  const { adminClaimed } = useAuth()
  const navigate = useNavigate()

  useEffect(() => {
    if (adminClaimed === false) {
      navigate('/claim', { replace: true })
    }
  }, [adminClaimed, navigate])

  // Still loading admin status — render nothing to avoid flash
  if (adminClaimed === null || adminClaimed === false) return null

  return <>{children}</>
}
