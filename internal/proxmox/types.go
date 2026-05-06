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

// ClusterStatusEntry is one row from /api2/json/cluster/status. The endpoint
// returns a heterogeneous list (cluster + per-node entries); the per-node
// rows carry the node's address. Fields we don't read are dropped — Proxmox
// also reports level, quorum state, etc.
type ClusterStatusEntry struct {
	Type   string `json:"type"`   // "cluster" | "node"
	Name   string `json:"name"`   // node hostname (empty for the cluster row)
	IP     string `json:"ip"`     // address Proxmox advertises for this node
	Online int    `json:"online"` // 1 = part of corosync quorum
	Local  int    `json:"local"`  // 1 if this is the node we made the API call on
	NodeID int    `json:"nodeid"` // numeric corosync id
}

// MemPair is the {used,total,free} shape Proxmox returns inside its node
// status response. Both `memory` and `swap` use this layout.
type MemPair struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// NodeStatus is the subset of /nodes/{node}/status we read. Memory + Swap
// are the headline fields; CPUInfo carries the model + clock-speed strings
// the dashboard displays alongside core count so operators can tell a
// 14th-gen i5 apart from a 2nd-gen i5 (the Proxmox web UI shows the same
// data on Datacenter → Node → Summary).
type NodeStatus struct {
	Memory  MemPair  `json:"memory"`
	Swap    MemPair  `json:"swap"`
	CPUInfo *CPUInfo `json:"cpuinfo,omitempty"`
}

// CPUInfo mirrors the cpuinfo block from /nodes/{node}/status. Proxmox
// reports `mhz` as a quoted string (e.g. "3600.000") — that's the
// *current* P-state from /proc/cpuinfo, not a stable base or boost
// clock — so we don't surface it. The model string is enough for
// dashboards (operators eyeball it) and feeds the auto-tag arch
// detection in the scheduler.
//
// Cores is the per-socket count; Cpus is the total logical-thread count.
// We don't always need both — Cpus matches what Node.MaxCPU already
// returns, but having Cores helps render "16c (8c × 2 sockets)" on
// dashboards if we ever want that detail.
type CPUInfo struct {
	Model   string `json:"model"`
	Cpus    int    `json:"cpus"`
	Sockets int    `json:"sockets"`
	Cores   int    `json:"cores"`
}

// Disk mirrors one row from /nodes/{node}/disks/list. Type is one of
// "ssd", "hdd", "nvme", "usb" — Proxmox's own classification, not a
// guess from device name. Used by the auto-tag derivation to decide
// whether a node carries fast storage.
//
// Other fields (vendor, serial, gpt, size, used) are present in the
// API but unused; we only need Type today.
type Disk struct {
	Devpath string `json:"devpath"`
	Type    string `json:"type"`
	Size    uint64 `json:"size"`
	Used    string `json:"used,omitempty"`
}

// PCIDevice mirrors one row from /nodes/{node}/hardware/pci. The
// `vendor` and `device_id` strings come back as 4-digit hex with a
// leading "0x" (e.g. "0x10de" for NVIDIA). DeviceName/VendorName are
// human-readable when Proxmox can resolve them via lspci's database;
// empty otherwise.
//
// We use this to detect discrete GPU presence — currently NVIDIA-only
// (vendor 10de). AMD (1002) overlaps with iGPUs in APUs and Intel
// (8086) is dominated by integrated graphics, so we don't auto-tag
// from those vendors.
type PCIDevice struct {
	ID         string `json:"id"`        // BDF, e.g. "0000:01:00.0"
	Vendor     string `json:"vendor"`    // hex like "0x10de"
	Device     string `json:"device_id"` // hex like "0x2204"
	Class      string `json:"class"`     // hex like "0x030000" (display)
	DeviceName string `json:"device_name,omitempty"`
	VendorName string `json:"vendor_name,omitempty"`
}

// VMStatus is the subset of /nodes/{node}/qemu data we consume.
type VMStatus struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "running" / "stopped"
	Template int    `json:"template"`
}

// LXCStatus is the subset of /nodes/{node}/lxc data we consume. LXC VMIDs
// share the cluster-wide ID space with QEMU VMs, so callers can index by
// VMID without disambiguating type.
type LXCStatus struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "running" / "stopped"
	Template int    `json:"template"`
}

// HAResource mirrors one row from /cluster/ha/resources. Proxmox's HA Manager
// owns the runtime — corosync heartbeats, watchdog fencing, automatic restart
// on a surviving node — and this struct is just the read-model Nimbus exposes
// in the cluster-VM list and the per-VM HA chip.
//
// SID is the resource identifier in the form "type:vmid" (e.g. "vm:100");
// State is one of started/stopped/disabled/error/migrate/relocate (plus a
// few transitional values the manager emits during failover). Group is the
// HA group name when one was assigned at register time; empty falls back to
// the cluster-default group. MaxRestart and MaxRelocate are restart-flap
// counters Proxmox enforces; we expose them so a future settings UI can
// show what's configured without re-querying.
type HAResource struct {
	SID         string `json:"sid"`
	Type        string `json:"type"`
	State       string `json:"state"`
	Group       string `json:"group,omitempty"`
	Comment     string `json:"comment,omitempty"`
	MaxRestart  int    `json:"max_restart,omitempty"`
	MaxRelocate int    `json:"max_relocate,omitempty"`
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

// ClusterVMDetail is the enriched per-VM snapshot the admin view consumes.
// One row per non-template QEMU VM on every online node. IP discovery falls
// back from cloud-init `ipconfig0` to the qemu-guest-agent's first non-loopback
// IPv4 address; an empty IP means neither source produced one.
type ClusterVMDetail struct {
	VMID        int
	Node        string
	Name        string
	Status      string   // "running" / "stopped" / "paused"
	IP          string   // bare IPv4, "" if undiscoverable
	IPSource    string   // "ipconfig0" | "agent" | "" when IP is empty
	Tags        []string // raw tags from Proxmox; preserves user-applied entries
	Description string   // raw description; carries Nimbus tier/OS marker for managed VMs
	OSType      string   // raw `ostype` field — "l26", "win10", … "" when unset
	OS          *OSInfo  // best-effort agent osinfo; nil when unavailable
}

// OSInfo mirrors the fields returned by qemu-guest-agent's guest-get-osinfo
// command. Field tags match the wire format. Any subset may be empty when the
// guest is partial-impl (Windows agents, for example, often skip `kernel-*`).
type OSInfo struct {
	ID            string `json:"id"`             // "ubuntu", "debian", "fedora", "mswindows", …
	Name          string `json:"name"`           // "Ubuntu"
	PrettyName    string `json:"pretty-name"`    // "Ubuntu 22.04.3 LTS"
	Version       string `json:"version"`        // "22.04.3 LTS (Jammy Jellyfish)"
	VersionID     string `json:"version-id"`     // "22.04"
	KernelRelease string `json:"kernel-release"` // "5.15.0-91-generic"
	KernelVersion string `json:"kernel-version"` // "#101-Ubuntu SMP …"
	Machine       string `json:"machine"`        // "x86_64"
	VariantID     string `json:"variant-id,omitempty"`
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
