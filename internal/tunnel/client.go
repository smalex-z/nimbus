// Package tunnel is a focused client for Gopher's external tunnel API.
//
// Gopher (https://github.com/uclaacm/gopher) is ACM@UCLA's self-hosted reverse
// tunnel gateway — rathole + Caddy on a VPS. It exposes services running on
// internal hosts at public HTTPS subdomains. Nimbus uses Gopher's external
// API to register a tunnel for each VM that asks for one; the actual tunnel
// is established by a one-line bootstrap script run on the VM.
//
// All endpoints use Bearer-token auth via the GOPHER_API_KEY env var. When the
// configured base URL is empty the constructor returns nil and callers are
// expected to skip tunnel work entirely — see provision.Service for the
// "tunnel-disabled" code path.
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
	"regexp"
	"strings"
	"time"
)

// subdomainRE constrains subdomains to a DNS-safe label. Mirrors RFC 1123
// single-label hostnames (lowercase). Validating client-side is best-effort —
// Gopher is the source of truth for "available", but we want to reject obvious
// garbage before round-tripping.
var subdomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateSubdomain returns nil for a syntactically valid subdomain, or a
// descriptive error. Callers should still expect Gopher to reject duplicates.
func ValidateSubdomain(s string) error {
	if !subdomainRE.MatchString(s) {
		return fmt.Errorf("subdomain must be 1–63 chars, lowercase letters/digits/hyphens, no leading or trailing hyphen")
	}
	return nil
}

// Tunnel statuses returned by Gopher's Get endpoint.
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusFailed  = "failed"
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

// CreateRequest is the body of POST /api/v1/tunnels.
type CreateRequest struct {
	Subdomain  string `json:"subdomain"`
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`
}

// Tunnel is the canonical Gopher tunnel object. Field names match Gopher's
// JSON. URL is empty until Status flips to "active".
type Tunnel struct {
	ID           string `json:"id"`
	Subdomain    string `json:"subdomain"`
	Status       string `json:"status"`
	URL          string `json:"url,omitempty"`
	BootstrapURL string `json:"bootstrap_url,omitempty"`
	TargetIP     string `json:"target_ip,omitempty"`
	TargetPort   int    `json:"target_port,omitempty"`
}

// ErrSubdomainTaken is returned by Create when Gopher reports the subdomain
// is already registered (HTTP 409). Callers map this to a fail-fast
// validation error.
var ErrSubdomainTaken = errors.New("tunnel: subdomain already taken")

// Create registers a new tunnel. On 409 returns ErrSubdomainTaken.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*Tunnel, error) {
	var out Tunnel
	if err := c.do(ctx, http.MethodPost, "/api/v1/tunnels", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get fetches one tunnel by ID. Used to poll status during the bootstrap
// window.
func (c *Client) Get(ctx context.Context, id string) (*Tunnel, error) {
	var out Tunnel
	if err := c.do(ctx, http.MethodGet, "/api/v1/tunnels/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete tears down a tunnel and releases its subdomain. 404 is treated as
// success (already gone) so retries are idempotent.
func (c *Client) Delete(ctx context.Context, id string) error {
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

// List returns every tunnel known to Gopher. Admin-scoped — non-admin tokens
// may receive a filtered list.
func (c *Client) List(ctx context.Context) ([]Tunnel, error) {
	var out []Tunnel
	if err := c.do(ctx, http.MethodGet, "/api/v1/tunnels", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// httpError carries a non-2xx response so callers can branch on status code.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("tunnel: gopher returned HTTP %d: %s", e.Status, e.Body)
}

// do is the shared request body. body and out are optional; pass nil to skip
// either side. Non-2xx responses are returned as *httpError.
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tunnel: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(respBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("tunnel: decode %s %s: %w", method, path, err)
		}
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrSubdomainTaken
	}
	return &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
}
