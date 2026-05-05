package handlers

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

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
	// TunnelURL is "host:port" when the VM has an established Gopher SSH
	// tunnel. Set only for local-source VMs; surfaced so the admin SSH
	// modal can render the public ssh command alongside the LAN one.
	TunnelURL string `json:"tunnel_url,omitempty"`

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
//
// @Summary     Aggregate cluster-level stats (admin)
// @Description Currently just storage totals (used + total bytes). Shared
// @Description storage pools are deduped across nodes so the totals reflect
// @Description the actual cluster footprint, not the sum of per-node views.
// @Tags        cluster
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=clusterStatsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /cluster/stats [get]
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
//
// @Summary     List every VM on the cluster (admin)
// @Description Joined view: Proxmox cluster walk + Nimbus DB rows. The Source
// @Description field distinguishes local-Nimbus / foreign-Nimbus / external
// @Description VMs so the SPA can gate features (e.g. SSH copy needs local
// @Description credentials, available only for source="local").
// @Tags        cluster
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]clusterVMView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /cluster/vms [get]
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
			view.TunnelURL = managed.TunnelURL
			view.CreatedAt = managed.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
			if managed.IP != "" {
				view.IP = managed.IP
				view.IPSource = "" // DB-sourced; not from cluster walk
			}
			out = append(out, view)
			continue
		}

		// Tier/OS for foreign-Nimbus VMs come from the description marker
		// (current scheme) with a fallback to legacy `nimbus-tier-*` /
		// `nimbus-os-*` tags for VMs whose owning instance hasn't migrated
		// yet. The bare `nimbus` tag remains the recognition signal.
		legacyTier, legacyOS, isNimbus := proxmox.ParseNimbusTags(d.Tags)
		descTier, descOS, hasDesc := proxmox.ParseNimbusDescription(d.Description)
		tier, osTemplate := descTier, descOS
		if !hasDesc {
			tier, osTemplate = legacyTier, legacyOS
		}
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

// DeleteVM handles DELETE /api/cluster/vms/{id} — admin-only destroy of a
// local-source VM by Nimbus DB row id, regardless of who provisioned it.
// Foreign and external VMs (no DB row) cannot be reached through this
// endpoint; they return 404.
//
// @Summary     Destroy a local-source VM by DB id (admin)
// @Description Foreign / external VMs have no DB row and 404 here; route them
// @Description through POST /cluster/vms/{node}/{vmid}/{op} with op="stop"
// @Description and remove from Proxmox directly instead.
// @Tags        cluster
// @Security    cookieAuth
// @Param       id path int true "Nimbus VM DB id"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /cluster/vms/{id} [delete]
func (h *Cluster) DeleteVM(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	if err := h.svc.AdminDelete(r.Context(), uint(id)); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// VMLifecycle handles POST /api/cluster/vms/{node}/{vmid}/{op} where op is
// one of start | shutdown | stop | reboot. Admin-only. Works on any source
// (local, foreign, external) since it routes by (node, vmid) instead of
// nimbus DB row id. For local rows the status column is updated
// optimistically; the reconciler corrects any drift.
//
// @Summary     Run a power op on any cluster VM (admin)
// @Description Routes by (node, vmid) so foreign + external VMs are reachable
// @Description without a Nimbus DB row. Reboot waits on a Proxmox task — the
// @Description handler timeout is 2 minutes.
// @Tags        cluster
// @Security    cookieAuth
// @Param       node path string true "Proxmox node name"
// @Param       vmid path int    true "Proxmox VMID"
// @Param       op   path string true "Lifecycle op" Enums(start, shutdown, stop, reboot)
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /cluster/vms/{node}/{vmid}/{op} [post]
func (h *Cluster) VMLifecycle(w http.ResponseWriter, r *http.Request) {
	node := chi.URLParam(r, "node")
	vmidStr := chi.URLParam(r, "vmid")
	vmid, err := strconv.Atoi(vmidStr)
	if err != nil || vmid <= 0 {
		response.BadRequest(w, "invalid vmid")
		return
	}
	op := provision.VMLifecycleOp(chi.URLParam(r, "op"))
	if err := h.svc.AdminLifecycleByVMID(r.Context(), node, vmid, op); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
