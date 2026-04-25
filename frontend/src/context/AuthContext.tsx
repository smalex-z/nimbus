import { createContext, useContext, useEffect, useState, ReactNode } from 'react'
import api from '@/api/client'

interface UserView {
  id: number
  name: string
  email: string
  is_admin: boolean
}

interface AuthContextValue {
  user: UserView | null
  loading: boolean
  adminClaimed: boolean | null
  refresh: () => Promise<void>
  refreshAdminStatus: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue>({
  user: null,
  loading: true,
  adminClaimed: null,
  refresh: async () => {},
  refreshAdminStatus: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserView | null>(null)
  const [loading, setLoading] = useState(true)
  const [adminClaimed, setAdminClaimed] = useState<boolean | null>(null)

  const refresh = async () => {
    try {
      const { data } = await api.get<UserView>('/me')
      setUser(data)
    } catch {
      setUser(null)
    }
  }

  const refreshAdminStatus = async () => {
    try {
      const { data } = await api.get<{ claimed: boolean }>('/admin/status')
      setAdminClaimed(data.claimed)
    } catch {
      setAdminClaimed(null)
    }
  }

  useEffect(() => {
    Promise.all([refresh(), refreshAdminStatus()]).finally(() => setLoading(false))
  }, [])

  return (
    <AuthContext.Provider value={{ user, loading, adminClaimed, refresh, refreshAdminStatus }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
