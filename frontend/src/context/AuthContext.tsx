import { createContext, useEffect, useState, ReactNode } from 'react'
import api, { getAccessCodeStatus } from '@/api/client'

interface UserView {
  id: number
  name: string
  email: string
  is_admin: boolean
}

interface AuthContextValue {
  user: UserView | null
  loading: boolean
  verified: boolean
  refresh: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue>({
  user: null,
  loading: true,
  verified: false,
  refresh: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserView | null>(null)
  const [verified, setVerified] = useState(false)
  const [loading, setLoading] = useState(true)

  const refresh = async () => {
    try {
      const { data } = await api.get<UserView>('/me')
      setUser(data)
      // Admins are always verified server-side; skip the extra request.
      if (data.is_admin) {
        setVerified(true)
      } else {
        try {
          const status = await getAccessCodeStatus()
          setVerified(status.verified)
        } catch {
          setVerified(false)
        }
      }
    } catch {
      setUser(null)
      setVerified(false)
    }
  }

  useEffect(() => {
    refresh().finally(() => setLoading(false))
  }, [])

  return (
    <AuthContext.Provider value={{ user, loading, verified, refresh }}>
      {children}
    </AuthContext.Provider>
  )
}

export default AuthContext
