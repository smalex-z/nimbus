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
  enable_gpu?: boolean
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
  // id is the Nimbus DB row id — used by the result page to wire the
  // Networks (per-port tunnels) modal without a separate lookup.
  id: number
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
  // tunnel_url is "host:port" when the VM has an established Gopher SSH
  // tunnel. Present only for local-source VMs.
  tunnel_url?: string
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

// IPSource describes who claimed the IP. Mirrors ippool.Source* in Go:
//   - "local"    — claimed by this Nimbus instance via Reserve/MarkAllocated
//   - "adopted"  — observed in Proxmox; another Nimbus instance owns the VM
//   - "external" — detected on the LAN by netscan; not in Proxmox at all
export type IPSource = 'local' | 'adopted' | 'external' | ''

export type IPStatus = 'free' | 'reserved' | 'allocated'

export interface IPAllocation {
  ip: string
  status: IPStatus
  vmid?: number | null
  hostname?: string | null
  reserved_at?: string | null
  allocated_at?: string | null
  last_seen_at?: string | null
  source?: IPSource
  missed_cycles?: number
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

// GPU plane (Phase 4) types ───────────────────────────────────────────

export type GPUJobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export interface GPUJob {
  id: number
  owner_id: number
  vm_id?: number
  status: GPUJobStatus
  image: string
  command: string
  env?: Record<string, string>
  worker_id?: string
  exit_code?: number
  artifact_path?: string
  error_msg?: string
  queued_at: string
  started_at?: string
  finished_at?: string
  log_tail?: string
}

export interface GPUSubmitRequest {
  image: string
  command: string
  env?: Record<string, string>
  vm_id?: number
}

export interface GPUInferenceStatus {
  enabled: boolean
  base_url?: string
  model?: string
  status: 'up' | 'down' | 'unconfigured'
}

export interface GPUSettingsView {
  enabled: boolean
  base_url: string
  inference_model: string
  configured: boolean
  // gx10_hostname is the GX10's self-reported hostname at pairing time.
  // Empty before the first successful pairing.
  gx10_hostname?: string
}
