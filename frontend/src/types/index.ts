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
export type VMStatus = 'provisioning' | 'running' | 'stopped' | 'paused' | 'failed' | 'unknown'

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
  // Optional host-aggregate constraint as a CSV string (e.g.
  // "fast-cpu,nvme"). Empty = no constraint; the scheduler then
  // places by capacity alone. Validated as ≤256 chars by the handler.
  required_tags?: string
  os_template: OSTemplate
  ssh_key_id?: number
  ssh_pubkey?: string
  ssh_privkey?: string
  generate_key?: boolean
  public_tunnel?: boolean
  enable_gpu?: boolean
  // SDN subnet selection — at most one of (subnet_id, subnet_name)
  // set. Both omitted means "use the user's default subnet (auto-
  // create on first provision)." Backend silently ignores both when
  // SDN is disabled cluster-wide.
  subnet_id?: number
  subnet_name?: string
  // Admin-only escape hatch: attach the VM directly to a cluster
  // bridge (e.g. "vmbr0"), bypassing per-user SDN. Members get a
  // 400. Sent only when the form's subnetMode is 'bridge'.
  bridge?: string
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
  // system_generated keys are auto-minted by Nimbus for internal VMs
  // (e.g. the S3 storage bootstrap). Hidden from the Keys list by
  // default; the page exposes a toggle to reveal them for debugging.
  system_generated?: boolean
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
  // Set when the VM landed on a per-user SDN subnet. Drives the result
  // page's "isolated subnet — IP only reachable from inside" framing.
  subnet_name?: string
  subnet_cidr?: string
  // One-time console password for the cloud-init default user.
  // Surfaced once on the result page; not persisted.
  console_password?: string
  // Set when Nimbus tried but failed to upload + attach the per-VM
  // cloud-init ISO (qga won't auto-install — surfaced so operators
  // see why the readiness check timed out).
  cloud_init_error?: string
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

// LockState mirrors db.Node.LockState. "none" is the default; the others
// are operator-set via the /nodes admin page. The provision scheduler
// skips anything other than "none".
export type NodeLockState = 'none' | 'cordoned' | 'draining' | 'drained'

// Specialization is the auto-detected node classification (vCPU per
// GiB ratio): more cores than memory → cpu; lots of memory per core
// → memory; otherwise balanced. Surfaces on the node card as a chip
// to help operators decide which tags to apply. Informational only —
// does NOT drive scoring (that's now operator-tag-driven via the
// host-aggregate model).
export type Specialization = 'cpu' | 'memory' | 'balanced'

// ScoreBreakdown is the SPA-facing payload for one (node, tier) pair.
// Components carries the per-term map the dashboard tooltip renders
// ("0.45·mem(0.85) + 0.30·cpu(0.92) + 0.25·disk(0.60) = 0.78").
// Empty when the node was rejected (Score == 0); Reasons then carries
// the nodescore.Reason strings the rejection chip displays.
export interface ScoreBreakdown {
  score: number
  components?: Record<string, number>
  spec: Specialization
  reasons?: string[]
}

// NodeViewWithScores is the decorated payload from
// GET /api/nodes?include_scores=true. The scoring matrix consumes it.
export interface NodeViewWithScores extends NodeView {
  score?: ScoreBreakdown
  preview_tier?: string
}

export interface NodeView {
  name: string
  status: 'online' | 'offline' | 'unknown'
  lock_state: NodeLockState
  locked_at?: string
  locked_by?: number
  lock_reason?: string
  tags: string[]
  // auto_tags are system-derived (currently arch: "x86" or "arm")
  // and are not editable by operators. The scheduler treats them as
  // equal to operator tags for required_tags matching.
  auto_tags: string[]
  cpu: number
  max_cpu: number
  mem_used: number
  mem_total: number
  mem_allocated: number
  // swap_used + swap_total come from /nodes/{node}/status — the Admin
  // dashboard renders them; the new /nodes admin page hides them.
  swap_used: number
  swap_total: number
  // CPU model from cpuinfo (e.g. "Intel(R) Core(TM) i7-9700K CPU @
  // 3.60GHz"). Empty when Proxmox didn't return it (nested-virt or
  // older PVE). Clock speed is not surfaced — Proxmox's cpuinfo.mhz
  // is the live P-state which inverts the apparent ranking on idle
  // vs busy nodes; the model name carries enough signal.
  cpu_model?: string
  // cpu_cores is the physical core count (sockets × cores-per-socket).
  // max_cpu above is the *thread* count. Card renders "Nc/Mt" when
  // both are known so a 4c/8t laptop chip is visually distinct from
  // a real 8c desktop chip.
  cpu_cores?: number
  // Per-node VM-disk pool metrics (the storage backend identified by
  // cfg.VMDiskStorage, default local-lvm). disk_allocated is the sum
  // of every non-template VM's configured maxdisk on this node — the
  // pessimistic "fully grown" figure. All zero when no pool is
  // configured or the node doesn't expose the pool.
  disk_used: number
  disk_total: number
  disk_allocated: number
  // Pool name actually queried; empty when disk telemetry is off.
  // Surfaced so the SPA can label the bar (e.g. "local-lvm").
  disk_pool_name?: string
  // Strongest disk class observed via /disks/list — "nvme" | "ssd" |
  // "hdd" | undefined. Card displays this instead of the pool name
  // so operators can compare storage tier across nodes at a glance;
  // the pool name moves to the row's title tooltip.
  disk_type?: 'nvme' | 'ssd' | 'hdd'
  vm_count: number
  vm_count_total: number
  // Corosync ring address for this node, when available. The IP-pool table
  // uses it to render hypervisor LAN addresses as "PROXMOX NODE <name>"
  // instead of the generic EXTERNAL chip netscan would otherwise stamp on
  // them.
  ip?: string
  // last_seen_at — wall time of the most recent reconcile observation.
  // Purely informational; the SPA may render "seen N seconds ago".
  last_seen_at: string
  // is_self_host is true for the node Nimbus itself runs on. The /nodes
  // page hides Cordon/Drain/Remove buttons on this row to prevent the
  // operator from locking themselves out.
  is_self_host: boolean
}

// SchedulingSettings is the cluster-wide overcommit policy. All three
// ratios are clamped server-side to [1.0, 64.0]. Changes take effect
// immediately on the next provision/drain — no restart required.
export interface SchedulingSettings {
  cpu_allocation_ratio: number
  ram_allocation_ratio: number
  disk_allocation_ratio: number
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
