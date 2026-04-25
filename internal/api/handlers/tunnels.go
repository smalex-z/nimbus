package handlers

import (
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/tunnel"
)

// Tunnels exposes a thin admin view over the Gopher tunnel registry. The
// underlying client is optional: when GOPHER_API_URL is unset the service
// runs without tunnel support and these endpoints return 503.
type Tunnels struct {
	client *tunnel.Client
}

func NewTunnels(c *tunnel.Client) *Tunnels { return &Tunnels{client: c} }

// List handles GET /api/tunnels. Mirrors the Gopher external API — Nimbus
// does not maintain its own tunnel cache; this is a live pass-through so
// admins always see ground truth. Returns 503 when tunnel support is
// disabled (operator hasn't set GOPHER_API_URL).
func (h *Tunnels) List(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		response.ServiceUnavailable(w, "tunnel integration disabled (GOPHER_API_URL unset)")
		return
	}
	rows, err := h.client.List(r.Context())
	if err != nil {
		response.InternalError(w, "list tunnels: "+err.Error())
		return
	}
	response.Success(w, rows)
}
