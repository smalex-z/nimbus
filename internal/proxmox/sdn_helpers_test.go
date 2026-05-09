package proxmox_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"nimbus/internal/proxmox"
)

// TestFormatStandaloneZoneName_Shape pins the format invariants:
// 8 chars, "s" prefix, lowercase hex tail, deterministic.
func TestFormatStandaloneZoneName_Shape(t *testing.T) {
	t.Parallel()
	got := proxmox.FormatStandaloneZoneName("vm-uuid-1")
	if len(got) != 8 {
		t.Errorf("len = %d, want 8 (Proxmox 8-char zone-id cap)", len(got))
	}
	if got[0] != 's' {
		t.Errorf("prefix = %c, want s", got[0])
	}
	for i := 1; i < len(got); i++ {
		c := got[i]
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !ok {
			t.Errorf("char %d = %c, want lowercase hex", i, c)
		}
	}
	// Determinism: same input → same output.
	if got2 := proxmox.FormatStandaloneZoneName("vm-uuid-1"); got2 != got {
		t.Errorf("non-deterministic: %q vs %q", got, got2)
	}
	// Different inputs → different outputs (trivial collision check
	// for two specific strings — exhaustive testing isn't useful here).
	other := proxmox.FormatStandaloneZoneName("vm-uuid-2")
	if other == got {
		t.Errorf("collision on trivially-different inputs: both %q", got)
	}
}

// TestFormatVPCZoneName_PrefixDistinguishesFromStandalone asserts the
// prefix byte is what differentiates VPC from Standalone names — so
// PVE zones land in distinct namespaces visually + via prefix-match
// debugging.
func TestFormatVPCZoneName_PrefixDistinguishesFromStandalone(t *testing.T) {
	t.Parallel()
	vpc := proxmox.FormatVPCZoneName("some-id")
	standalone := proxmox.FormatStandaloneZoneName("some-id")
	if vpc[0] != 'v' {
		t.Errorf("vpc prefix = %c, want v", vpc[0])
	}
	if vpc == standalone {
		t.Errorf("vpc and standalone names should differ at minimum in prefix: vpc=%q standalone=%q", vpc, standalone)
	}
	if vpc[1:] != standalone[1:] {
		t.Errorf("hash bodies should match for the same id (only prefix differs): vpc=%q standalone=%q", vpc, standalone)
	}
}

// TestResolveOnlinePeerIPs covers the cluster-status walk: filters
// out cluster-row entries (Type != "node"), skips offline nodes,
// drops empty IPs. Comma-joins what's left.
func TestResolveOnlinePeerIPs(t *testing.T) {
	t.Parallel()
	_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, []map[string]any{
			{"type": "cluster", "name": "demo"},
			{"type": "node", "name": "alpha", "ip": "10.0.0.1", "online": 1},
			{"type": "node", "name": "bravo", "ip": "10.0.0.2", "online": 0}, // skipped (offline)
			{"type": "node", "name": "charlie", "ip": "", "online": 1},       // skipped (empty IP)
			{"type": "node", "name": "delta", "ip": "10.0.0.4", "online": 1},
		})
	})

	got, err := proxmox.ResolveOnlinePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("ResolveOnlinePeerIPs: %v", err)
	}
	want := "10.0.0.1,10.0.0.4"
	if got != want {
		t.Errorf("peers = %q, want %q", got, want)
	}
}

// TestZoneExists covers the three branches: exists, not found,
// transport error. Used by collision-retry loops in zone-name
// allocation.
func TestZoneExists(t *testing.T) {
	t.Parallel()
	t.Run("exists", func(t *testing.T) {
		t.Parallel()
		_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, proxmox.SDNZone{Zone: "x", Type: "simple"})
		})
		exists, err := proxmox.ZoneExists(context.Background(), c, "x")
		if err != nil || !exists {
			t.Errorf("exists=%t err=%v, want exists=true err=nil", exists, err)
		}
	})
	t.Run("not-found", func(t *testing.T) {
		t.Parallel()
		_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		exists, err := proxmox.ZoneExists(context.Background(), c, "x")
		if err != nil || exists {
			t.Errorf("exists=%t err=%v, want exists=false err=nil", exists, err)
		}
	})
	t.Run("not-found-via-500-quirk", func(t *testing.T) {
		t.Parallel()
		// Proxmox returns 500 (not 404) for missing SDN zones with
		// `does not exist` in the body — GetSDNZone normalizes this
		// to ErrNotFound, so ZoneExists should report false.
		_, c := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":null,"message":"sdn 'x' does not exist\n"}`))
		})
		exists, err := proxmox.ZoneExists(context.Background(), c, "x")
		if err != nil || exists {
			t.Errorf("exists=%t err=%v, want exists=false err=nil", exists, err)
		}
	})
}

// TestLXCNetSpec covers the comma-joined kv format Proxmox expects
// on netN config keys, including empty-field skipping.
func TestLXCNetSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec proxmox.LXCNetSpec
		want string
	}{
		{
			name: "full",
			spec: proxmox.LXCNetSpec{Name: "eth0", Bridge: "vmbr0", IP: "192.168.1.10/24", Gw: "192.168.1.1", Hwaddr: "BC:24:11:00:00:01"},
			want: "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1,hwaddr=BC:24:11:00:00:01",
		},
		{
			name: "no-gw-no-hwaddr",
			spec: proxmox.LXCNetSpec{Name: "eth1", Bridge: "v0123abc", IP: "10.42.0.1/16"},
			want: "name=eth1,bridge=v0123abc,ip=10.42.0.1/16",
		},
		{
			name: "dhcp",
			spec: proxmox.LXCNetSpec{Name: "eth0", Bridge: "vmbr0", IP: "dhcp"},
			want: "name=eth0,bridge=vmbr0,ip=dhcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.String()
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			// Sanity: every field should appear at most once.
			parts := strings.Split(got, ",")
			seen := map[string]bool{}
			for _, p := range parts {
				k := strings.SplitN(p, "=", 2)[0]
				if seen[k] {
					t.Errorf("duplicate key %q in %q", k, got)
				}
				seen[k] = true
			}
		})
	}
}
