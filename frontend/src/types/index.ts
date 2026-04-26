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
}

export interface ProvisionRequest {
  hostname: string
  tier: TierName
  os_template: OSTemplate
  ssh_key_id?: number
  ssh_pubkey?: string
  ssh_privkey?: string
  generate_key?: boolean
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
}

// Backend-emitted phase IDs streamed during a provision call. Keep in sync
// with internal/provision/types.go.
export type ProvisionStep =
  | 'reserve_ip'
  | 'clone_template'
  | 'configure_vm'
  | 'start_vm'
  | 'wait_guest_agent'

export interface ProvisionProgress {
  step: ProvisionStep
  label: string
}

export interface NodeView {
  name: string
  status: 'online' | 'offline' | 'unknown'
  cpu: number
  max_cpu: number
  mem_used: number
  mem_total: number
  mem_allocated: number
  swap_used: number
  swap_total: number
  vm_count: number
  vm_count_total: number
}

export interface ClusterStats {
  storage_used: number
  storage_total: number
}

export type ClusterVMStatus = 'running' | 'stopped' | 'paused'

export type VMSource = 'local' | 'foreign' | 'external'

export interface ClusterVM {
  vmid: number
  name: string
  node: string
  status: ClusterVMStatus
  source: VMSource
  nimbus_managed: boolean
  // id is the Nimbus DB row id; present only for local-source VMs. Used by
  // the admin SSH modal to call the per-VM private-key download endpoint.
  id?: number
  // key_name is the SSH key file name; present only for local-source VMs
  // that were provisioned with a vault-stored key.
  key_name?: string
  hostname?: string
  ip?: string
  // ip_source identifies how the IP was discovered: "ipconfig0" (cloud-init)
  // or "agent" (qemu-guest-agent fallback). Empty when the IP came from the
  // local Nimbus DB.
  ip_source?: string
  // tier is one of TierName for Nimbus-managed VMs, or "custom" for external.
  tier?: TierName | 'custom'
  // os_template is a known OSTemplate for Nimbus-managed VMs, or a raw
  // Proxmox ostype hint (e.g. "l26", "win10") for external VMs.
  os_template?: OSTemplate | string
  username?: string
  created_at?: string
  // Best-effort qemu-guest-agent OS info. Empty when the agent is unavailable
  // or the VM is stopped. Cached server-side for 24h since OS rarely changes
  // on a running VM.
  os_id?: string         // "ubuntu" / "debian" / "mswindows" / …
  os_pretty?: string     // "Ubuntu 22.04.3 LTS"
  os_version?: string    // "22.04.3 LTS (Jammy Jellyfish)"
  os_version_id?: string // "22.04"
  os_kernel?: string     // "5.15.0-91-generic"
  os_machine?: string    // "x86_64"
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
