import { useEffect, useState, ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import api from '@/api/client'

export default function RequireAuth({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const [checking, setChecking] = useState(true)

  useEffect(() => {
    api.get('/me')
      .then(() => setChecking(false))
      .catch(() => navigate('/login', { replace: true }))
  }, [navigate])

  if (checking) return null

  return <>{children}</>
}
