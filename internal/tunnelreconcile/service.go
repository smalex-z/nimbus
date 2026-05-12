// Package tunnelreconcile keeps db.VM.TunnelURL aligned with Gopher's actual
// state. Each VM row that has a tunnel_id is checked against Gopher on every
// cycle and the URL is updated when Gopher's view diverges from what we
// stored at provision time — covers the cases where the gateway-side tunnel
// got renamed, replaced (takeover-on-409), or manually edited on the Gopher
// UI. The Gopher side is treated as the source of truth; Nimbus's local DB
// is converged to match (same direction-of-truth as ippool's reconciler).
//
// Scope: only TunnelURL on db.VM. Machine state is per-VM and not yet
// surfaced in the UI, so we don't bother reconciling MachineID or tunnel
// existence (a tunnel that vanished on Gopher stays "stale URL" in our DB
// until the operator deletes the VM or manually clears it).
package tunnelreconcile

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/tunnel"
)

// Service holds the live tunnel client + the DB handle. The client is
// optional — when nil, every reconcile cycle is a no-op (matches the
// "Gopher not configured" pattern used by the provision service).
type Service struct {
	gdb     *gorm.DB
	tunnels *tunnel.Client
}

func New(gdb *gorm.DB) *Service {
	return &Service{gdb: gdb}
}

// SetTunnelClient installs (or replaces) the Gopher client. Mirrors the
// applier pattern used by provision.Service so Settings.SaveGopher can
// push the freshly-built client without a restart.
func (s *Service) SetTunnelClient(t *tunnel.Client) {
	s.tunnels = t
}

// Reconcile runs one full pass: list tunnels from Gopher once, then update
// every db.VM row whose stored TunnelURL diverges from Gopher's. Returns
// (checked, updated, error). Designed to be safe to call concurrently with
// provision-time writes — the WHERE clause on the UPDATE pins it to the
// specific VM row, and SQLite single-writer serialises with everything else.
func (s *Service) Reconcile(ctx context.Context) (checked, updated int, err error) {
	client := s.tunnels
	if client == nil {
		return 0, 0, nil
	}

	// One cluster-wide list per cycle — cheaper than N GetTunnel calls.
	tunnels, err := client.ListTunnels(ctx)
	if err != nil {
		return 0, 0, err
	}
	byID := make(map[string]tunnel.Tunnel, len(tunnels))
	for _, t := range tunnels {
		byID[t.ID] = t
	}

	// Find every VM with a tunnel_id we know about. Soft-deleted rows are
	// excluded by gorm's default scope so we don't keep updating tombstones.
	var rows []db.VM
	if err := s.gdb.WithContext(ctx).
		Where("tunnel_id != ''").
		Find(&rows).Error; err != nil {
		return 0, 0, err
	}

	for _, vm := range rows {
		checked++
		gt, ok := byID[vm.TunnelID]
		if !ok {
			// Gopher no longer knows this tunnel — could be: deleted,
			// orphaned by a takeover, or moved beyond ListTunnels'
			// pagination window. Skip silently; the operator can clean
			// up via the VM detail page if it's permanent.
			continue
		}
		// Compare against TunnelURL (the only field we persist + display).
		// Empty TunnelURL on Gopher's side means the tunnel hasn't been
		// activated yet — don't overwrite a populated local URL with empty.
		if gt.TunnelURL == "" || gt.TunnelURL == vm.TunnelURL {
			continue
		}
		if err := s.gdb.WithContext(ctx).
			Model(&db.VM{}).
			Where("id = ?", vm.ID).
			Update("tunnel_url", gt.TunnelURL).Error; err != nil {
			log.Printf("tunnel reconcile: vm %d: %v", vm.ID, err)
			continue
		}
		log.Printf("tunnel reconcile: vm %d tunnel_url %q → %q", vm.ID, vm.TunnelURL, gt.TunnelURL)
		updated++
	}
	return checked, updated, nil
}

// Run drives Reconcile on a ticker. interval <= 0 disables the loop.
// Errors are logged and the loop continues — a transient Gopher outage
// shouldn't take the reconciler down.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Printf("tunnel reconcile loop disabled (interval=%v)", interval)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runCtx, cancel := context.WithTimeout(ctx, interval)
			checked, updated, err := s.Reconcile(runCtx)
			cancel()
			if err != nil {
				log.Printf("tunnel reconcile error: %v", err)
				continue
			}
			if updated > 0 {
				log.Printf("tunnel reconcile: checked=%d updated=%d", checked, updated)
			}
		}
	}
}
