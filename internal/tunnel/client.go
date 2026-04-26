// Package tunnel is a focused client for Gopher's external tunnel API.
//
// Gopher (https://github.com/uclaacm/gopher) is ACM@UCLA's self-hosted reverse
// tunnel gateway — rathole + Caddy on a VPS. Nimbus uses Gopher's external
// API to expose VMs to the internet at provision time.
//
// Gopher models exposure in two layers:
//
//   - **Machines**: a registered host running the rathole client. Created
//     with `public_ssh: true` to flip on SSH exposure. The response carries
//     a one-shot bootstrap_url; running `curl <bootstrap_url> | sh` on the
//     VM links it to Gopher and the machine flips from `pending` to `active`.
//
//   - **Tunnels**: per-port exposures *on top of* an active machine. Used to
//     route additional services (HTTP, custom TCP) to specific ports inside
//     the VM. Provision-time SSH exposure does NOT use this surface — it
//     rides on the machine's `public_ssh` flag.
//
// All endpoints use Bearer-token auth via the Gopher API key. When the
// configured base URL is empty the constructor returns nil; callers do
// `if c == nil { skip }` for the tunnel-disabled code path.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Status values returned by Gopher. Machines transition pending → connected →
// failed; tunnels are active|failed (synchronous create).
const (
	StatusPending   = "pending"
	StatusConnected = "connected"
	StatusActive    = "active"
	StatusFailed    = "failed"
)

// Client wraps the Gopher external API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a Client. Returns (nil, nil) when baseURL is empty so callers
// can do `if c == nil { skip }` without distinguishing "not configured" from
// "construction failed". timeout caps per-request HTTP calls; 0 → 15s.
func New(baseURL, apiKey string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, nil
	}
	if apiKey == "" {
		return nil, errors.New("tunnel: GOPHER_API_KEY is required when GOPHER_API_URL is set")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("tunnel: invalid base URL %q: %w", baseURL, err)
	}
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// ── Machines ──────────────────────────────────────────────────────────────────

// Machine is the canonical Gopher object — a host that runs the rathole
// client. Field names match Gopher's JSON. BootstrapURL is set on creation
// (one-shot, used by `curl … | sh` to link the VM). Once Status flips to
// "connected", the machine's reverse tunnel is established; with
// PublicSSH=true, Gopher exposes SSH at the gateway. The exact response
// fields for host/port are inferred from the design — the live API today
// returns only {id, status, public_ssh, bootstrap_url, error, created_at},
// so callers must derive host:port from settings until Gopher exposes them.
type Machine struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PublicSSH     bool   `json:"public_ssh"`
	BootstrapURL  string `json:"bootstrap_url,omitempty"`
	PublicSSHHost string `json:"public_ssh_host,omitempty"`
	PublicSSHPort int    `json:"public_ssh_port,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// CreateMachineRequest is the body of POST /api/v1/machines.
type CreateMachineRequest struct {
	PublicSSH bool `json:"public_ssh,omitempty"`
}

// CreateMachine registers a new machine. With PublicSSH=true, Gopher
// allocates an SSH port at the gateway and returns a bootstrap_url that the
// VM must run to establish the rathole tunnel.
func (c *Client) CreateMachine(ctx context.Context, req CreateMachineRequest) (*Machine, error) {
	var out Machine
	if err := c.do(ctx, http.MethodPost, "/api/v1/machines", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMachine fetches one machine by ID. Used to poll status during the
// bootstrap window. Returns ErrNotFound on 404 (machine deleted).
func (c *Client) GetMachine(ctx context.Context, id string) (*Machine, error) {
	var out Machine
	if err := c.do(ctx, http.MethodGet, "/api/v1/machines/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteMachine tears down a machine and all its tunnels. 404 is treated as
// success (already gone) so retries are idempotent.
func (c *Client) DeleteMachine(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/machines/"+url.PathEscape(id), nil, nil)
	if err == nil {
		return nil
	}
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// ListMachines returns every machine known to Gopher. Admin-scoped — non-admin
// tokens may receive a filtered list. Surfaces the first page (default limit).
func (c *Client) ListMachines(ctx context.Context) ([]Machine, error) {
	var page paginatedMachines
	if err := c.do(ctx, http.MethodGet, "/api/v1/machines", nil, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// ── Tunnels ───────────────────────────────────────────────────────────────────

// Tunnel is an additional port exposure on an existing connected machine.
// Used for HTTP/custom services beyond SSH (which is exposed via the
// public_ssh flag on the Machine itself, not a tunnel record).
type Tunnel struct {
	ID         string `json:"id"`
	MachineID  string `json:"machine_id"`
	Status     string `json:"status,omitempty"`
	Subdomain  string `json:"subdomain,omitempty"`
	TargetIP   string `json:"target_ip,omitempty"`
	TargetPort int    `json:"target_port"`
	TunnelURL  string `json:"tunnel_url,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// CreateTunnelRequest is the body of POST /api/v1/tunnels. Subdomain is
// optional — Gopher derives one from the machine name when blank.
type CreateTunnelRequest struct {
	MachineID  string `json:"machine_id"`
	TargetPort int    `json:"target_port"`
	Subdomain  string `json:"subdomain,omitempty"`
}

// CreateTunnel adds a port exposure to an active machine. Returns an error
// when MachineID names a machine that doesn't exist or is still pending.
func (c *Client) CreateTunnel(ctx context.Context, req CreateTunnelRequest) (*Tunnel, error) {
	var out Tunnel
	if err := c.do(ctx, http.MethodPost, "/api/v1/tunnels", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTunnel removes a tunnel. 404 → idempotent success.
func (c *Client) DeleteTunnel(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/tunnels/"+url.PathEscape(id), nil, nil)
	if err == nil {
		return nil
	}
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// ListTunnels returns every tunnel known to Gopher. Admin-scoped.
func (c *Client) ListTunnels(ctx context.Context) ([]Tunnel, error) {
	var page paginatedTunnels
	if err := c.do(ctx, http.MethodGet, "/api/v1/tunnels", nil, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// ListTunnelsForMachine fetches every tunnel and filters down to those
// attached to the named machine. Gopher's external API doesn't expose a
// per-machine query parameter, so we filter client-side. The first page is
// adequate for the usual handful-of-tunnels-per-machine case; if a single
// machine ever sprouts >50 tunnels this would silently truncate.
func (c *Client) ListTunnelsForMachine(ctx context.Context, machineID string) ([]Tunnel, error) {
	all, err := c.ListTunnels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Tunnel, 0, len(all))
	for _, t := range all {
		if t.MachineID == machineID {
			out = append(out, t)
		}
	}
	return out, nil
}

// ── Internals ─────────────────────────────────────────────────────────────────

// httpError carries a non-2xx response so callers can branch on status code.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("tunnel: gopher returned HTTP %d: %s", e.Status, e.Body)
}

// envelope is Gopher's standard {success, data, error} JSON wrapper.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error,omitempty"`
}

type paginatedMachines struct {
	Items  []Machine `json:"items"`
	Limit  int       `json:"limit,omitempty"`
	Offset int       `json:"offset,omitempty"`
	Total  int       `json:"total,omitempty"`
}

type paginatedTunnels struct {
	Items  []Tunnel `json:"items"`
	Limit  int      `json:"limit,omitempty"`
	Offset int      `json:"offset,omitempty"`
	Total  int      `json:"total,omitempty"`
}

// do issues the HTTP request, decodes Gopher's {success, data, error}
// envelope, and surfaces server-supplied error messages on non-2xx.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("tunnel: marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("tunnel: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tunnel: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(resp.Body)

	var env envelope
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &env)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !env.Success && env.Error != "" {
			return fmt.Errorf("tunnel: %s %s: %s", method, path, env.Error)
		}
		if out == nil || len(env.Data) == 0 {
			return nil
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("tunnel: decode data for %s %s: %w (body=%s)",
				method, path, err, strings.TrimSpace(string(respBody)))
		}
		return nil
	}

	body4xx := env.Error
	if body4xx == "" {
		body4xx = strings.TrimSpace(string(respBody))
	}
	return &httpError{Status: resp.StatusCode, Body: body4xx}
}
