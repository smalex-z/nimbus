package handlers

import (
	"net/http"
	"sync"

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

type clusterVMView struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"` // running, stopped, paused

	// Nimbus-managed fields. Zero values when the VM was not created by Nimbus.
	NimbusManaged bool   `json:"nimbus_managed"`
	Hostname      string `json:"hostname,omitempty"`
	IP            string `json:"ip,omitempty"`
	Tier          string `json:"tier,omitempty"`
	OSTemplate    string `json:"os_template,omitempty"`
	Username      string `json:"username,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
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
// Nimbus DB info where available.
func (h *Cluster) ListVMs(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.px.GetNodes(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	// Fan out ListVMs across online nodes.
	type nodeVMs struct {
		node string
		vms  []proxmox.VMStatus
	}
	results := make([]nodeVMs, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		if n.Status != "online" {
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			vms, err := h.px.ListVMs(r.Context(), name)
			if err != nil {
				return
			}
			results[i] = nodeVMs{node: name, vms: vms}
		}(i, n.Name)
	}
	wg.Wait()

	// Index Nimbus DB VMs by vmid for the join.
	dbVMs, err := h.svc.List(r.Context(), nil)
	if err != nil {
		response.FromError(w, err)
		return
	}
	byVMID := make(map[int]db.VM, len(dbVMs))
	for _, v := range dbVMs {
		byVMID[v.VMID] = v
	}

	out := make([]clusterVMView, 0)
	for _, nv := range results {
		for _, vm := range nv.vms {
			if vm.Template != 0 {
				continue
			}
			view := clusterVMView{
				VMID:   vm.VMID,
				Name:   vm.Name,
				Node:   nv.node,
				Status: vm.Status,
			}
			if managed, ok := byVMID[vm.VMID]; ok {
				view.NimbusManaged = true
				view.Hostname = managed.Hostname
				view.IP = managed.IP
				view.Tier = managed.Tier
				view.OSTemplate = managed.OSTemplate
				view.Username = managed.Username
				view.CreatedAt = managed.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
			}
			out = append(out, view)
		}
	}
	response.Success(w, out)
}
