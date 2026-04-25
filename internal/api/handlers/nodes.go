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

	// Fetch VM counts from Proxmox for each node concurrently.
	type vmCounts struct {
		running int
		total   int
	}
	counts := make([]vmCounts, len(nodes))
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
			c := vmCounts{}
			for _, vm := range vms {
				if vm.Template != 0 {
					continue
				}
				c.total++
				if vm.Status == "running" {
					c.running++
				}
			}
			counts[i] = c
		}(i, n.Name)
	}
	wg.Wait()

	out := make([]nodeView, 0, len(nodes))
	for i, n := range nodes {
		out = append(out, nodeView{
			Name: n.Name, Status: n.Status,
			CPU: n.CPU, MaxCPU: n.MaxCPU,
			MemUsed: n.Mem, MemTotal: n.MaxMem,
			VMCount:      counts[i].running,
			VMCountTotal: counts[i].total,
		})
	}
	response.Success(w, out)
}
