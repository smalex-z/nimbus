import axios, { AxiosInstance } from 'axios'
import type {
  ClusterStats,
  ClusterVM,
  CreateKeyRequest,
  CreateKeyResponse,
  GPUInferenceStatus,
  GPUJob,
  GPUSettingsView,
  GPUSubmitRequest,
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
  // /cluster/vms walks every VM on every online node and probes each one's
  // qemu-guest-agent for OS info — a cold cache (e.g. fresh after a binary
  // restart) can push the response past the default 10s axios cap. The
  // backend now fans out per-VM enrichment, but we still give it a longer
  // budget for the first pass; subsequent polls hit the warm cache and
  // return in <100ms.
  const { data } = await api.get<ClusterVM[]>('/cluster/vms', { timeout: 45_000 })
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

export async function deleteVM(id: number): Promise<void> {
  await api.delete(`/vms/${id}`)
}

export async function adminDeleteVM(id: number): Promise<void> {
  await api.delete(`/cluster/vms/${id}`)
}

export type VMLifecycleOp = 'start' | 'shutdown' | 'stop' | 'reboot'

// vmLifecycle issues a power op against the caller's own VM. Owner-gated;
// requesting ops on someone else's VM returns 404. Reboots can take ~30s
// while the Proxmox task drains, so the timeout is generous.
export async function vmLifecycle(id: number, op: VMLifecycleOp): Promise<void> {
  await api.post(`/vms/${id}/${op}`, undefined, { timeout: 2 * 60 * 1000 })
}

// adminVMLifecycle issues a power op against any cluster VM (local /
// foreign / external) by (node, vmid). Admin-only.
export async function adminVMLifecycle(
  node: string,
  vmid: number,
  op: VMLifecycleOp,
): Promise<void> {
  await api.post(`/cluster/vms/${encodeURIComponent(node)}/${vmid}/${op}`, undefined, {
    timeout: 2 * 60 * 1000,
  })
}

export interface VMTunnel {
  id: string
  machine_id: string
  status?: string
  subdomain?: string
  target_ip?: string
  target_port: number
  // Gopher fills these on create + read so the UI shows the actual stored
  // state, not just what was requested (UDP / no-subdomain coerce some).
  transport?: 'tcp' | 'udp'
  private?: boolean
  no_tls?: boolean
  server_port?: number
  bot_protection_enabled?: boolean
  bot_protection_ttl?: number
  bot_protection_allow_ip?: string
  tls_skip_verify?: boolean
  tunnel_url?: string
  error?: string
  created_at?: string
}

export interface CreateVMTunnelRequest {
  target_port: number
  subdomain?: string
  transport?: 'tcp' | 'udp'
  private?: boolean
  no_tls?: boolean
  bot_protection_enabled?: boolean
  bot_protection_ttl?: number
  bot_protection_allow_ip?: string
  tls_skip_verify?: boolean
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

export type SelfBootstrapState =
  | ''
  | 'registering'
  | 'installing'
  | 'waiting_connect'
  | 'creating_tunnel'
  | 'active'
  | 'failed'

export interface SelfBootstrapStatus {
  state: SelfBootstrapState
  error?: string
  tunnel_url?: string
}

export async function getSelfBootstrapStatus(): Promise<SelfBootstrapStatus> {
  const { data } = await api.get<SelfBootstrapStatus>('/settings/gopher/self-bootstrap')
  return data
}

export async function startSelfBootstrap(): Promise<void> {
  await api.post('/settings/gopher/self-bootstrap', {})
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
  vm_prefix_len?: number
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
  // password is true when the sign-in page should still render the
  // email/password form. Stays true unless the admin has set
  // passwordless_goal AND every user has linked an OAuth provider.
  password: boolean
  // passwordless_goal is the admin's stated intent. When true but
  // password is also still true, the system is in transition (some
  // users haven't linked yet); the UI uses both fields to render the
  // explanatory banner.
  passwordless_goal: boolean
}

export interface AccountView {
  id: number
  name: string
  email: string
  is_admin: boolean
  has_password: boolean
  google_connected: boolean
  github_connected: boolean
}

export async function getAccount(): Promise<AccountView> {
  const { data } = await api.get<AccountView>('/account')
  return data
}

export interface PasswordlessStatus {
  passwordless_goal: boolean
  stragglers: number
  password_active: boolean
}

export async function getPasswordlessStatus(): Promise<PasswordlessStatus> {
  const { data } = await api.get<PasswordlessStatus>('/settings/oauth/passwordless')
  return data
}

export async function setPasswordlessAuth(enabled: boolean): Promise<PasswordlessStatus> {
  const { data } = await api.put<PasswordlessStatus>('/settings/oauth/passwordless', { enabled })
  return data
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

export interface UserManagementView {
  id: number
  name: string
  email: string
  is_admin: boolean
  created_at: string
  verified: boolean
  // Best-effort sign-in providers the user has used at least once. May
  // contain "password" (email/password registered), "github" (GitHub OAuth
  // login captured), and/or "google" (inferred when neither password nor
  // github is set — Google OAuth doesn't leave a per-user marker today).
  providers: string[]
}

export async function listUsers(): Promise<UserManagementView[]> {
  const { data } = await api.get<UserManagementView[]>('/users')
  return data
}

// promoteUser flips a member to admin after re-confirming the requesting
// admin's password. Returns the upgraded user or rejects with a
// 401-translated "incorrect password" message when the gate fails.
export async function promoteUser(id: number, password: string): Promise<{ id: number; is_admin: boolean }> {
  const { data } = await api.post<{ id: number; is_admin: boolean }>(`/users/${id}/promote`, { password })
  return data
}

// deleteUser removes a user with the chosen VM disposition: "delete"
// destroys their VMs on Proxmox, "transfer" reassigns ownership to the
// requesting admin. SSH keys + GPU jobs follow the same disposition so
// transferred VMs keep working.
export async function deleteUser(
  id: number,
  vmAction: 'delete' | 'transfer',
): Promise<{ id: number; vm_action: string; vms_handled: number }> {
  const { data } = await api.delete<{ id: number; vm_action: string; vms_handled: number }>(
    `/users/${id}`,
    { data: { vm_action: vmAction } },
  )
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

// ── S3 storage (Phase 3 — singleton MinIO VM) ────────────────────────────

export type S3Status = 'deploying' | 'ready' | 'error' | 'deleting'

export interface S3StorageView {
  vmid: number
  node: string
  status: S3Status
  disk_gb: number
  endpoint?: string
  root_user?: string
  root_password?: string
  error_msg?: string
}

export interface S3Bucket {
  name: string
  created_at: string
  object_count: number
  total_size_bytes: number
}

export interface S3DeployProgress {
  step: string
  label: string
}

// getS3Storage returns the singleton storage row, or null when nothing
// is deployed (the API returns 404 in that case — we translate that to
// null so the UI can render the empty state without try/catch).
export async function getS3Storage(): Promise<S3StorageView | null> {
  try {
    const { data } = await api.get<S3StorageView>('/s3/storage')
    return data
  } catch (err) {
    if (err instanceof Error && err.message === 'no s3 storage deployed') return null
    throw err
  }
}

// deployS3Storage POSTs to /api/s3/storage, NDJSON-streamed like
// provisionVMStreaming. Same shape: progress events, then a single
// terminal `result` (success) or `error` line. Resolves with the final
// storage row.
export async function deployS3Storage(
  diskGB: number,
  onProgress: (evt: S3DeployProgress) => void,
  signal?: AbortSignal,
): Promise<S3StorageView> {
  const resp = await fetch('/api/s3/storage', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', Accept: 'application/x-ndjson' },
    body: JSON.stringify({ disk_gb: diskGB }),
    signal,
  })
  if (!resp.ok) {
    let message = `request failed (${resp.status})`
    try {
      const body = await resp.json()
      if (body?.error) message = body.error
    } catch {
      // body wasn't JSON
    }
    throw new Error(message)
  }
  if (!resp.body) throw new Error('server returned no body')

  const reader = resp.body.getReader()
  const decoder = new TextDecoder('utf-8')
  let buffer = ''
  let done = false
  while (!done) {
    const chunk = await reader.read()
    done = chunk.done
    if (chunk.value) buffer += decoder.decode(chunk.value, { stream: true })

    let nl = buffer.indexOf('\n')
    while (nl >= 0) {
      const line = buffer.slice(0, nl).trim()
      buffer = buffer.slice(nl + 1)
      nl = buffer.indexOf('\n')
      if (!line) continue

      let evt: { type?: string; step?: string; label?: string; data?: S3StorageView; message?: string }
      try {
        evt = JSON.parse(line)
      } catch {
        continue
      }
      if (evt.type === 'progress' && evt.step && evt.label) {
        onProgress({ step: evt.step, label: evt.label })
      } else if (evt.type === 'result' && evt.data) {
        return evt.data
      } else if (evt.type === 'error') {
        throw new Error(evt.message ?? 's3 storage deploy failed')
      }
    }
  }
  throw new Error('deploy stream ended without a result')
}

export async function deleteS3Storage(): Promise<void> {
  await api.delete('/s3/storage')
}

export async function listS3Buckets(): Promise<S3Bucket[]> {
  const { data } = await api.get<S3Bucket[]>('/s3/buckets')
  return data ?? []
}

export async function createS3Bucket(name: string): Promise<{ name: string }> {
  const { data } = await api.post<{ name: string }>('/s3/buckets', { name })
  return data
}

export async function deleteS3Bucket(name: string): Promise<void> {
  await api.delete(`/s3/buckets/${encodeURIComponent(name)}`)
}

// ──────────────────────── GPU plane (Phase 4) ────────────────────────

export async function listGPUJobs(status?: string): Promise<GPUJob[]> {
  const params = status ? { status } : {}
  const { data } = await api.get<GPUJob[]>('/gpu/jobs', { params })
  return data
}

export async function getGPUJob(id: number): Promise<GPUJob> {
  const { data } = await api.get<GPUJob>(`/gpu/jobs/${id}`)
  return data
}

export async function submitGPUJob(req: GPUSubmitRequest): Promise<GPUJob> {
  const { data } = await api.post<GPUJob>('/gpu/jobs', req)
  return data
}

export async function cancelGPUJob(id: number): Promise<GPUJob> {
  const { data } = await api.post<GPUJob>(`/gpu/jobs/${id}/cancel`)
  return data
}

export async function getGPUInference(): Promise<GPUInferenceStatus> {
  const { data } = await api.get<GPUInferenceStatus>('/gpu/inference')
  return data
}

export async function getGPUSettings(): Promise<GPUSettingsView> {
  const { data } = await api.get<GPUSettingsView>('/settings/gpu')
  return data
}

export async function saveGPUSettings(req: {
  enabled: boolean
  base_url: string
  inference_model: string
}): Promise<GPUSettingsView> {
  const { data } = await api.put<GPUSettingsView>('/settings/gpu', req)
  return data
}

export interface GPUPairingView {
  token: string
  expires_in_seconds: number
  curl: string
}

// mintGPUPairingToken issues a fresh 5-minute pairing window. Returns a
// pre-baked `sudo bash <(curl ...)` command for the operator to paste on
// the GX10. Replaces any active pairing token.
export async function mintGPUPairingToken(): Promise<GPUPairingView> {
  const { data } = await api.post<GPUPairingView>('/settings/gpu/pairing')
  return data
}

export interface GPUUnpairView {
  cancelled_jobs: number
  // cleanup_cmd is the shell snippet the operator runs on the GX10 to
  // stop the systemd units. Nimbus can't reach into the GX10 itself —
  // the pairing flow is GX10-pulls-from-Nimbus, never the reverse.
  cleanup_cmd: string
}

// unpairGX10 wipes the worker token, disables the GPU plane, and bulk-
// cancels every queued/running job. Returns the cleanup command the
// operator runs on the GX10 to stop the local systemd units.
export async function unpairGX10(): Promise<GPUUnpairView> {
  const { data } = await api.post<GPUUnpairView>('/settings/gpu/unpair')
  return data
}

export interface NetworkSettingsView {
  ip_pool_start: string
  ip_pool_end: string
  gateway_ip: string
  prefix_len: number
}

export interface SaveNetworkSettingsRequest {
  ip_pool_start?: string
  ip_pool_end?: string
  gateway_ip?: string
  prefix_len?: number
}

export async function getNetworkSettings(): Promise<NetworkSettingsView> {
  const { data } = await api.get<NetworkSettingsView>('/settings/network')
  return data
}

export async function saveNetworkSettings(
  req: SaveNetworkSettingsRequest,
): Promise<NetworkSettingsView> {
  const { data } = await api.put<NetworkSettingsView>('/settings/network', req)
  return data
}

export interface NetworkOpFailure {
  vm_row_id: number
  vmid: number
  hostname: string
  error: string
}

export interface NetworkOpReport {
  updated: number
  failures: NetworkOpFailure[]
}

// renumberAllVMs reassigns every managed VM to a fresh IP from the saved pool
// and reboots them. The current saved gateway is used. Long-running — every
// VM bounces in sequence.
export async function renumberAllVMs(): Promise<NetworkOpReport> {
  const { data } = await api.post<NetworkOpReport>(
    '/settings/network/renumber-vms',
    {},
    { timeout: 15 * 60 * 1000 },
  )
  return data
}

// forceGatewayUpdate pushes the saved gateway to every managed VM via
// `qm set --ipconfig0` and reboots them. Each VM keeps its existing IP.
export async function forceGatewayUpdate(): Promise<NetworkOpReport> {
  const { data } = await api.post<NetworkOpReport>(
    '/settings/network/force-gateway-update',
    {},
    { timeout: 15 * 60 * 1000 },
  )
  return data
}

export interface VMReconcileMigration {
  vm_row_id: number
  vmid: number
  hostname: string
  from_node: string
  to_node: string
}

export interface VMReconcileMiss {
  vm_row_id: number
  vmid: number
  hostname: string
  node: string
  missed_cycles: number
}

export interface VMReconcileDeleted {
  vm_row_id: number
  vmid: number
  hostname: string
  node: string
}

export interface VMReconcileReport {
  migrated: VMReconcileMigration[]
  missed: VMReconcileMiss[]
  deleted: VMReconcileDeleted[]
  no_ops: number
  snapshot_at: string
}

// reconcileVMs walks the local vms table against the live Proxmox cluster.
// Updates rows whose VMs migrated to a different node, soft-deletes rows
// whose VMID hasn't been seen for the configured miss threshold, and reports
// rows that are still under threshold so the operator sees them going stale.
// The backend refuses (400) when Proxmox returns an empty cluster snapshot.
export async function reconcileVMs(): Promise<VMReconcileReport> {
  const { data } = await api.post<VMReconcileReport>('/vms/reconcile', {})
  return data
}

export default api
