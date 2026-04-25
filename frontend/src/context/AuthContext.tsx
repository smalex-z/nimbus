import { createContext, useContext, useEffect, useState, ReactNode } from 'react'
import api from '@/api/client'

interface UserView {
  id: number
  name: string
  email: string
}

interface AuthContextValue {
  user: UserView | null
  loading: boolean
  refresh: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue>({
  user: null,
  loading: true,
  refresh: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserView | null>(null)
  const [loading, setLoading] = useState(true)

  const refresh = async () => {
    try {
      const { data } = await api.get<UserView>('/me')
      setUser(data)
    } catch {
      setUser(null)
    }
  }

  useEffect(() => {
    refresh().finally(() => setLoading(false))
  }, [])

  return (
    <AuthContext.Provider value={{ user, loading, refresh }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
