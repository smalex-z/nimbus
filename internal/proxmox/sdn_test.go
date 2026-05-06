package proxmox_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"nimbus/internal/proxmox"
)

// TestClient_GetSDNZone_Found exercises the happy path of zone reads.
// The path is the operative thing — Proxmox's SDN URL space is
// idiosyncratic and easy to typo.
func TestClient_GetSDNZone_Found(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/sdn/zones/nimbus" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		writeEnvelope(w, proxmox.SDNZone{Zone: "nimbus", Type: "simple"})
	})

	zone, err := c.GetSDNZone(context.Background(), "nimbus")
	if err != nil {
		t.Fatalf("GetSDNZone: %v", err)
	}
	if zone.Zone != "nimbus" || zone.Type != "simple" {
		t.Errorf("decoded wrong: %+v", zone)
	}
}

// TestClient_GetSDNZone_NotFound asserts a missing zone surfaces as
// proxmox.ErrNotFound — vnetmgr.Bootstrap dispatches on this to
// decide "needs creating".
func TestClient_GetSDNZone_NotFound(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetSDNZone(context.Background(), "missing")
	if !errors.Is(err, proxmox.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %T %v", err, err)
	}
}

// TestClient_CreateSDNZone_FormEncoded asserts the zone-create wire
// shape Proxmox expects: form-encoded POST with type + zone params.
// Same hard rule as cloud-init — JSON body silently fails.
func TestClient_CreateSDNZone_FormEncoded(t *testing.T) {
	t.Parallel()
	var capturedCT, capturedBody string
	var capturedMethod, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, nil)
	})

	if err := c.CreateSDNZone(context.Background(), proxmox.SDNZone{
		Zone: "nimbus",
		Type: "simple",
	}); err != nil {
		t.Fatalf("CreateSDNZone: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", capturedMethod)
	}
	if capturedPath != "/api2/json/cluster/sdn/zones" {
		t.Errorf("path = %s, want /cluster/sdn/zones", capturedPath)
	}
	if capturedCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-urlencoded", capturedCT)
	}
	vals, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	if vals.Get("zone") != "nimbus" {
		t.Errorf("zone param = %q, want nimbus", vals.Get("zone"))
	}
	if vals.Get("type") != "simple" {
		t.Errorf("type param = %q, want simple", vals.Get("type"))
	}
}

// TestClient_CreateSDNZone_VXLANPeers asserts that VXLAN-specific
// fields land on the wire only when set — simple zones don't carry
// them. Cheap defense against accidentally polluting future zones.
func TestClient_CreateSDNZone_VXLANPeers(t *testing.T) {
	t.Parallel()
	var capturedBody string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, nil)
	})

	if err := c.CreateSDNZone(context.Background(), proxmox.SDNZone{
		Zone:  "vx",
		Type:  "vxlan",
		Peers: "10.0.0.1,10.0.0.2",
	}); err != nil {
		t.Fatalf("CreateSDNZone: %v", err)
	}
	vals, _ := url.ParseQuery(capturedBody)
	if vals.Get("peers") != "10.0.0.1,10.0.0.2" {
		t.Errorf("peers = %q, want 10.0.0.1,10.0.0.2", vals.Get("peers"))
	}
}

// TestClient_DeleteSDNSubnet_PVEEncoding asserts the subnet-delete
// path encodes the CIDR with Proxmox's `/`-→-`-` quirk (10.42.0.0/24
// → 10.42.0.0-24). Documented in the Proxmox SDN docs; easy to miss.
func TestClient_DeleteSDNSubnet_PVEEncoding(t *testing.T) {
	t.Parallel()
	var capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		writeEnvelope(w, nil)
	})

	if err := c.DeleteSDNSubnet(context.Background(), "nbu1", "10.42.0.0/24"); err != nil {
		t.Fatalf("DeleteSDNSubnet: %v", err)
	}
	want := "/api2/json/cluster/sdn/vnets/nbu1/subnets/10.42.0.0-24"
	if capturedPath != want {
		t.Errorf("path = %q, want %q", capturedPath, want)
	}
}

// TestClient_ApplySDN_PUT asserts the apply call hits the right verb
// + path. Without this, Create/Delete operations stay pending and
// nothing reaches running config — the load-bearing call.
func TestClient_ApplySDN_PUT(t *testing.T) {
	t.Parallel()
	var capturedMethod, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		writeEnvelope(w, nil)
	})

	if err := c.ApplySDN(context.Background()); err != nil {
		t.Fatalf("ApplySDN: %v", err)
	}
	if capturedMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", capturedMethod)
	}
	if capturedPath != "/api2/json/cluster/sdn" {
		t.Errorf("path = %q, want /cluster/sdn", capturedPath)
	}
}

// TestClient_SetVMNetwork_BridgeOnly covers the post-clone bridge
// override path: net0=virtio,bridge=<vnet> with no MAC injected.
func TestClient_SetVMNetwork_BridgeOnly(t *testing.T) {
	t.Parallel()
	var capturedBody, capturedPath string
	_, c := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		writeEnvelope(w, nil)
	})

	if err := c.SetVMNetwork(context.Background(), "alpha", 200, "net0", "nbu1", ""); err != nil {
		t.Fatalf("SetVMNetwork: %v", err)
	}
	if !strings.HasSuffix(capturedPath, "/qemu/200/config") {
		t.Errorf("path = %q, expected to end with /qemu/200/config", capturedPath)
	}
	vals, _ := url.ParseQuery(capturedBody)
	got := vals.Get("net0")
	if got != "virtio,bridge=nbu1" {
		t.Errorf("net0 = %q, want virtio,bridge=nbu1", got)
	}
}

// TestClient_SetVMNetwork_RejectEmptyBridge asserts the defensive
// guard fires — without a bridge the call is meaningless and would
// cause Proxmox to reject net0= with an unhelpful error.
func TestClient_SetVMNetwork_RejectEmptyBridge(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeEnvelope(w, nil)
	})

	err := c.SetVMNetwork(context.Background(), "alpha", 200, "net0", "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls.Load() != 0 {
		t.Errorf("server called %d times — guard should reject before HTTP", calls.Load())
	}
}
