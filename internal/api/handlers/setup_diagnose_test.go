package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"nimbus/internal/config"
	"nimbus/internal/proxmox"
)

func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://192.168.0.5:8006":  "192.168.0.5:8006",
		"https://192.168.0.5:8006/": "192.168.0.5:8006",
		"HTTPS://Host:8006":         "host:8006",
		"http://10.0.0.1:8006":      "10.0.0.1:8006",
		"  https://localhost:8006 ": "localhost:8006",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBindingDetail(t *testing.T) {
	t.Parallel()
	if d := bindingDetail(proxmox.BindingOK, false); d != "" {
		t.Errorf("ok state should have empty detail, got %q", d)
	}
	for _, st := range []proxmox.BindingState{proxmox.BindingUnauthorized, proxmox.BindingUnreachable} {
		for _, alt := range []bool{true, false} {
			if d := bindingDetail(st, alt); d == "" {
				t.Errorf("bindingDetail(%q, %v) returned empty detail", st, alt)
			}
		}
	}
	// The "your token works elsewhere" copy must differ from the "nobody
	// accepts it" copy — that distinction is the whole point of the probe.
	if bindingDetail(proxmox.BindingUnauthorized, true) == bindingDetail(proxmox.BindingUnauthorized, false) {
		t.Error("unauthorized detail should differ depending on whether alternatives exist")
	}
}

func TestProbeAlternatives(t *testing.T) {
	t.Parallel()
	// A node that accepts the token (answers /version) and one that 401s.
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":"8.2","release":"8.2.2"}}`))
	}))
	defer good.Close()
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()

	h := &Setup{cfg: &config.Config{ProxmoxTokenID: "root@pam!nimbus", ProxmoxTokenSecret: "secret"}}
	eps := []DiscoveredEndpoint{
		{URL: good.URL, IP: "1.1.1.1", NodeName: "good", Source: "scan"},
		{URL: bad.URL, IP: "2.2.2.2", NodeName: "bad", Source: "scan"},
	}

	// Configured host not in the list: the 401 node is dropped, only the
	// node that accepts the token is returned.
	got := h.probeAlternatives(context.Background(), eps, "https://9.9.9.9:8006")
	if len(got) != 1 || got[0].URL != good.URL {
		t.Fatalf("discrimination: want only %s, got %+v", good.URL, got)
	}

	// Configured host == the good node: it is skipped (we already know the
	// configured one), leaving no alternatives (bad still 401s).
	got = h.probeAlternatives(context.Background(), eps, good.URL)
	if len(got) != 0 {
		t.Fatalf("skip-configured: want none, got %+v", got)
	}
}
