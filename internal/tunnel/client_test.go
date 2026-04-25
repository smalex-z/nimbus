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

// newMockGopher spins up an httptest server and returns it plus a Client
// wired to it. The handler closure inspects requests and produces canned
// responses per test.
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tunnel.Tunnel{
			ID:           "t-123",
			Subdomain:    "my-app",
			Status:       tunnel.StatusPending,
			BootstrapURL: "https://router.example.com/bootstrap/abc",
		})
	})
	got, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "my-app", TargetIP: "10.0.0.42", TargetPort: 22,
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
	// Body must contain the form fields as JSON.
	if !contains(capturedBody, `"subdomain":"my-app"`) ||
		!contains(capturedBody, `"target_ip":"10.0.0.42"`) ||
		!contains(capturedBody, `"target_port":22`) {
		t.Errorf("request body = %s", capturedBody)
	}
}

func TestCreate_409_ReturnsErrSubdomainTaken(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"subdomain in use"}`))
	})
	_, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "taken", TargetIP: "10.0.0.42", TargetPort: 22,
	})
	if !errors.Is(err, tunnel.ErrSubdomainTaken) {
		t.Errorf("err = %v, want ErrSubdomainTaken", err)
	}
}

func TestCreate_500_ReturnsHTTPError(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oh no`))
	})
	_, err := c.Create(context.Background(), tunnel.CreateRequest{
		Subdomain: "x", TargetIP: "10.0.0.42", TargetPort: 22,
	})
	if err == nil || errors.Is(err, tunnel.ErrSubdomainTaken) {
		t.Errorf("err = %v, want generic non-2xx", err)
	}
}

func TestGet_ReturnsTunnel(t *testing.T) {
	t.Parallel()
	var capturedPath string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tunnel.Tunnel{
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

func TestDelete_404_IsIdempotent(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
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
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Delete(context.Background(), "t-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if capturedMethod != http.MethodDelete || capturedPath != "/api/v1/tunnels/t-1" {
		t.Errorf("%s %s", capturedMethod, capturedPath)
	}
}

func TestList_ReturnsTunnels(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]tunnel.Tunnel{
			{ID: "a", Subdomain: "one", Status: tunnel.StatusActive},
			{ID: "b", Subdomain: "two", Status: tunnel.StatusPending},
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
