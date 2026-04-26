package handlers

import (
	"net/http"
	"sync"

	"nimbus/internal/api/response"
	"nimbus/internal/proxmox"
)

// Nodes wraps the proxmox client for read-through cluster status.
type Nodes struct {
	px *proxmox.Client
}

func NewNodes(px *proxmox.Client) *Nodes { return &Nodes{px: px} }

type nodeView struct {
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	CPU          float64 `json:"cpu"`
	MaxCPU       int     `json:"max_cpu"`
	MemUsed      uint64  `json:"mem_used"`
	MemTotal     uint64  `json:"mem_total"`
	MemAllocated uint64  `json:"mem_allocated"`  // sum of maxmem across all non-template VMs
	SwapUsed     uint64  `json:"swap_used"`      // bytes paged out (0 when no swap pressure)
	SwapTotal    uint64  `json:"swap_total"`     // configured swap on the host
	VMCount      int     `json:"vm_count"`       // running, non-template
	VMCountTotal int     `json:"vm_count_total"` // all non-template (running + stopped)
}

// List handles GET /api/nodes.
func (h *Nodes) List(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.px.GetNodes(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	// One cluster-wide call gives us every VM with its configured maxmem and
	// status, replacing the per-node ListVMs fan-out and letting us derive
	// allocated-RAM totals in the same pass.
	vms, err := h.px.GetClusterVMs(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	type aggregate struct {
		running      int
		total        int
		memAllocated uint64
	}
	agg := make(map[string]aggregate, len(nodes))
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		a := agg[vm.Node]
		a.total++
		a.memAllocated += vm.MaxMem
		if vm.Status == "running" {
			a.running++
		}
		agg[vm.Node] = a
	}

	// Swap counters live on /nodes/{node}/status, not /nodes — fan out per
	// online node in parallel. A failed status call shouldn't block the rest:
	// we leave swap fields at zero for that node and serve the page anyway.
	swap := make([]proxmox.MemPair, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		if n.Status != "online" {
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			st, err := h.px.GetNodeStatus(r.Context(), name)
			if err != nil {
				return
			}
			swap[i] = st.Swap
		}(i, n.Name)
	}
	wg.Wait()

	out := make([]nodeView, 0, len(nodes))
	for i, n := range nodes {
		a := agg[n.Name]
		out = append(out, nodeView{
			Name: n.Name, Status: n.Status,
			CPU: n.CPU, MaxCPU: n.MaxCPU,
			MemUsed: n.Mem, MemTotal: n.MaxMem,
			MemAllocated: a.memAllocated,
			SwapUsed:     swap[i].Used,
			SwapTotal:    swap[i].Total,
			VMCount:      a.running,
			VMCountTotal: a.total,
		})
	}
	response.Success(w, out)
}
