package handlers

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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

// tunnelInfoView is the JSON shape returned by GET /api/tunnels/info.
type tunnelInfoView struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host,omitempty"`
}

// Info handles GET /api/tunnels/info. Public endpoint used by the SPA to
// preview where a user's SSH tunnel will land before provisioning. Returns
// {enabled, host} — host is the routable hostname Gopher will expose SSH on.
//
// Host-resolution rule (from Gopher's deployment convention):
//
//	If the apex domain (e.g. altsuite.co) and the API host
//	(e.g. router.altsuite.co) resolve to the same IP, the apex is also
//	the gateway — return the shorter apex form. If they diverge (operator
//	runs personal site on the apex), return the API host. SSH is exposed
//	at <host>:<port>, where port is allocated by Gopher post-provision.
//
// @Summary     Routing-host preview for the Gopher tunnel gateway
// @Description Public so the Provision form can render the disabled-checkbox
// @Description hint copy with the right hostname before submitting. `enabled`
// @Description reflects whether Gopher credentials are configured at all.
// @Tags        tunnels
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=tunnelInfoView}
// @Router      /tunnels/info [get]
func (h *Tunnels) Info(w http.ResponseWriter, r *http.Request) {
	host := ""
	if h.apiURL != "" {
		if u, err := url.Parse(h.apiURL); err == nil && u.Host != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			host = preferredTunnelHost(ctx, u.Hostname())
		}
	}
	response.Success(w, tunnelInfoView{
		Enabled: h.client != nil,
		Host:    host,
	})
}

// preferredTunnelHost compares apex DNS to API DNS and returns whichever the
// SSH tunnel should be advertised at. Falls back to apiHost on any DNS
// failure — better to show the longer-but-correct form than silently mislead.
func preferredTunnelHost(ctx context.Context, apiHost string) string {
	apex := apexOf(apiHost)
	if apex == "" || apex == apiHost {
		return apiHost
	}
	resolver := net.DefaultResolver
	apexIPs, err1 := resolver.LookupHost(ctx, apex)
	apiIPs, err2 := resolver.LookupHost(ctx, apiHost)
	if err1 != nil || err2 != nil {
		return apiHost
	}
	apiSet := make(map[string]struct{}, len(apiIPs))
	for _, ip := range apiIPs {
		apiSet[ip] = struct{}{}
	}
	for _, ip := range apexIPs {
		if _, ok := apiSet[ip]; ok {
			return apex
		}
	}
	return apiHost
}

// apexOf strips the leftmost DNS label. Naive — doesn't consult a public
// suffix list, so it would mis-handle "example.co.uk" → "co.uk". Adequate
// for the typical "router.example.com" → "example.com" deployment shape.
func apexOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(host, ".")
	if len(parts) < 3 {
		return host
	}
	return strings.Join(parts[1:], ".")
}

// List handles GET /api/tunnels. Live pass-through to Gopher — Nimbus
// doesn't cache. The admin view is *machines* (each is an exposed VM);
// per-port tunnels-on-top are a future surface. Returns 503 when tunnel
// support is disabled (operator hasn't configured Gopher).
//
// @Summary     List Gopher-registered machines (admin)
// @Description Pass-through to the upstream Gopher `/machines` endpoint,
// @Description scoped to this Nimbus's API key. Returns 503 when Gopher
// @Description credentials aren't configured.
// @Tags        tunnels
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]tunnel.Machine}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /tunnels [get]
func (h *Tunnels) List(w http.ResponseWriter, r *http.Request) {
	if h.client == nil {
		response.ServiceUnavailable(w, "tunnel integration disabled (Gopher API URL not configured)")
		return
	}
	rows, err := h.client.ListMachines(r.Context())
	if err != nil {
		response.InternalError(w, "list machines: "+err.Error())
		return
	}
	response.Success(w, rows)
}
