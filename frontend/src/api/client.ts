import axios, { AxiosInstance } from 'axios'
import type {
  ClusterStats,
  ClusterVM,
  CreateKeyRequest,
  CreateKeyResponse,
  HealthResponse,
  IPAllocation,
  NodeView,
  ProvisionProgress,
  ProvisionRequest,
  ProvisionResult,
  ProvisionStep,
  SSHKey,
  VM,
} from '@/types'

// Default-timeout client used for fast endpoints.
const api: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 10000,
  withCredentials: true,
  headers: { 'Content-Type': 'application/json' },
})

// Bootstrap downloads cloud images and creates templates on every cluster node.
// Can take up to 30 min on a large cluster — timeout matches server WriteTimeout.
const bootstrapClient: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 35 * 60 * 1000,
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
      const url: string = error.config?.url ?? ''
      if (redirectOn401) {
        if (
          error.response?.status === 401 &&
          !url.includes('/auth/') &&
          !url.includes('/me') &&
          !url.includes('/access-code/')
        ) {
          window.location.href = '/login'
        }
      }
      const errMsg = error.response?.data?.error
      // Server signals "non-admin user is no longer verified against the
      // current access code" with a 403 + this sentinel error string. Stash
      // the request the user was trying to make so the verify page can
      // resume it after the user enters the new code.
      if (
        error.response?.status === 403 &&
        errMsg === 'access_code_required' &&
        !url.includes('/access-code/')
      ) {
        try {
          sessionStorage.setItem('nimbus_resume_path', window.location.pathname + window.location.search)
        } catch {
          // sessionStorage may be unavailable (private mode); not fatal.
        }
        if (window.location.pathname !== '/verify') {
          window.location.href = '/verify?stale=1'
        }
      }
      const message = errMsg ?? error.message ?? 'unknown error'
      return Promise.reject(new Error(message))
    },
  )
}

unwrap(api, true)
unwrap(bootstrapClient)

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

export async function listClusterVMs(): Promise<ClusterVM[]> {
  const { data } = await api.get<ClusterVM[]>('/cluster/vms')
  return data
}

export async function getClusterStats(): Promise<ClusterStats> {
  const { data } = await api.get<ClusterStats>('/cluster/stats')
  return data
}

export class ProvisionError extends Error {
  code: 'validation' | 'conflict' | 'not_found' | 'internal' | 'network'
  failedStep?: ProvisionStep

  constructor(
    message: string,
    code: ProvisionError['code'],
    failedStep?: ProvisionStep,
  ) {
    super(message)
    this.name = 'ProvisionError'
    this.code = code
    this.failedStep = failedStep
  }
}

// provisionVMStreaming POSTs to /api/vms and reads the NDJSON response
// stream, invoking onProgress as each backend phase completes. Resolves with
// the final ProvisionResult or rejects with a ProvisionError naming the
// step that failed (when known).
export async function provisionVMStreaming(
  req: ProvisionRequest,
  onProgress: (evt: ProvisionProgress) => void,
  signal?: AbortSignal,
): Promise<ProvisionResult> {
  const resp = await fetch('/api/vms', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', Accept: 'application/x-ndjson' },
    body: JSON.stringify(req),
    signal,
  })

  // Pre-stream errors (validation, auth, etc.) come back as the regular
  // {success,error} JSON envelope with a 4xx status. Unwrap them here.
  if (!resp.ok) {
    let message = `request failed (${resp.status})`
    try {
      const body = await resp.json()
      if (body?.error) message = body.error
    } catch {
      // body wasn't JSON — fall through with the generic message
    }
    const code: ProvisionError['code'] =
      resp.status === 400 ? 'validation'
        : resp.status === 404 ? 'not_found'
          : resp.status === 503 ? 'conflict'
            : 'internal'
    throw new ProvisionError(message, code)
  }

  if (!resp.body) {
    throw new ProvisionError('server returned no body', 'network')
  }

  const reader = resp.body.getReader()
  const decoder = new TextDecoder('utf-8')
  let buffer = ''
  let lastProgress: ProvisionStep | undefined
  let done = false

  while (!done) {
    const chunk = await reader.read()
    done = chunk.done
    if (chunk.value) buffer += decoder.decode(chunk.value, { stream: true })

    // Flush every complete line we've buffered.
    let nl = buffer.indexOf('\n')
    while (nl >= 0) {
      const line = buffer.slice(0, nl).trim()
      buffer = buffer.slice(nl + 1)
      nl = buffer.indexOf('\n')
      if (!line) continue

      let evt: { type?: string; step?: ProvisionStep; label?: string; data?: ProvisionResult; code?: ProvisionError['code']; message?: string }
      try {
        evt = JSON.parse(line)
      } catch {
        continue // ignore malformed line, keep streaming
      }

      if (evt.type === 'progress' && evt.step && evt.label) {
        lastProgress = evt.step
        onProgress({ step: evt.step, label: evt.label })
      } else if (evt.type === 'result' && evt.data) {
        return evt.data
      } else if (evt.type === 'error') {
        throw new ProvisionError(
          evt.message ?? 'provision failed',
          evt.code ?? 'internal',
          // The error implicates the *next* step after the last completed
          // progress event — that's the one we were attempting when it failed.
          nextStepAfter(lastProgress),
        )
      }
    }
  }

  // Stream ended without a terminal event — treat as a network error.
  throw new ProvisionError('provision stream ended without a result', 'network')
}

const STEP_ORDER: ProvisionStep[] = [
  'reserve_ip',
  'clone_template',
  'configure_vm',
  'start_vm',
  'wait_guest_agent',
]

function nextStepAfter(step: ProvisionStep | undefined): ProvisionStep {
  if (!step) return STEP_ORDER[0]
  const i = STEP_ORDER.indexOf(step)
  return STEP_ORDER[Math.min(i + 1, STEP_ORDER.length - 1)]
}

export interface VMPrivateKey {
  key_name: string
  private_key: string
}

export async function getVMPrivateKey(id: number): Promise<VMPrivateKey> {
  const { data } = await api.get<VMPrivateKey>(`/vms/${id}/private-key`)
  return data
}

export interface VMTunnel {
  id: string
  machine_id: string
  status?: string
  subdomain?: string
  target_ip?: string
  target_port: number
  tunnel_url?: string
  error?: string
  created_at?: string
}

export interface CreateVMTunnelRequest {
  target_port: number
  subdomain?: string
}

export async function listVMTunnels(vmId: number): Promise<VMTunnel[]> {
  const { data } = await api.get<VMTunnel[]>(`/vms/${vmId}/tunnels`)
  return data
}

export async function createVMTunnel(
  vmId: number,
  req: CreateVMTunnelRequest,
): Promise<VMTunnel> {
  const { data } = await api.post<VMTunnel>(`/vms/${vmId}/tunnels`, req)
  return data
}

export async function deleteVMTunnel(vmId: number, tunnelId: string): Promise<void> {
  await api.delete(`/vms/${vmId}/tunnels/${encodeURIComponent(tunnelId)}`)
}

export async function listKeys(): Promise<SSHKey[]> {
  const { data } = await api.get<SSHKey[]>('/keys')
  return data
}

export async function createKey(req: CreateKeyRequest): Promise<CreateKeyResponse> {
  const { data } = await api.post<CreateKeyResponse>('/keys', req)
  return data
}

export async function getKeyPrivate(id: number): Promise<VMPrivateKey> {
  const { data } = await api.get<VMPrivateKey>(`/keys/${id}/private-key`)
  return data
}

export async function setDefaultKey(id: number): Promise<void> {
  await api.post(`/keys/${id}/default`, {})
}

export async function attachPrivateKey(id: number, privateKey: string): Promise<void> {
  await api.post(`/keys/${id}/private-key`, { private_key: privateKey })
}

export interface TunnelInfo {
  enabled: boolean
  host: string
}

export async function getTunnelInfo(): Promise<TunnelInfo> {
  const { data } = await api.get<TunnelInfo>('/tunnels/info')
  return data
}

export interface GopherSettingsView {
  api_url: string
  configured: boolean
}

export interface SaveGopherSettingsRequest {
  api_url?: string
  api_key?: string
}

export async function getGopherSettings(): Promise<GopherSettingsView> {
  const { data } = await api.get<GopherSettingsView>('/settings/gopher')
  return data
}

export async function saveGopherSettings(
  req: SaveGopherSettingsRequest,
): Promise<GopherSettingsView> {
  const { data } = await api.put<GopherSettingsView>('/settings/gopher', req)
  return data
}

export async function deleteKey(id: number): Promise<void> {
  await api.delete(`/keys/${id}`)
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
  gopher_api_url?: string
  gopher_api_key?: string
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

export interface TemplateOutcome {
  os: string
  vmid: number
  node: string
  duration: number
  error?: string
}

export interface BootstrapResult {
  created: TemplateOutcome[]
  skipped: TemplateOutcome[]
  failed: TemplateOutcome[]
}

export async function bootstrapTemplates(): Promise<BootstrapResult> {
  const { data } = await bootstrapClient.post<BootstrapResult>('/admin/bootstrap-templates', {})
  return data
}

export async function getBootstrapStatus(): Promise<{ bootstrapped: boolean }> {
  const { data } = await api.get<{ bootstrapped: boolean }>('/admin/bootstrap-status')
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

export interface AccessCodeView {
  access_code: string
  version: number
}

export async function getAccessCode(): Promise<AccessCodeView> {
  const { data } = await api.get<AccessCodeView>('/settings/access-code')
  return data
}

export async function regenerateAccessCode(): Promise<AccessCodeView> {
  const { data } = await api.post<AccessCodeView>('/settings/access-code/regenerate', {})
  return data
}

export async function getAccessCodeStatus(): Promise<{ verified: boolean }> {
  const { data } = await api.get<{ verified: boolean }>('/access-code/status')
  return data
}

export async function verifyAccessCode(code: string): Promise<{ verified: boolean }> {
  const { data } = await api.post<{ verified: boolean }>('/access-code/verify', { code })
  return data
}

export interface AuthorizedDomainsView {
  domains: string[]
}

export async function getAuthorizedGoogleDomains(): Promise<AuthorizedDomainsView> {
  const { data } = await api.get<AuthorizedDomainsView>('/settings/google-domains')
  return data
}

export async function saveAuthorizedGoogleDomains(
  domains: string[],
): Promise<AuthorizedDomainsView> {
  const { data } = await api.put<AuthorizedDomainsView>('/settings/google-domains', { domains })
  return data
}

export interface AuthorizedOrgsView {
  orgs: string[]
}

export async function getAuthorizedGitHubOrgs(): Promise<AuthorizedOrgsView> {
  const { data } = await api.get<AuthorizedOrgsView>('/settings/github-orgs')
  return data
}

export async function saveAuthorizedGitHubOrgs(
  orgs: string[],
): Promise<AuthorizedOrgsView> {
  const { data } = await api.put<AuthorizedOrgsView>('/settings/github-orgs', { orgs })
  return data
}

export default api
