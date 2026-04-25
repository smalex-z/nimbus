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
// agent ready) that can legitimately take 60-180s.
const provisionClient: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 200000,
  withCredentials: true,
  headers: { 'Content-Type': 'application/json' },
})

// Both clients unwrap the standard `{success, data}` envelope.
const unwrap = (instance: AxiosInstance, redirectOn401 = false) => {
  instance.interceptors.response.use(
    (response) => {
      const body = response.data
      if (body && typeof body === 'object' && 'success' in body && 'data' in body) {
        response.data = body.data
      }
      return response
    },
    (error) => {
      if (redirectOn401) {
        const url: string = error.config?.url ?? ''
        if (
          error.response?.status === 401 &&
          !url.includes('/auth/') &&
          !url.includes('/me')
        ) {
          window.location.href = '/login'
        }
      }
      const message = error.response?.data?.error ?? error.message ?? 'unknown error'
      return Promise.reject(new Error(message))
    },
  )
}

unwrap(api, true)
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

export interface DiscoverResult {
  is_hypervisor: boolean
  endpoints: string[]
  suggested_gateway?: string
}

export async function discoverProxmox(): Promise<DiscoverResult> {
  const { data } = await api.get<DiscoverResult>('/setup/discover')
  return data
}

export interface SetupStatus {
  configured: boolean
  needs_admin_setup: boolean
}

export interface TestConnRequest {
  proxmox_host: string
  proxmox_token_id: string
  proxmox_token_secret: string
}

export interface SaveConfigRequest {
  proxmox_host: string
  proxmox_token_id: string
  proxmox_token_secret: string
  ip_pool_start: string
  ip_pool_end: string
  gateway_ip: string
  nameserver?: string
  search_domain?: string
  port?: string
}

export async function getSetupStatus(): Promise<SetupStatus> {
  const { data } = await api.get<SetupStatus>('/setup/status')
  return data
}

export async function testProxmoxConnection(
  req: TestConnRequest,
): Promise<{ proxmox_version: string }> {
  const { data } = await api.post<{ proxmox_version: string }>('/setup/test', req)
  return data
}

export async function saveSetupConfig(req: SaveConfigRequest): Promise<{ message: string }> {
  const { data } = await api.post<{ message: string }>('/setup/save', req)
  return data
}

export interface CreateAdminRequest {
  name: string
  email: string
  password: string
}

export async function createAdminAccount(req: CreateAdminRequest): Promise<{ id: number; name: string; email: string; is_admin: boolean }> {
  const { data } = await api.post('/setup/admin', req)
  return data
}

export interface OAuthProviders {
  github: boolean
  google: boolean
}

export interface OAuthSettingsView {
  github_client_id: string
  google_client_id: string
  github_configured: boolean
  google_configured: boolean
}

export interface SaveOAuthSettingsRequest {
  github_client_id?: string
  github_client_secret?: string
  google_client_id?: string
  google_client_secret?: string
}

export async function getProviders(): Promise<OAuthProviders> {
  const { data } = await api.get<OAuthProviders>('/auth/providers')
  return data
}

export async function getOAuthSettings(): Promise<OAuthSettingsView> {
  const { data } = await api.get<OAuthSettingsView>('/settings/oauth')
  return data
}

export async function saveOAuthSettings(
  req: SaveOAuthSettingsRequest,
): Promise<{ message: string }> {
  const { data } = await api.put<{ message: string }>('/settings/oauth', req)
  return data
}

export default api
