import axios, { AxiosInstance } from 'axios'
import type {
  HealthResponse,
  IPAllocation,
  NodeView,
  ProvisionRequest,
  ProvisionResult,
  VM,
} from '@/types'

// Default-timeout client used for fast endpoints.
const api: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 10000,
  withCredentials: true,
  headers: { 'Content-Type': 'application/json' },
})

// Provisioning is a long-running call (template clone, cloud-init, boot,
// agent ready) that can legitimately take 60-180s. Use a separate client with
// a generous timeout so the SPA waits patiently rather than aborting.
const provisionClient: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 200000,
  withCredentials: true,
  headers: { 'Content-Type': 'application/json' },
})

// Both clients unwrap the standard `{success, data}` envelope.
const unwrap = (instance: AxiosInstance) => {
  instance.interceptors.response.use(
    (response) => {
      const body = response.data
      if (body && typeof body === 'object' && 'success' in body && 'data' in body) {
        response.data = body.data
      }
      return response
    },
    (error) => {
      const message = error.response?.data?.error ?? error.message ?? 'unknown error'
      return Promise.reject(new Error(message))
    },
  )
}
unwrap(api)
unwrap(provisionClient)

export async function getHealth(): Promise<HealthResponse> {
  const { data } = await api.get<HealthResponse>('/health')
  return data
}

export async function listNodes(): Promise<NodeView[]> {
  const { data } = await api.get<NodeView[]>('/nodes')
  return data
}

export async function listIPs(): Promise<IPAllocation[]> {
  const { data } = await api.get<IPAllocation[]>('/ips')
  return data
}

export async function listVMs(): Promise<VM[]> {
  const { data } = await api.get<VM[]>('/vms')
  return data
}

export async function provisionVM(req: ProvisionRequest): Promise<ProvisionResult> {
  const { data } = await provisionClient.post<ProvisionResult>('/vms', req)
  return data
}

export default api
