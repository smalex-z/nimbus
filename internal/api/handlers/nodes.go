package handlers

import (
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/proxmox"
)

// Nodes wraps the proxmox client for read-through cluster status.
type Nodes struct {
	px *proxmox.Client
}

func NewNodes(px *proxmox.Client) *Nodes { return &Nodes{px: px} }

type nodeView struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	CPU      float64 `json:"cpu"`
	MaxCPU   int     `json:"max_cpu"`
	MemUsed  uint64  `json:"mem_used"`
	MemTotal uint64  `json:"mem_total"`
}

// List handles GET /api/nodes.
func (h *Nodes) List(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.px.GetNodes(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeView{
			Name: n.Name, Status: n.Status,
			CPU: n.CPU, MaxCPU: n.MaxCPU,
			MemUsed: n.Mem, MemTotal: n.MaxMem,
		})
	}
	response.Success(w, out)
}
