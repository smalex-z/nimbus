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

func boolPtr(b bool) *bool { return &b }

func TestSetCloudInit_CIUpgrade(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *bool
		want string // "" means the param must be absent
	}{
		// Absent is NOT neutral — Proxmox's own default is "upgrade" —
		// so a caller that wants a fast boot must send 0 explicitly.
		{"nil leaves the flag untouched", nil, ""},
		{"false disables the first-boot upgrade", boolPtr(false), "0"},
		{"true opts in", boolPtr(true), "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got atomic.Value
			_, cl := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				parsed, _ := url.ParseQuery(string(body))
				got.Store(parsed.Get("ciupgrade"))
				writeEnvelope(w, nil)
			})

			err := cl.SetCloudInit(context.Background(), "node1", 100, proxmox.CloudInitConfig{
				CIUser:     "ubuntu",
				AptUpgrade: c.in,
			})
			if err != nil {
				t.Fatalf("SetCloudInit: %v", err)
			}
			if v := got.Load().(string); v != c.want {
				t.Errorf("ciupgrade = %q, want %q", v, c.want)
			}
		})
	}
}

// PVE < 8.1 has no ciupgrade. Rather than failing every provision, the
// client retries once without it.
func TestSetCloudInit_CIUpgradeUnsupportedFallsBack(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	var sawUpgradeOnRetry atomic.Bool
	_, cl := newMockPVE(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		parsed, _ := url.ParseQuery(string(body))
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":{"ciupgrade":"property is not defined in schema and the schema does not allow additional properties"},"data":null}`))
			return
		}
		sawUpgradeOnRetry.Store(parsed.Get("ciupgrade") != "")
		// The rest of the config must survive the retry.
		if parsed.Get("ciuser") != "ubuntu" {
			t.Errorf("retry dropped ciuser: %q", parsed.Get("ciuser"))
		}
		writeEnvelope(w, nil)
	})

	err := cl.SetCloudInit(context.Background(), "node1", 100, proxmox.CloudInitConfig{
		CIUser:     "ubuntu",
		AptUpgrade: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("SetCloudInit should fall back on an unsupported param, got: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("expected exactly one retry, got %d attempts", attempts.Load())
	}
	if sawUpgradeOnRetry.Load() {
		t.Error("retry still carried ciupgrade")
	}
}

// A bad *value* for a param that DOES exist must stay an error — the
// fallback is for old schemas, not for masking real rejections.
func TestSetCloudInit_CIUpgradeRealErrorNotSwallowed(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	_, cl := newMockPVE(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":{"ciupgrade":"type check ('boolean') failed - got 'banana'"},"data":null}`))
	})

	err := cl.SetCloudInit(context.Background(), "node1", 100, proxmox.CloudInitConfig{
		AptUpgrade: boolPtr(true),
	})
	if err == nil {
		t.Fatal("expected the type-check rejection to surface")
	}
	if !strings.Contains(err.Error(), "type check") {
		t.Errorf("unexpected error: %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("must not retry a real rejection, got %d attempts", attempts.Load())
	}
}
