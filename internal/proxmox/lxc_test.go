package proxmox_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"nimbus/internal/proxmox"
)

// TestClient_CreateLXC_Wire pins the wire shape Proxmox's
// /nodes/{n}/lxc endpoint expects. ostemplate, vmid, hostname are
// required; rootfs is "<storage>:<gb>"; netN are comma-joined kv
// strings. If we ever drift on any of these, PVE returns generic
// 400s without naming the bad field.
func TestClient_CreateLXC_Wire(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath, capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, "UPID:n:00:00:00:00:lxc:create:ok")
	})

	taskID, err := c.CreateLXC(context.Background(), "alpha", proxmox.LXCCreateOpts{
		VMID:         200,
		OSTemplate:   "local:vztmpl/alpine-3.20-default_20240908_amd64.tar.xz",
		Hostname:     "nbu-gw-1",
		Storage:      "local-lvm",
		RootDiskGiB:  1,
		MemoryMiB:    128,
		Cores:        1,
		Net0:         "name=eth0,bridge=vmbr0,ip=192.168.1.200/24,gw=192.168.1.1",
		Net1:         "name=eth1,bridge=v0123abc,ip=10.42.0.1/16",
		Unprivileged: true,
		Features:     "keyctl=1",
	})
	if err != nil {
		t.Fatalf("CreateLXC: %v", err)
	}
	if taskID == "" {
		t.Errorf("expected non-empty task id")
	}
	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/nodes/alpha/lxc" {
		t.Errorf("path = %q", capturedPath)
	}
	vals, _ := url.ParseQuery(capturedBody)
	checks := map[string]string{
		"vmid":         "200",
		"ostemplate":   "local:vztmpl/alpine-3.20-default_20240908_amd64.tar.xz",
		"hostname":     "nbu-gw-1",
		"rootfs":       "local-lvm:1",
		"memory":       "128",
		"cores":        "1",
		"net0":         "name=eth0,bridge=vmbr0,ip=192.168.1.200/24,gw=192.168.1.1",
		"net1":         "name=eth1,bridge=v0123abc,ip=10.42.0.1/16",
		"unprivileged": "1",
		"features":     "keyctl=1",
	}
	for k, want := range checks {
		if got := vals.Get(k); got != want {
			t.Errorf("body[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestClient_CreateLXC_RejectsEmpty asserts the defensive guards
// catch missing required fields before the HTTP call. Proxmox would
// reject these with generic 400s; failing fast saves a round trip.
func TestClient_CreateLXC_RejectsEmpty(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeEnvelope(w, nil)
	})
	cases := []proxmox.LXCCreateOpts{
		{OSTemplate: "x", Hostname: "h"}, // VMID=0
		{VMID: 1, Hostname: "h"},         // empty OSTemplate
		{VMID: 1, OSTemplate: "x"},       // empty Hostname
	}
	for i, opts := range cases {
		if _, err := c.CreateLXC(context.Background(), "n", opts); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times — guards should reject before HTTP", calls.Load())
	}
}

// TestClient_LXCLifecycle_Paths covers Start/Stop/Destroy paths.
// Each is a one-liner under the hood, but the path strings are
// load-bearing — typo in /lxc/N/status/start vs /qemu/N/status/start
// would silently route Proxmox to the wrong endpoint.
func TestClient_LXCLifecycle_Paths(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedMethod string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		writeEnvelope(w, "UPID:n:00:00:00:00:lxc:op:ok")
	})

	cases := []struct {
		name       string
		fn         func() (string, error)
		wantPath   string
		wantMethod string
	}{
		{
			name:       "start",
			fn:         func() (string, error) { return c.StartLXC(context.Background(), "alpha", 200) },
			wantPath:   "/api2/json/nodes/alpha/lxc/200/status/start",
			wantMethod: http.MethodPost,
		},
		{
			name:       "stop",
			fn:         func() (string, error) { return c.StopLXC(context.Background(), "alpha", 200) },
			wantPath:   "/api2/json/nodes/alpha/lxc/200/status/stop",
			wantMethod: http.MethodPost,
		},
		{
			name:       "destroy",
			fn:         func() (string, error) { return c.DestroyLXC(context.Background(), "alpha", 200) },
			wantPath:   "/api2/json/nodes/alpha/lxc/200",
			wantMethod: http.MethodDelete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.fn(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if capturedPath != tc.wantPath {
				t.Errorf("%s path = %q, want %q", tc.name, capturedPath, tc.wantPath)
			}
			if capturedMethod != tc.wantMethod {
				t.Errorf("%s method = %s, want %s", tc.name, capturedMethod, tc.wantMethod)
			}
		})
	}
}

// TestClient_DestroyLXC_PurgeFlag asserts the destroy call sets
// purge=1 + destroy-unreferenced-disks=1 — without these PVE
// leaves stale HA/replication entries and orphan rootfs volumes.
func TestClient_DestroyLXC_PurgeFlag(t *testing.T) {
	t.Parallel()
	var capturedQuery string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		writeEnvelope(w, "UPID:n:00:00:00:00:lxc:destroy:ok")
	})
	if _, err := c.DestroyLXC(context.Background(), "alpha", 200); err != nil {
		t.Fatalf("DestroyLXC: %v", err)
	}
	vals, _ := url.ParseQuery(capturedQuery)
	if vals.Get("purge") != "1" {
		t.Errorf("purge = %q, want 1", vals.Get("purge"))
	}
	if vals.Get("destroy-unreferenced-disks") != "1" {
		t.Errorf("destroy-unreferenced-disks = %q, want 1", vals.Get("destroy-unreferenced-disks"))
	}
}

// TestClient_LXCExec_WireAndDecode pins the multi-arg `command=...`
// form-encoding shape (same trick as agent/exec) and the response
// shape (out-data / err-data / exitcode).
func TestClient_LXCExec_WireAndDecode(t *testing.T) {
	t.Parallel()
	var capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, map[string]any{
			"out-data": "hello\n",
			"err-data": "",
			"exitcode": 0,
		})
	})

	res, err := c.LXCExec(context.Background(), "alpha", 200, []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("LXCExec: %v", err)
	}
	if res.OutData != "hello\n" || res.ExitCode != 0 {
		t.Errorf("res = %+v", res)
	}
	vals, _ := url.ParseQuery(capturedBody)
	cmds := vals["command"]
	want := []string{"echo", "hello"}
	if len(cmds) != len(want) {
		t.Fatalf("command values = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Errorf("command[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
}

// TestClient_LXCExecShell wraps shell scripts in `sh -c` with
// single-quote escaping. Used for multi-line bootstrap scripts in
// the gateway-LXC flow.
func TestClient_LXCExecShell(t *testing.T) {
	t.Parallel()
	var capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, map[string]any{"exitcode": 0})
	})
	if _, err := c.LXCExecShell(context.Background(), "alpha", 200, "echo hi && ls /"); err != nil {
		t.Fatalf("LXCExecShell: %v", err)
	}
	vals, _ := url.ParseQuery(capturedBody)
	cmds := vals["command"]
	if len(cmds) != 3 || cmds[0] != "sh" || cmds[1] != "-c" {
		t.Fatalf("expected [sh -c <script>], got %v", cmds)
	}
	if !strings.Contains(cmds[2], "echo hi") {
		t.Errorf("script body lost its content: %q", cmds[2])
	}
}

// TestClient_SetLXCConfig pins the path + method (PUT not POST).
func TestClient_SetLXCConfig(t *testing.T) {
	t.Parallel()
	var capturedPath, capturedMethod, capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, nil)
	})
	params := url.Values{}
	params.Set("memory", "256")
	params.Set("net0", "name=eth0,bridge=vmbr0")
	if err := c.SetLXCConfig(context.Background(), "alpha", 200, params); err != nil {
		t.Fatalf("SetLXCConfig: %v", err)
	}
	if capturedPath != "/api2/json/nodes/alpha/lxc/200/config" {
		t.Errorf("path = %q", capturedPath)
	}
	if capturedMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", capturedMethod)
	}
	vals, _ := url.ParseQuery(capturedBody)
	if vals.Get("memory") != "256" {
		t.Errorf("memory = %q", vals.Get("memory"))
	}
}
