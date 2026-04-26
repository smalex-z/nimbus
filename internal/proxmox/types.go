package proxmox

// Node is the subset of /api2/json/nodes data Nimbus reads. Field names match
// the Proxmox JSON keys after Go's standard tag-based decoding.
type Node struct {
	Name   string  `json:"node"`
	Status string  `json:"status"` // "online" / "offline" / "unknown"
	CPU    float64 `json:"cpu"`    // 0.0..1.0
	MaxCPU int     `json:"maxcpu"`
	Mem    uint64  `json:"mem"`    // bytes used
	MaxMem uint64  `json:"maxmem"` // bytes total
}

// MemPair is the {used,total,free} shape Proxmox returns inside its node
// status response. Both `memory` and `swap` use this layout.
type MemPair struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// NodeStatus is the subset of /nodes/{node}/status we read — currently just
// the swap counters (the physical memory totals are already on Node).
type NodeStatus struct {
	Memory MemPair `json:"memory"`
	Swap   MemPair `json:"swap"`
}

// VMStatus is the subset of /nodes/{node}/qemu data we consume.
type VMStatus struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "running" / "stopped"
	Template int    `json:"template"`
}

// ClusterStorage is one row from /cluster/resources?type=storage. A shared
// storage pool appears once per node; callers should dedupe by Storage name
// when Shared==1 to avoid double-counting.
//
// /cluster/resources reports capacity under maxdisk/disk (NOT total/used —
// those keys are only on /nodes/{node}/storage).
type ClusterStorage struct {
	Storage string `json:"storage"`
	Node    string `json:"node"`
	Shared  int    `json:"shared"`
	Total   uint64 `json:"maxdisk"`
	Used    uint64 `json:"disk"`
}

// ClusterVM is one row from /cluster/resources?type=vm — every VM (running or
// stopped, plus templates) on every node, with both the configured ceiling
// (maxmem/maxdisk/maxcpu) and live usage (mem/cpu). The scorer sums MaxMem
// across non-template rows per node to derive committed RAM, so a node hosting
// stopped VMs is not treated as having free capacity those VMs would reclaim
// on restart.
type ClusterVM struct {
	VMID     int    `json:"vmid"`
	Node     string `json:"node"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "running" / "stopped"
	Template int    `json:"template"`
	MaxMem   uint64 `json:"maxmem"`  // configured RAM (bytes)
	MaxDisk  uint64 `json:"maxdisk"` // configured disk (bytes)
	MaxCPU   int    `json:"maxcpu"`  // configured vCPU count
}

// Storage describes one storage entry on a node, returned by GET /nodes/{n}/storage.
//
// The Content field is a comma-separated list (Proxmox sends it as a string) of
// the content types this storage accepts: "iso", "images", "rootdir", "backup",
// "vztmpl", "snippets". Bootstrap uses this to pick the right storage for
// downloaded cloud images vs. VM disks.
type Storage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`    // "dir", "lvmthin", "nfs", "ceph", ...
	Content string `json:"content"` // comma-separated
	Enabled int    `json:"enabled"` // 0/1
	Active  int    `json:"active"`  // 0/1
}

// CreateVMOpts are the parameters for CreateVMWithImport. See the method
// docstring for the exact wire format these get translated into.
type CreateVMOpts struct {
	Name        string
	Memory      int    // MB
	Cores       int    // vCPUs
	Bridge      string // network bridge, e.g. "vmbr0"
	DiskStorage string // e.g. "local-lvm" — where the imported disk lands
	// ImagePath identifies the source image to import. API tokens cannot pass
	// raw filesystem paths — use a Proxmox volid like "local:iso/foo.img"
	// instead of "/var/lib/vz/template/iso/foo.img". The field name is kept
	// for backward compat but the value should always be a volid in practice.
	ImagePath    string
	SerialOnly   bool   // cloud images need serial0=socket + vga=serial0 to boot headless
	AgentEnabled bool   // enables the qemu-guest-agent feature flag
	OSType       string // "l26" for Linux 2.6+ — Proxmox uses this for sane defaults
}

// CloudInitConfig carries the per-clone configuration Nimbus applies after
// cloning a template. Cloud-init fields (CIUser, SSHKeys, …) and hardware
// fields (Cores, Memory) all target the same Proxmox /config endpoint, so
// they're set in one round-trip — Cores/Memory must be applied here because
// a fresh clone otherwise inherits the template's small defaults.
//
// SSHKeys is the *raw* OpenSSH authorized-keys string (one or more keys, one
// per line). The client URL-encodes it on the wire — callers MUST NOT
// pre-encode.
type CloudInitConfig struct {
	CIUser       string
	SSHKeys      string
	IPConfig0    string // e.g. "ip=192.168.0.142/24,gw=192.168.0.1"
	Nameserver   string // e.g. "1.1.1.1 8.8.8.8"
	SearchDomain string // e.g. "local"
	Cores        int    // vCPU count; 0 leaves the cloned value unchanged
	Memory       int    // memory in MiB; 0 leaves the cloned value unchanged
	// CPU is the Proxmox CPU model (e.g. "x86-64-v3", "host"). Empty leaves
	// the cloned value unchanged — which means falling back to whatever the
	// template set (typically Proxmox's default kvm64/x86-64-v2-AES, neither
	// of which exposes AVX/AVX2 to the guest). Set "x86-64-v3" or higher when
	// the workload needs AVX2 (Bun, Claude Code, modern numerics).
	CPU string
}

// IPAddress is one entry inside a NetworkInterface's ip-addresses list.
type IPAddress struct {
	IPAddressType string `json:"ip-address-type"`
	IPAddress     string `json:"ip-address"`
	Prefix        int    `json:"prefix"`
}

// NetworkInterface is a single interface returned by the qemu-guest-agent's
// network-get-interfaces command.
type NetworkInterface struct {
	Name        string      `json:"name"`
	IPAddresses []IPAddress `json:"ip-addresses"`
}

// ClusterIP is one observed static IP claim parsed from a VM's cloud-init
// ipconfig0 setting. The reconciliation layer treats the union of all
// ClusterIPs (across every node in the cluster) as the source of truth for
// what IPs are actually in use, in contrast with the per-instance pool DB.
type ClusterIP struct {
	IP        string // bare "192.168.0.142", no /N suffix
	VMID      int
	Node      string
	Hostname  string // VM's `name` field; empty if unset on the VM
	Source    string // "ipconfig0" today; reserved for future "agent" reads
	RawConfig string // verbatim ipconfig0 value — retained for debugging
}

// taskStatus is what /nodes/{node}/tasks/{upid}/status returns.
type taskStatus struct {
	Status     string `json:"status"`     // "running" / "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" on success
}

// Template OS aliases — the Phase 1 OS catalogue. Index = offset from
// PROXMOX_TEMPLATE_BASE_VMID. Order matches design doc §6.2.
var TemplateOffsets = map[string]int{
	"ubuntu-24.04": 0,
	"ubuntu-22.04": 1,
	"debian-12":    2,
	"debian-11":    3,
}

// TemplateUsername returns the cloud-init default username for a given OS.
// Cloud-init images ship with a built-in user that becomes the SSH user when
// `ciuser` is unset, but Nimbus always sets `ciuser` explicitly to match.
func TemplateUsername(os string) string {
	if len(os) >= 6 && os[:6] == "ubuntu" {
		return "ubuntu"
	}
	return "debian"
}
