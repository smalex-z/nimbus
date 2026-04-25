export interface User {
  id: number
  createdAt: string
  updatedAt: string
  deletedAt: string | null
  name: string
  email: string
  is_admin: boolean
}

export interface ApiError {
  error: string
}

export interface HealthResponse {
  status: string
  timestamp: string
}
