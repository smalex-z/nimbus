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
  error_msg?: string
}

export interface ProvisionRequest {
  hostname: string
  tier: TierName
  os_template: OSTemplate
  ssh_pubkey?: string
  generate_key?: boolean
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
  // Non-empty when the VM was created but reachability couldn't be confirmed
  // (usually Nimbus running outside the cluster LAN). Credentials are valid.
  warning?: string
}

export interface NodeView {
  name: string
  status: 'online' | 'offline' | 'unknown'
  cpu: number
  max_cpu: number
  mem_used: number
  mem_total: number
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
