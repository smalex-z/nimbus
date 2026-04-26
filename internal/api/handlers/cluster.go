package handlers

import (
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// Cluster exposes cluster-wide views that join Proxmox state with the Nimbus DB.
type Cluster struct {
	px  *proxmox.Client
	svc *provision.Service
}

func NewCluster(px *proxmox.Client, svc *provision.Service) *Cluster {
	return &Cluster{px: px, svc: svc}
}

// vmSource describes which Nimbus instance (if any) provisioned a VM.
//   - "local"    — created by this Nimbus instance (in the local DB)
//   - "foreign"  — created by another Nimbus instance (carries nimbus-* tags)
//   - "external" — not Nimbus-provisioned (no marker)
type vmSource string

const (
	sourceLocal    vmSource = "local"
	sourceForeign  vmSource = "foreign"
	sourceExternal vmSource = "external"
)

type clusterVMView struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"` // running, stopped, paused

	// Source distinguishes local-Nimbus / foreign-Nimbus / external. The
	// frontend uses this for the SOURCE column and to gate features that need
	// local credentials (e.g. SSH copy).
	Source vmSource `json:"source"`

	// NimbusManaged is true for both local and foreign Nimbus VMs. Retained
	// for backward compatibility with frontend code that predates Source.
	NimbusManaged bool `json:"nimbus_managed"`

	IP         string `json:"ip,omitempty"`
	IPSource   string `json:"ip_source,omitempty"` // ipconfig0 | agent
	Tier       string `json:"tier,omitempty"`      // small/medium/large/xl, "custom" for external
	OSTemplate string `json:"os_template,omitempty"`
	Hostname   string `json:"hostname,omitempty"` // local-only Nimbus hostname
	Username   string `json:"username,omitempty"` // local-only SSH username
	CreatedAt  string `json:"created_at,omitempty"`

	// ID is the Nimbus DB row id, set only for local-source VMs. Used by the
	// admin SSH modal to call the per-VM private-key download endpoint.
	ID uint `json:"id,omitempty"`
	// KeyName is the SSH key file name, set only for local-source VMs that
	// were provisioned with a vault-stored key.
	KeyName string `json:"key_name,omitempty"`

	// OS detail block — best-effort agent osinfo, surfaced to the frontend
	// so it can render an icon, version label, and a hover popover with
	// kernel/arch details. Empty string when unavailable.
	OSID        string `json:"os_id,omitempty"`         // "ubuntu" / "debian" / "mswindows" / …
	OSPretty    string `json:"os_pretty,omitempty"`     // "Ubuntu 22.04.3 LTS"
	OSVersion   string `json:"os_version,omitempty"`    // "22.04.3 LTS (Jammy Jellyfish)"
	OSVersionID string `json:"os_version_id,omitempty"` // "22.04"
	OSKernel    string `json:"os_kernel,omitempty"`     // "5.15.0-91-generic"
	OSMachine   string `json:"os_machine,omitempty"`    // "x86_64"
}

type clusterStatsView struct {
	StorageUsed  uint64 `json:"storage_used"`
	StorageTotal uint64 `json:"storage_total"`
}

// Stats handles GET /api/cluster/stats — aggregate cluster-level stats that
// don't fit per-node (storage pools span the cluster).
func (h *Cluster) Stats(w http.ResponseWriter, r *http.Request) {
	pools, err := h.px.GetClusterStorage(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	// Shared storage appears once per node; dedupe by storage name.
	seen := make(map[string]bool)
	var used, total uint64
	for _, p := range pools {
		if p.Shared == 1 {
			if seen[p.Storage] {
				continue
			}
			seen[p.Storage] = true
		}
		used += p.Used
		total += p.Total
	}
	response.Success(w, clusterStatsView{StorageUsed: used, StorageTotal: total})
}

// ListVMs handles GET /api/cluster/vms — every VM on the cluster, joined with
// Nimbus DB info where available and enriched with Proxmox config (tags,
// ipconfig0, ostype, optional guest-agent IP fallback).
//
// Per-VM data sources, in priority order:
//  1. Local Nimbus DB row → tier, OS template, hostname, username, IP, created_at.
//  2. Proxmox tags (nimbus-tier-*, nimbus-os-*) → tier and OS for foreign-Nimbus VMs.
//  3. Proxmox ostype → best-effort OS hint for external VMs (raw "l26"/"win10"/…).
//  4. ipconfig0 / qemu-guest-agent → IP for external and foreign-Nimbus VMs.
func (h *Cluster) ListVMs(w http.ResponseWriter, r *http.Request) {
	details, err := h.px.GetClusterVMDetails(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	dbVMs, err := h.svc.List(r.Context(), nil)
	if err != nil {
		response.FromError(w, err)
		return
	}
	byVMID := make(map[int]db.VM, len(dbVMs))
	for _, v := range dbVMs {
		byVMID[v.VMID] = v
	}

	out := make([]clusterVMView, 0, len(details))
	for _, d := range details {
		view := clusterVMView{
			VMID:     d.VMID,
			Name:     d.Name,
			Node:     d.Node,
			Status:   d.Status,
			IP:       d.IP,
			IPSource: d.IPSource,
		}
		if d.OS != nil {
			view.OSID = d.OS.ID
			view.OSPretty = d.OS.PrettyName
			view.OSVersion = d.OS.Version
			view.OSVersionID = d.OS.VersionID
			view.OSKernel = d.OS.KernelRelease
			view.OSMachine = d.OS.Machine
		}

		if managed, ok := byVMID[d.VMID]; ok {
			// Local Nimbus DB row wins for tier/OS/hostname/username/IP.
			view.Source = sourceLocal
			view.NimbusManaged = true
			view.ID = managed.ID
			view.KeyName = managed.KeyName
			view.Hostname = managed.Hostname
			view.Tier = managed.Tier
			view.OSTemplate = managed.OSTemplate
			view.Username = managed.Username
			view.CreatedAt = managed.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
			if managed.IP != "" {
				view.IP = managed.IP
				view.IPSource = "" // DB-sourced; not from cluster walk
			}
			out = append(out, view)
			continue
		}

		tier, osTemplate, isNimbus := proxmox.ParseNimbusTags(d.Tags)
		switch {
		case isNimbus:
			view.Source = sourceForeign
			view.NimbusManaged = true
			view.Tier = tier
			view.OSTemplate = osTemplate
		default:
			view.Source = sourceExternal
			view.Tier = "custom"
			// OSTemplate left empty for external — the frontend renders the
			// raw ostype hint instead via a separate field if needed. For
			// now, surface a user-friendly fallback when ostype is set.
			if d.OSType != "" {
				view.OSTemplate = d.OSType
			}
		}
		out = append(out, view)
	}
	response.Success(w, out)
}
