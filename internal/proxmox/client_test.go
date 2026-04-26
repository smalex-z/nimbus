package proxmox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		CPU:          "x86-64-v3",
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
	// sshkeys is double-encoded on the wire (Proxmox quirk — see
	// SetCloudInit docstring). After form decoding once we should see the
	// URL-encoded form of the original key, not the original key itself.
	wantEncoded := strings.ReplaceAll(url.QueryEscape(cfg.SSHKeys), "+", "%20")
	if got := parsed.Get("sshkeys"); got != wantEncoded {
		t.Errorf("sshkeys after form-decode:\n got: %q\nwant: %q (URL-encoded form of the key)", got, wantEncoded)
	}
	if got := parsed.Get("ipconfig0"); got != cfg.IPConfig0 {
		t.Errorf("ipconfig0 = %q, want %q", got, cfg.IPConfig0)
	}
	if got := parsed.Get("ciuser"); got != "ubuntu" {
		t.Errorf("ciuser = %q", got)
	}
	if got := parsed.Get("cpu"); got != "x86-64-v3" {
		t.Errorf("cpu = %q, want x86-64-v3", got)
	}

	// Verify the wire bytes contain the DOUBLE-encoded sshkeys — the form
	// layer encodes the percent signs again (% → %25). Defends against any
	// future change that might bypass the URL pre-encoding step.
	if !strings.Contains(capturedBody, "sshkeys=ssh-ed25519%2520") {
		t.Errorf("expected sshkeys to be DOUBLE-URL-escaped on wire (sshkeys=ssh-ed25519%%2520...), got: %s", capturedBody)
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
		_, _ = w.Write([]byte(`{"errors"
	"fmt":"permission denied"}`))
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

func TestClient_NextVMIDFrom(t *testing.T) {
	t.Parallel()
	// Cluster has VMs at 100, 101, 9000, 9002. Lowest free at or above 9000
	// must be 9001 — not 102 (which is below the floor) or 9003 (which would
	// imply we counted from the top).
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("type"); got != "vm" {
			t.Errorf("type query = %q, want vm", got)
		}
		writeEnvelope(w, []map[string]any{
			{"vmid": 100}, {"vmid": 101}, {"vmid": 9000}, {"vmid": 9002},
		})
	})
	id, err := c.NextVMIDFrom(context.Background(), 9000)
	if err != nil {
		t.Fatalf("NextVMIDFrom: %v", err)
	}
	if id != 9001 {
		t.Errorf("got %d, want 9001 (lowest free at or above floor)", id)
	}
}

func TestClient_NextVMIDFrom_EmptyCluster(t *testing.T) {
	t.Parallel()
	// Fresh cluster — should return the floor itself.
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, []map[string]any{})
	})
	id, err := c.NextVMIDFrom(context.Background(), 9000)
	if err != nil {
		t.Fatalf("NextVMIDFrom: %v", err)
	}
	if id != 9000 {
		t.Errorf("got %d, want 9000 (floor on empty cluster)", id)
	}
}

func TestClient_GetStorages(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/hppve/storage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		writeEnvelope(w, []proxmox.Storage{
			{Storage: "local", Type: "dir", Content: "backup,iso,vztmpl", Enabled: 1, Active: 1},
			{Storage: "local-lvm", Type: "lvmthin", Content: "images,rootdir", Enabled: 1, Active: 1},
		})
	})
	out, err := c.GetStorages(context.Background(), "hppve")
	if err != nil {
		t.Fatalf("GetStorages: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].Storage != "local" || out[0].Type != "dir" {
		t.Errorf("first storage decoded wrong: %+v", out[0])
	}
}

func TestClient_StorageHasFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		filename string
		want     bool
	}{
		{
			name:     "file exists",
			filename: "ubuntu-24.04-server-cloudimg-amd64.img",
			want:     true,
		},
		{
			name:     "file missing",
			filename: "ubuntu-22.04-server-cloudimg-amd64.img",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api2/json/nodes/hppve/storage/local/content" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if r.URL.Query().Get("content") != "iso" {
					t.Errorf("missing content=iso query param: %s", r.URL.RawQuery)
				}
				writeEnvelope(w, []proxmox.StorageContentItem{
					{Volid: "local:iso/ubuntu-24.04-server-cloudimg-amd64.img", Format: "raw"},
					{Volid: "local:iso/some-other.iso", Format: "iso"},
				})
			})
			got, err := c.StorageHasFile(context.Background(), "hppve", "local", "iso", tt.filename)
			if err != nil {
				t.Fatalf("StorageHasFile: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClient_DownloadStorageURL(t *testing.T) {
	t.Parallel()
	var capturedBody, capturedPath, capturedCT string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeEnvelope(w, "UPID:hppve:00001234::download:test.img:root@pam!nimbus:")
	})

	taskID, err := c.DownloadStorageURL(context.Background(), "hppve", "local", "import",
		"https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img",
		"ubuntu-24.04-server-cloudimg-amd64.img")
	if err != nil {
		t.Fatalf("DownloadStorageURL: %v", err)
	}
	if !strings.HasPrefix(taskID, "UPID:") {
		t.Errorf("taskID = %q, want UPID prefix", taskID)
	}

	if capturedPath != "/api2/json/nodes/hppve/storage/local/download-url" {
		t.Errorf("path = %s", capturedPath)
	}
	if capturedCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %s", capturedCT)
	}

	parsed, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	if got := parsed.Get("url"); !strings.HasPrefix(got, "https://cloud-images.ubuntu.com/") {
		t.Errorf("url param wrong: %q", got)
	}
	if parsed.Get("content") != "import" {
		t.Errorf("content = %q, want import", parsed.Get("content"))
	}
	if parsed.Get("filename") != "ubuntu-24.04-server-cloudimg-amd64.img" {
		t.Errorf("filename wrong: %q", parsed.Get("filename"))
	}
}

func TestClient_CreateVMWithImport(t *testing.T) {
	t.Parallel()
	var capturedBody, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeEnvelope(w, "UPID:hppve:00005678::qmcreate:9000:root@pam!nimbus:")
	})

	opts := proxmox.CreateVMOpts{
		Name:         "ubuntu-24-template",
		Memory:       1024,
		Cores:        1,
		DiskStorage:  "local-lvm",
		ImagePath:    "local:iso/ubuntu-24.04-server-cloudimg-amd64.img",
		SerialOnly:   true,
		AgentEnabled: true,
	}
	taskID, err := c.CreateVMWithImport(context.Background(), "hppve", 9000, opts)
	if err != nil {
		t.Fatalf("CreateVMWithImport: %v", err)
	}
	if !strings.HasPrefix(taskID, "UPID:") {
		t.Errorf("taskID = %q", taskID)
	}
	if capturedPath != "/api2/json/nodes/hppve/qemu" {
		t.Errorf("path = %s", capturedPath)
	}

	parsed, _ := url.ParseQuery(capturedBody)
	if parsed.Get("vmid") != "9000" {
		t.Errorf("vmid = %q", parsed.Get("vmid"))
	}
	if parsed.Get("name") != "ubuntu-24-template" {
		t.Errorf("name = %q", parsed.Get("name"))
	}
	// The critical scsi0 wire format — Proxmox magic for "import this image".
	// MUST use a volid (storage:iso/file.img), not a raw filesystem path,
	// because API tokens are denied "arbitrary filesystem paths".
	wantScsi := "local-lvm:0,import-from=local:iso/ubuntu-24.04-server-cloudimg-amd64.img"
	if parsed.Get("scsi0") != wantScsi {
		t.Errorf("scsi0 = %q\nwant: %q", parsed.Get("scsi0"), wantScsi)
	}
	if parsed.Get("serial0") != "socket" {
		t.Errorf("serial0 = %q, want socket (cloud images need it)", parsed.Get("serial0"))
	}
	if parsed.Get("vga") != "serial0" {
		t.Errorf("vga = %q, want serial0", parsed.Get("vga"))
	}
	if parsed.Get("agent") != "enabled=1" {
		t.Errorf("agent = %q", parsed.Get("agent"))
	}
	if parsed.Get("net0") != "virtio,bridge=vmbr0" {
		t.Errorf("net0 = %q", parsed.Get("net0"))
	}
}

func TestClient_SetCloudInitDrive(t *testing.T) {
	t.Parallel()
	var capturedBody, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		writeEnvelope(w, nil)
	})

	if err := c.SetCloudInitDrive(context.Background(), "hppve", 9000, "local-lvm"); err != nil {
		t.Fatalf("SetCloudInitDrive: %v", err)
	}
	if capturedPath != "/api2/json/nodes/hppve/qemu/9000/config" {
		t.Errorf("path = %s", capturedPath)
	}
	parsed, _ := url.ParseQuery(capturedBody)
	if parsed.Get("ide2") != "local-lvm:cloudinit" {
		t.Errorf("ide2 = %q, want local-lvm:cloudinit", parsed.Get("ide2"))
	}
}

func TestClient_ConvertToTemplate(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedMethod string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		writeEnvelope(w, nil)
	})

	if err := c.ConvertToTemplate(context.Background(), "hppve", 9000); err != nil {
		t.Fatalf("ConvertToTemplate: %v", err)
	}
	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/hppve/qemu/9000/template" {
		t.Errorf("path = %s", capturedPath)
	}
}
func TestParseIPConfig0(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		wantIP string
		wantOK bool
	}{
		{"empty", "", "", false},
		{"ipv4 with cidr and gw", "ip=192.168.0.142/24,gw=192.168.0.1", "192.168.0.142", true},
		{"ipv4 bare", "ip=10.0.0.5", "10.0.0.5", true},
		{"ipv4 with cidr no gw", "ip=10.0.0.5/24", "10.0.0.5", true},
		{"dhcp with auto gw", "ip=dhcp,gw=auto", "", false},
		{"dhcp only", "ip=dhcp", "", false},
		{"ipv6 with prefix", "ip=2001:db8::1/64,gw6=fe80::1", "2001:db8::1", true},
		{"malformed no equals", "ipconfig", "", false},
		{"ip value not parseable", "ip=not-an-ip", "", false},
		{"only gateway present", "gw=192.168.1.1", "", false},
		{"ip key after others", "name=frodo,ip=10.0.0.42/24", "10.0.0.42", true},
		{"whitespace tolerated", " ip = 10.0.0.7/24 , gw=10.0.0.1", "10.0.0.7", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := proxmox.ParseIPConfig0(tc.in)
			if got != tc.wantIP || ok != tc.wantOK {
				t.Errorf("ParseIPConfig0(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.wantIP, tc.wantOK)
			}
		})
	}
}

func TestClient_GetVMConfig(t *testing.T) {
	t.Parallel()

	t.Run("returns raw map on success", func(t *testing.T) {
		t.Parallel()
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, map[string]any{
				"name":      "vm-200",
				"ipconfig0": "ip=10.0.0.42/24,gw=10.0.0.1",
				"scsi0":     "local-lvm:vm-200-disk-0,size=10G",
			})
		})
		cfg, err := c.GetVMConfig(context.Background(), "node1", 200)
		if err != nil {
			t.Fatalf("GetVMConfig: %v", err)
		}
		if cfg["ipconfig0"] != "ip=10.0.0.42/24,gw=10.0.0.1" {
			t.Errorf("ipconfig0 = %v", cfg["ipconfig0"])
		}
	})

	t.Run("404 returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		_, err := c.GetVMConfig(context.Background(), "node1", 999)
		if !errors.Is(err, proxmox.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("Proxmox 500 'does not exist' normalizes to ErrNotFound", func(t *testing.T) {
		t.Parallel()
		_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"data":null,"message":"Configuration file 'nodes/x/qemu-server/999.conf' does not exist\n"}`))
		})
		_, err := c.GetVMConfig(context.Background(), "node1", 999)
		if !errors.Is(err, proxmox.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestClient_ListClusterIPs builds a small mock cluster with two online nodes
// and one offline node, with a mix of running/stopped/template VMs and
// static/dhcp/missing ipconfig0 values, and asserts that ListClusterIPs returns
// exactly the static-IP claims.
func TestClient_ListClusterIPs(t *testing.T) {
	t.Parallel()

	// Per-node fixtures: node -> vmid -> { listVMs entry, configMap }
	type vmFixture struct {
		listVM proxmox.VMStatus
		config map[string]any
	}
	fixtures := map[string]map[int]vmFixture{
		"alpha": {
			200: {
				listVM: proxmox.VMStatus{VMID: 200, Name: "static-vm", Status: "running"},
				config: map[string]any{"ipconfig0": "ip=10.0.0.10/24,gw=10.0.0.1"},
			},
			201: {
				listVM: proxmox.VMStatus{VMID: 201, Name: "dhcp-vm", Status: "running"},
				config: map[string]any{"ipconfig0": "ip=dhcp"},
			},
			9000: {
				// Templates must be skipped.
				listVM: proxmox.VMStatus{VMID: 9000, Name: "ubuntu-template", Status: "stopped", Template: 1},
				config: map[string]any{"ipconfig0": "ip=192.168.99.99/24"},
			},
		},
		"bravo": {
			300: {
				listVM: proxmox.VMStatus{VMID: 300, Name: "stopped-vm", Status: "stopped"},
				// stopped VMs still hold their IP allocation; reconciler should see them
				config: map[string]any{"ipconfig0": "ip=10.0.0.20/24"},
			},
			301: {
				listVM: proxmox.VMStatus{VMID: 301, Name: "no-ipconfig", Status: "running"},
				config: map[string]any{}, // no ipconfig0 at all
			},
		},
	}

	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/nodes":
			writeEnvelope(w, []proxmox.Node{
				{Name: "alpha", Status: "online"},
				{Name: "bravo", Status: "online"},
				{Name: "ghost", Status: "offline"},
			})
		case strings.HasSuffix(r.URL.Path, "/qemu") && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/"):
			node := strings.Split(r.URL.Path, "/")[4]
			vms := make([]proxmox.VMStatus, 0)
			for _, fx := range fixtures[node] {
				vms = append(vms, fx.listVM)
			}
			writeEnvelope(w, vms)
		case strings.HasSuffix(r.URL.Path, "/lxc") && strings.HasPrefix(r.URL.Path, "/api2/json/nodes/"):
			// Empty LXC list — these fixtures only cover QEMU VMs.
			writeEnvelope(w, []proxmox.LXCStatus{})
		case strings.HasSuffix(r.URL.Path, "/config"):
			parts := strings.Split(r.URL.Path, "/")
			node := parts[4]
			vmid := 0
			_, _ = fmt.Sscanf(parts[6], "%d", &vmid)
			if fx, ok := fixtures[node][vmid]; ok {
				writeEnvelope(w, fx.config)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})

	got, err := c.ListClusterIPs(context.Background())
	if err != nil {
		t.Fatalf("ListClusterIPs: %v", err)
	}
	// Expected: static-vm (alpha/200/10.0.0.10) + stopped-vm (bravo/300/10.0.0.20)
	// Skipped: dhcp-vm (dhcp), ubuntu-template (template), no-ipconfig (missing), ghost (offline)
	want := map[string]proxmox.ClusterIP{
		"10.0.0.10": {IP: "10.0.0.10", VMID: 200, Node: "alpha", Hostname: "static-vm", Source: "ipconfig0", RawConfig: "ip=10.0.0.10/24,gw=10.0.0.1"},
		"10.0.0.20": {IP: "10.0.0.20", VMID: 300, Node: "bravo", Hostname: "stopped-vm", Source: "ipconfig0", RawConfig: "ip=10.0.0.20/24"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cluster IPs, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		w, ok := want[c.IP]
		if !ok {
			t.Errorf("unexpected ClusterIP %+v", c)
			continue
		}
		if c != w {
			t.Errorf("ClusterIP for %s = %+v, want %+v", c.IP, c, w)
		}
	}
}

// TestClient_ListClusterIPs_PartialNodeFailure ensures one node failing does
// not blackhole the whole walk: callers receive both partial results and a
// joined error so they can decide whether to use them.
func TestClient_ListClusterIPs_PartialNodeFailure(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes":
			writeEnvelope(w, []proxmox.Node{
				{Name: "good", Status: "online"},
				{Name: "bad", Status: "online"},
			})
		case "/api2/json/nodes/good/qemu":
			writeEnvelope(w, []proxmox.VMStatus{{VMID: 200, Name: "ok-vm", Status: "running"}})
		case "/api2/json/nodes/good/qemu/200/config":
			writeEnvelope(w, map[string]any{"ipconfig0": "ip=10.0.0.10/24"})
		case "/api2/json/nodes/good/lxc":
			writeEnvelope(w, []proxmox.LXCStatus{})
		case "/api2/json/nodes/bad/qemu":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":"node down"}`))
		case "/api2/json/nodes/bad/lxc":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":"node down"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, err := c.ListClusterIPs(context.Background())
	if err == nil {
		t.Errorf("expected joined error from bad node")
	}
	if len(got) != 1 || got[0].IP != "10.0.0.10" {
		t.Errorf("expected partial result containing 10.0.0.10, got %+v", got)
	}
}

// TestClient_StopVM verifies the wire shape of a stop request: POST to
// /nodes/{n}/qemu/{vmid}/status/stop, task UPID returned via the standard
// envelope.
func TestClient_StopVM(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		writeEnvelope(w, "UPID:node1:001:stop:42")
	})

	taskID, err := c.StopVM(context.Background(), "node1", 42)
	if err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/node1/qemu/42/status/stop" {
		t.Errorf("path = %s", capturedPath)
	}
	if taskID != "UPID:node1:001:stop:42" {
		t.Errorf("taskID = %q", taskID)
	}
}

// TestClient_DeleteVM verifies the wire shape of a destroy request: DELETE
// to /nodes/{n}/qemu/{vmid} with purge=1 + destroy-unreferenced-disks=1 in
// the query string. Asserting these is critical — silently dropping either
// flag leaves orphan disks in storage that the IP pool can't see.
func TestClient_DeleteVM(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath, capturedQuery string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		writeEnvelope(w, "UPID:node1:002:qmdestroy:42")
	})

	taskID, err := c.DeleteVM(context.Background(), "node1", 42)
	if err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/node1/qemu/42" {
		t.Errorf("path = %s", capturedPath)
	}
	q, _ := url.ParseQuery(capturedQuery)
	if q.Get("purge") != "1" {
		t.Errorf("purge = %q, want 1", q.Get("purge"))
	}
	if q.Get("destroy-unreferenced-disks") != "1" {
		t.Errorf("destroy-unreferenced-disks = %q, want 1", q.Get("destroy-unreferenced-disks"))
	}
	if taskID != "UPID:node1:002:qmdestroy:42" {
		t.Errorf("taskID = %q", taskID)
	}
}
