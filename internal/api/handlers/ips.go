package handlers

import (
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/ippool"
)

// IPs exposes the static IP pool state for debugging and the admin UI.
type IPs struct {
	pool *ippool.Pool
}

func NewIPs(pool *ippool.Pool) *IPs { return &IPs{pool: pool} }

// List handles GET /api/ips.
func (h *IPs) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.List(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, rows)
}
