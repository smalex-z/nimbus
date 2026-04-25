package tunnel_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nimbus/internal/tunnel"
)

// newMockGopher spins up an httptest server that mimics Gopher's standard
// {success, data, error} envelope. The handler closure inspects requests and
// produces canned responses per test.
func newMockGopher(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *tunnel.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := tunnel.New(srv.URL, "test-key", 2*time.Second)
	if err != nil {
		t.Fatalf("tunnel.New: %v", err)
	}
	return srv, c
}

// writeOK writes Gopher's {"success":true,"data":<v>} envelope.
func writeOK(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"data":    v,
	}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}

// writeErr writes Gopher's {"success":false,"error":msg} envelope at status.
func writeErr(t *testing.T, w http.ResponseWriter, status int, msg string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"error":   msg,
	}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
}

func TestNew_EmptyBaseURL_ReturnsNil(t *testing.T) {
	t.Parallel()
	c, err := tunnel.New("", "anything", 0)
	if err != nil {
		t.Errorf("err = %v, want nil for empty base URL", err)
	}
	if c != nil {
		t.Errorf("expected nil client when not configured, got %v", c)
	}
}

func TestNew_MissingKey_Errors(t *testing.T) {
	t.Parallel()
	if _, err := tunnel.New("https://example.com", "", 0); err == nil {
		t.Error("expected error when API key missing but URL set")
	}
}

func TestValidateSubdomain(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a", "my-app", "abc123", "x1y2"} {
		if err := tunnel.ValidateSubdomain(ok); err != nil {
			t.Errorf("ValidateSubdomain(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-leading", "trailing-", "UPPER", "with_underscore", "has.dot", "way-too-long-by-far-this-string-is-many-many-many-many-many-many-chars"} {
		if err := tunnel.ValidateSubdomain(bad); err == nil {
			t.Errorf("ValidateSubdomain(%q) accepted, want error", bad)
		}
	}
}

func TestCreate_HappyPath_SendsBearerAndReturnsTunnel(t *testing.T) {
	t.Parallel()
	var capturedAuth, capturedBody, capturedPath, capturedMethod string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeOK(t, w, tunnel.Tunnel{
			ID:           "t-123",
			Subdomain:    "my-app",
			Status:       tunnel.StatusPending,
			BootstrapURL: "https://router.example.com/bootstrap/abc",
		})
	})
	got, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "my-app", TargetIP: "10.0.0.42", TargetPort: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "t-123" || got.BootstrapURL == "" {
		t.Errorf("decoded tunnel = %+v", got)
	}
	if capturedAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q", capturedAuth)
	}
	if capturedMethod != http.MethodPost || capturedPath != "/api/v1/tunnels" {
		t.Errorf("%s %s", capturedMethod, capturedPath)
	}
	if !contains(capturedBody, `"subdomain":"my-app"`) ||
		!contains(capturedBody, `"target_ip":"10.0.0.42"`) ||
		!contains(capturedBody, `"target_port":80`) {
		t.Errorf("request body = %s", capturedBody)
	}
}

func TestCreate_409_ReturnsErrSubdomainTaken(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusConflict, "subdomain in use")
	})
	_, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "taken", TargetIP: "10.0.0.42", TargetPort: 80,
	})
	if !errors.Is(err, tunnel.ErrSubdomainTaken) {
		t.Errorf("err = %v, want ErrSubdomainTaken", err)
	}
}

func TestCreate_500_SurfacesEnvelopeError(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusInternalServerError, "database unreachable")
	})
	_, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "x", TargetIP: "10.0.0.42", TargetPort: 80,
	})
	if err == nil || errors.Is(err, tunnel.ErrSubdomainTaken) {
		t.Errorf("err = %v, want generic non-2xx", err)
	}
	if msg := err.Error(); !contains(msg, "database unreachable") {
		t.Errorf("error should surface envelope message, got %q", msg)
	}
}

func TestGet_ReturnsTunnel(t *testing.T) {
	t.Parallel()
	var capturedPath string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		writeOK(t, w, tunnel.Tunnel{
			ID: "t-123", Subdomain: "my-app", Status: tunnel.StatusActive,
			URL: "https://my-app.example.com",
		})
	})
	got, err := c.Get(context.Background(), "t-123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tunnel.StatusActive || got.URL != "https://my-app.example.com" {
		t.Errorf("got %+v", got)
	}
	if capturedPath != "/api/v1/tunnels/t-123" {
		t.Errorf("path = %s", capturedPath)
	}
}

func TestGet_404_ReturnsError(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, "tunnel not found")
	})
	_, err := c.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !contains(err.Error(), "tunnel not found") {
		t.Errorf("error should surface envelope message, got %q", err.Error())
	}
}

func TestDelete_404_IsIdempotent(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, "tunnel not found")
	})
	if err := c.Delete(context.Background(), "missing"); err != nil {
		t.Errorf("Delete on missing tunnel = %v, want nil", err)
	}
}

func TestDelete_2xx_OK(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		writeOK(t, w, nil) // success envelope with empty data
	})
	if err := c.Delete(context.Background(), "t-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if capturedMethod != http.MethodDelete || capturedPath != "/api/v1/tunnels/t-1" {
		t.Errorf("%s %s", capturedMethod, capturedPath)
	}
}

func TestList_UnwrapsPaginatedItems(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		// Mirrors Gopher's actual shape: data.items + pagination metadata.
		writeOK(t, w, map[string]any{
			"items": []tunnel.Tunnel{
				{ID: "a", Subdomain: "one", Status: tunnel.StatusActive},
				{ID: "b", Subdomain: "two", Status: tunnel.StatusPending},
			},
			"limit":  50,
			"offset": 0,
			"total":  2,
		})
	})
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestList_EmptyItems(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, map[string]any{
			"items": []tunnel.Tunnel{},
			"limit": 50, "offset": 0, "total": 0,
		})
	})
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %d items", len(got))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
