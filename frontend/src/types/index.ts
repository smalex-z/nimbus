export interface User {
  id: number
  createdAt: string
  updatedAt: string
  deletedAt: string | null
  name: string
  email: string
  is_admin: boolean
}

export type TierName = 'small' | 'medium' | 'large' | 'xl'
export type OSTemplate = 'ubuntu-24.04' | 'ubuntu-22.04' | 'debian-12' | 'debian-11'
export type VMStatus = 'provisioning' | 'running' | 'failed'

export interface Tier {
  name: TierName
  cpu: number
  memMB: number
  diskGB: number
}

export const TIERS: Record<TierName, Tier> = {
  small: { name: 'small', cpu: 1, memMB: 1024, diskGB: 15 },
  medium: { name: 'medium', cpu: 2, memMB: 2048, diskGB: 30 },
  large: { name: 'large', cpu: 4, memMB: 4096, diskGB: 60 },
  xl: { name: 'xl', cpu: 8, memMB: 8192, diskGB: 120 },
}

export const OS_OPTIONS: Array<{ value: OSTemplate; label: string }> = [
  { value: 'ubuntu-24.04', label: 'Ubuntu 24.04 LTS' },
  { value: 'ubuntu-22.04', label: 'Ubuntu 22.04 LTS' },
  { value: 'debian-12', label: 'Debian 12 (Bookworm)' },
  { value: 'debian-11', label: 'Debian 11 (Bullseye)' },
]

export interface VM {
  ID: number
  CreatedAt: string
  UpdatedAt: string
  DeletedAt: string | null
  vmid: number
  hostname: string
  ip: string
  node: string
  tier: TierName
  os_template: OSTemplate
  username: string
  status: VMStatus
  owner_id?: number | null
  key_name?: string
  error_msg?: string
  tunnel_id?: string
  tunnel_url?: string
  tunnel_error?: string
}

export interface ProvisionRequest {
  hostname: string
  tier: TierName
  os_template: OSTemplate
  ssh_key_id?: number
  ssh_pubkey?: string
  ssh_privkey?: string
  generate_key?: boolean
  public_tunnel?: boolean
}

export interface SSHKey {
  id: number
  name: string
  label?: string
  public_key: string
  fingerprint?: string
  is_default: boolean
  owner_id?: number | null
  source?: 'imported' | 'generated' | 'vm-auto' | string
  has_private_key: boolean
  created_at: string
  updated_at: string
}

export interface CreateKeyRequest {
  name: string
  label?: string
  public_key?: string
  private_key?: string
  generate?: boolean
  set_default?: boolean
}

// CreateKeyResponse extends the stored row with `private_key` populated when
// the server just generated the keypair — the only time it crosses the wire
// outside of an explicit download.
export interface CreateKeyResponse extends SSHKey {
  private_key?: string
}

export interface ProvisionResult {
  vmid: number
  hostname: string
  ip: string
  username: string
  os: OSTemplate
  tier: TierName
  node: string
  ssh_private_key?: string
  key_name?: string
  // Non-empty when the VM was created but reachability couldn't be confirmed
  // (usually Nimbus running outside the cluster LAN). Credentials are valid.
  warning?: string
  // Tunnel fields (Phase 2 — Gopher integration). tunnel_url is set when the
  // tunnel is active; tunnel_error carries any tunnel-specific failure. VM
  // success is independent of either.
  tunnel_url?: string
  tunnel_error?: string
}

export interface NodeView {
  name: string
  status: 'online' | 'offline' | 'unknown'
  cpu: number
  max_cpu: number
  mem_used: number
  mem_total: number
  vm_count: number
  vm_count_total: number
}

export interface ClusterStats {
  storage_used: number
  storage_total: number
}

export type ClusterVMStatus = 'running' | 'stopped' | 'paused'

export interface ClusterVM {
  vmid: number
  name: string
  node: string
  status: ClusterVMStatus
  nimbus_managed: boolean
  hostname?: string
  ip?: string
  tier?: TierName
  os_template?: OSTemplate
  username?: string
  created_at?: string
}

export interface IPAllocation {
  ip: string
  status: 'free' | 'reserved' | 'allocated'
  vmid?: number | null
  hostname?: string | null
  reserved_at?: string | null
  allocated_at?: string | null
}

export interface HealthResponse {
  status: string
  timestamp: string
  version: string
  proxmox_ok: boolean
  proxmox_version?: string
  proxmox_error?: string
}

export interface ApiError {
  error: string
}
