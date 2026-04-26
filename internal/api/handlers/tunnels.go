package handlers

import (
	"net/http"
	"net/url"

	"nimbus/internal/api/response"
	"nimbus/internal/tunnel"
)

// Tunnels exposes a thin admin view over the Gopher tunnel registry. The
// underlying client is optional: when GOPHER_API_URL is unset the service
// runs without tunnel support and these endpoints return 503.
type Tunnels struct {
	client *tunnel.Client
	apiURL string // configured GOPHER_API_URL — empty when disabled
}

func NewTunnels(c *tunnel.Client, apiURL string) *Tunnels {
	return &Tunnels{client: c, apiURL: apiURL}
}

// SetClient swaps in a new tunnel.Client + apiURL. Used when admin saves new
// Gopher settings on the Settings page so /api/tunnels and /api/tunnels/info
// reflect the new credentials without a restart.
func (h *Tunnels) SetClient(c *tunnel.Client, apiURL string) {
	h.client = c
	h.apiURL = apiURL
}

// Info handles GET /api/tunnels/info. Public endpoint used by the SPA to
// preview where a user's tunnel will land before provisioning. Returns
// {enabled, host} — the host is derived from GOPHER_API_URL so the SPA can
// build a "<subdomain>.<host>" preview without hardcoding the Gopher domain.
func (h *Tunnels) Info(w http.ResponseWriter, _ *http.Request) {
	host := ""
	if h.apiURL != "" {
		if u, err := url.Parse(h.apiURL); err == nil {
			host = u.Host
		}
	}
	response.Success(w, map[string]any{
		"enabled": h.client != nil,
		"host":    host,
	})
}

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
