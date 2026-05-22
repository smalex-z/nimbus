package proxmox_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"nimbus/internal/proxmox"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want proxmox.BindingState
	}{
		{"nil is ok", nil, proxmox.BindingOK},
		{"401 is unauthorized", &proxmox.HTTPError{Status: http.StatusUnauthorized, Method: "GET", Path: "/version"}, proxmox.BindingUnauthorized},
		{"403 is unauthorized", &proxmox.HTTPError{Status: http.StatusForbidden, Method: "GET", Path: "/version"}, proxmox.BindingUnauthorized},
		{"500 is unreachable", &proxmox.HTTPError{Status: http.StatusInternalServerError, Method: "GET", Path: "/version"}, proxmox.BindingUnreachable},
		{"404 is unreachable", &proxmox.HTTPError{Status: http.StatusNotFound}, proxmox.BindingUnreachable},
		{"transport error is unreachable", errors.New("dial tcp: connection refused"), proxmox.BindingUnreachable},
		// Wrapped *HTTPError is still classified via errors.As.
		{"wrapped 401 is unauthorized", fmt.Errorf("probe: %w", &proxmox.HTTPError{Status: http.StatusUnauthorized}), proxmox.BindingUnauthorized},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := proxmox.Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
