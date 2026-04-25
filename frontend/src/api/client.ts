import axios from 'axios'

const api = axios.create({
  baseURL: '/api',
  timeout: 10000,
  withCredentials: true,
  headers: {
    'Content-Type': 'application/json',
  },
})

// Unwrap the standard `{"success": true, "data": ...}` response envelope so
// callers receive the inner payload directly (e.g. `User[]`, not `{success, data}`).
api.interceptors.response.use(
  (response) => {
    const body = response.data
    if (body && typeof body === 'object' && 'success' in body && 'data' in body) {
      response.data = body.data
    }
    return response
  },
  (error) => {
    if (error.response?.status === 401 && !error.config?.url?.includes('/auth/')) {
      window.location.href = '/login'
    }
    const message =
      error.response?.data?.error ?? error.message ?? 'An unknown error occurred'
    return Promise.reject(new Error(message))
  },
)

export default api
