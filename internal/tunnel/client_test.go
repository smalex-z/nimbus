package tunnel_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nimbus/internal/tunnel"
)

// newMockGopher spins up an httptest server that mimics Gopher's standard
// {success, data, error} envelope. The handler closure inspects requests
// and produces canned responses per test.
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

// ── Machine endpoints ─────────────────────────────────────────────────────────

func TestCreateMachine_HappyPath_SendsBearerAndReturnsMachine(t *testing.T) {
	t.Parallel()
	var capturedAuth, capturedBody, capturedPath, capturedMethod string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeOK(t, w, tunnel.Machine{
			ID:           "m-123",
			Status:       tunnel.StatusPending,
			PublicSSH:    true,
			BootstrapURL: "https://router.example.com/bootstrap/abc",
		})
	})
	got, err := c.CreateMachine(context.Background(), tunnel.CreateMachineRequest{PublicSSH: true})
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if got.ID != "m-123" || got.BootstrapURL == "" {
		t.Errorf("decoded machine = %+v", got)
	}
	if capturedAuth != "Bearer test-key" {
		t.Errorf("Authorization header = %q", capturedAuth)
	}
	if capturedMethod != http.MethodPost || capturedPath != "/api/v1/machines" {
		t.Errorf("%s %s", capturedMethod, capturedPath)
	}
	if !strings.Contains(capturedBody, `"public_ssh":true`) {
		t.Errorf("request body should carry public_ssh:true; got %s", capturedBody)
	}
}

func TestCreateMachine_5xx_SurfacesEnvelopeError(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusInternalServerError, "database unreachable")
	})
	_, err := c.CreateMachine(context.Background(), tunnel.CreateMachineRequest{PublicSSH: true})
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if !strings.Contains(err.Error(), "database unreachable") {
		t.Errorf("error should surface envelope message, got %q", err.Error())
	}
}

func TestGetMachine_ReturnsMachine(t *testing.T) {
	t.Parallel()
	var capturedPath string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		writeOK(t, w, tunnel.Machine{
			ID: "m-123", Status: tunnel.StatusActive, PublicSSH: true,
			PublicSSHHost: "altsuite.co", PublicSSHPort: 43219,
		})
	})
	got, err := c.GetMachine(context.Background(), "m-123")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.Status != tunnel.StatusActive || got.PublicSSHPort != 43219 {
		t.Errorf("got %+v", got)
	}
	if capturedPath != "/api/v1/machines/m-123" {
		t.Errorf("path = %s", capturedPath)
	}
}

func TestGetMachine_404_ReturnsError(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, "machine not found")
	})
	_, err := c.GetMachine(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "machine not found") {
		t.Errorf("error should surface envelope message, got %q", err.Error())
	}
}

func TestDeleteMachine_404_IsIdempotent(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, "machine not found")
	})
	if err := c.DeleteMachine(context.Background(), "missing"); err != nil {
		t.Errorf("Delete on missing machine = %v, want nil", err)
	}
}

func TestDeleteMachine_204_OK(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteMachine(context.Background(), "m-1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	if capturedMethod != http.MethodDelete || capturedPath != "/api/v1/machines/m-1" {
		t.Errorf("%s %s", capturedMethod, capturedPath)
	}
}

func TestListMachines_UnwrapsPaginatedItems(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, map[string]any{
			"items": []tunnel.Machine{
				{ID: "a", Status: tunnel.StatusActive, PublicSSH: true},
				{ID: "b", Status: tunnel.StatusPending, PublicSSH: false},
			},
			"limit":  50,
			"offset": 0,
			"total":  2,
		})
	})
	got, err := c.ListMachines(context.Background())
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

// ── Tunnel endpoints (post-provision exposure of additional ports) ───────────

func TestCreateTunnel_HappyPath(t *testing.T) {
	t.Parallel()
	var capturedBody string
	_, c := newMockGopher(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeOK(t, w, tunnel.Tunnel{
			ID: "t-1", MachineID: "m-1", TargetPort: 8080,
			Status: tunnel.StatusActive,
			URL:    "https://something.altsuite.co",
		})
	})
	got, err := c.CreateTunnel(context.Background(), tunnel.CreateTunnelRequest{
		MachineID: "m-1", TargetPort: 8080,
	})
	if err != nil {
		t.Fatalf("CreateTunnel: %v", err)
	}
	if got.URL == "" || got.MachineID != "m-1" {
		t.Errorf("got %+v", got)
	}
	if !strings.Contains(capturedBody, `"machine_id":"m-1"`) ||
		!strings.Contains(capturedBody, `"target_port":8080`) {
		t.Errorf("request body = %s", capturedBody)
	}
}

func TestCreateTunnel_404_MachineNotActive(t *testing.T) {
	t.Parallel()
	_, c := newMockGopher(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, "machine not found")
	})
	_, err := c.CreateTunnel(context.Background(), tunnel.CreateTunnelRequest{
		MachineID: "ghost", TargetPort: 80,
	})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var he interface{ Error() string }
	if !errors.As(err, &he) || !strings.Contains(err.Error(), "machine not found") {
		t.Errorf("error should surface envelope message, got %q", err.Error())
	}
}
