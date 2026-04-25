package proxmox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/proxmox"
)

// newMockPVE spins up an httptest TLS server and returns it plus a Client that
// trusts its self-signed cert (via the same InsecureSkipVerify the production
// client uses).
//
// The handler closure can inspect the test's recorded request via the returned
// *http.Request channel for assertions.
func newMockPVE(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *proxmox.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	c := proxmox.New(srv.URL, "root@pam!nimbus", "test-secret-uuid", 5*time.Second)
	return srv, c
}

// writeEnvelope writes the standard Proxmox response shape: {"data": ...}
func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func TestClient_AuthHeader(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		captured.Store(r.Header.Get("Authorization"))
		writeEnvelope(w, []proxmox.Node{})
	})

	if _, err := c.GetNodes(context.Background()); err != nil {
		t.Fatalf("GetNodes: %v", err)
	}

	got := captured.Load().(string)
	want := "PVEAPIToken=root@pam!nimbus=test-secret-uuid"
	if got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

func TestClient_GetNodes(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		writeEnvelope(w, []proxmox.Node{
			{Name: "alpha", Status: "online", CPU: 0.42, MaxCPU: 8, Mem: 8 << 30, MaxMem: 16 << 30},
			{Name: "bravo", Status: "offline"},
		})
	})

	nodes, err := c.GetNodes(context.Background())
	if err != nil {
		t.Fatalf("GetNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	if nodes[0].Name != "alpha" || nodes[0].Status != "online" || nodes[0].MaxMem != 16<<30 {
		t.Errorf("alpha decoded wrong: %+v", nodes[0])
	}
}

// TestClient_SetCloudInit_FormEncoded is the highest-stakes test in this file.
// Cloud-init silently fails to inject SSH keys when the request body isn't
// form-encoded with the SSH key URL-escaped — a documented Proxmox API gotcha.
// This test asserts the wire format directly.
func TestClient_SetCloudInit_FormEncoded(t *testing.T) {
	t.Parallel()

	var capturedBody, capturedCT string
	var capturedMethod, capturedPath string

	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeEnvelope(w, nil)
	})

	cfg := proxmox.CloudInitConfig{
		CIUser:       "ubuntu",
		SSHKeys:      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK7g8x2Wq3nF9Lp2Mj4Yw1pXc5z6hQrV alex@laptop",
		IPConfig0:    "ip=192.168.0.142/24,gw=192.168.0.1",
		Nameserver:   "1.1.1.1 8.8.8.8",
		SearchDomain: "local",
	}
	if err := c.SetCloudInit(context.Background(), "node1", 200, cfg); err != nil {
		t.Fatalf("SetCloudInit: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/node1/qemu/200/config" {
		t.Errorf("path = %s", capturedPath)
	}
	if capturedCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %s, want application/x-www-form-urlencoded", capturedCT)
	}

	// Parse the form body and verify each field.
	parsed, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("captured body is not valid form-encoded: %v\nbody: %s", err, capturedBody)
	}
	if got := parsed.Get("sshkeys"); got != cfg.SSHKeys {
		t.Errorf("sshkeys roundtrip mismatch:\n got: %q\nwant: %q", got, cfg.SSHKeys)
	}
	if got := parsed.Get("ipconfig0"); got != cfg.IPConfig0 {
		t.Errorf("ipconfig0 = %q, want %q", got, cfg.IPConfig0)
	}
	if got := parsed.Get("ciuser"); got != "ubuntu" {
		t.Errorf("ciuser = %q", got)
	}

	// And specifically verify the wire bytes contain the URL-escaped form
	// (spaces as %20, slashes percent-encoded — defends against any future
	// encoder change that might silently produce raw spaces).
	if !strings.Contains(capturedBody, "sshkeys=ssh-ed25519+") &&
		!strings.Contains(capturedBody, "sshkeys=ssh-ed25519%20") {
		t.Errorf("expected sshkeys to be URL-escaped in body, got: %s", capturedBody)
	}
}

func TestClient_CloneVM_TargetParameter(t *testing.T) {
	t.Parallel()
	// gotcha #3: the clone endpoint must include `target=<node>` in the body.
	var capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeEnvelope(w, "UPID:node1:00001234:00ABCDEF::qmclone:200:root@pam:")
	})

	taskID, err := c.CloneVM(context.Background(), "source-node", "target-node", 9000, 200, "my-vm")
	if err != nil {
		t.Fatalf("CloneVM: %v", err)
	}
	if !strings.HasPrefix(taskID, "UPID:") {
		t.Errorf("taskID = %q, want UPID prefix", taskID)
	}

	parsed, _ := url.ParseQuery(capturedBody)
	if parsed.Get("target") != "target-node" {
		t.Errorf("target param missing or wrong: body=%q", capturedBody)
	}
	if parsed.Get("newid") != "200" {
		t.Errorf("newid wrong: body=%q", capturedBody)
	}
	if parsed.Get("name") != "my-vm" {
		t.Errorf("name wrong: body=%q", capturedBody)
	}
	if parsed.Get("full") != "1" {
		t.Errorf("full=1 missing: body=%q", capturedBody)
	}
}

func TestClient_TemplateExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		statusCode  int
		body        any
		wantPresent bool
		wantErr     bool
	}{
		{
			name:       "exists with cloud-init drive",
			statusCode: http.StatusOK,
			body: map[string]any{
				"name":  "ubuntu-template",
				"ide2":  "local-lvm:cloudinit,media=cdrom",
				"scsi0": "local-lvm:vm-9000-disk-0,size=10G",
			},
			wantPresent: true,
		},
		{
			name:       "exists but no cloud-init drive (silent failure trap)",
			statusCode: http.StatusOK,
			body: map[string]any{
				"name":  "ubuntu-no-ci",
				"scsi0": "local-lvm:vm-9000-disk-0,size=10G",
			},
			wantPresent: false,
		},
		{
			name:        "does not exist",
			statusCode:  http.StatusNotFound,
			wantPresent: false,
		},
		{
			// Proxmox quirk: missing VMID on a node returns 500, not 404.
			name:       "Proxmox 500 with 'does not exist' message",
			statusCode: http.StatusInternalServerError,
			body: map[string]any{
				"data":    nil,
				"message": "Configuration file 'nodes/hppve/qemu-server/9000.conf' does not exist\n",
			},
			wantPresent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.statusCode == http.StatusOK {
					writeEnvelope(w, tt.body)
					return
				}
				w.WriteHeader(tt.statusCode)
				if tt.body != nil {
					_ = json.NewEncoder(w).Encode(tt.body)
				}
			})
			got, err := c.TemplateExists(context.Background(), "node1", 9000)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.wantPresent {
				t.Errorf("got %v, want %v", got, tt.wantPresent)
			}
		})
	}
}

func TestClient_WaitForTask(t *testing.T) {
	t.Parallel()

	t.Run("success after a few polls", func(t *testing.T) {
		var calls atomic.Int32
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n < 3 {
				writeEnvelope(w, map[string]string{"status": "running"})
			} else {
				writeEnvelope(w, map[string]string{"status": "stopped", "exitstatus": "OK"})
			}
		})
		err := c.WaitForTask(context.Background(), "node1", "UPID:foo", 10*time.Millisecond)
		if err != nil {
			t.Errorf("WaitForTask: %v", err)
		}
		if calls.Load() < 3 {
			t.Errorf("expected at least 3 polls, got %d", calls.Load())
		}
	})

	t.Run("non-OK exit status returns error", func(t *testing.T) {
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, map[string]string{"status": "stopped", "exitstatus": "command failed"})
		})
		err := c.WaitForTask(context.Background(), "node1", "UPID:bar", 10*time.Millisecond)
		if err == nil {
			t.Errorf("expected error for failed task")
		}
	})

	t.Run("ctx cancellation aborts polling", func(t *testing.T) {
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, map[string]string{"status": "running"})
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := c.WaitForTask(ctx, "node1", "UPID:slow", 10*time.Millisecond)
		if err == nil {
			t.Errorf("expected context error, got nil")
		}
	})
}

func TestClient_HTTPError(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":"permission denied"}`))
	})
	_, err := c.GetNodes(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	var httpErr *proxmox.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusUnauthorized {
		t.Errorf("status = %d", httpErr.Status)
	}
}

func TestClient_GetAgentInterfaces(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{
			"result": []map[string]any{
				{
					"name": "lo",
					"ip-addresses": []map[string]any{
						{"ip-address-type": "ipv4", "ip-address": "127.0.0.1"},
					},
				},
				{
					"name": "ens18",
					"ip-addresses": []map[string]any{
						{"ip-address-type": "ipv4", "ip-address": "192.168.0.142", "prefix": 24},
						{"ip-address-type": "ipv6", "ip-address": "fe80::1234"},
					},
				},
			},
		})
	})

	ifaces, err := c.GetAgentInterfaces(context.Background(), "node1", 200)
	if err != nil {
		t.Fatalf("GetAgentInterfaces: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("got %d interfaces, want 2", len(ifaces))
	}
	if ifaces[1].Name != "ens18" || len(ifaces[1].IPAddresses) != 2 {
		t.Errorf("ens18 decoded wrong: %+v", ifaces[1])
	}
	if ifaces[1].IPAddresses[0].IPAddress != "192.168.0.142" {
		t.Errorf("first IP wrong: %+v", ifaces[1].IPAddresses[0])
	}
}

func TestClient_NextVMID(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		// Proxmox returns the ID as a string in the data field.
		writeEnvelope(w, "201")
	})
	id, err := c.NextVMID(context.Background())
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if id != 201 {
		t.Errorf("got %d, want 201", id)
	}
}
