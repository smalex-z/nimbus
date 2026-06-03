package reconciler

import (
	"context"
	"log"
	"time"
)

// Start launches a background goroutine that calls Reconcile on the
// given interval. Returns immediately; the goroutine exits when ctx
// is cancelled. interval <= 0 means no background loop (caller can
// still trigger via Reconcile).
//
// The first tick fires after `interval`, not immediately, so a Nimbus
// restart doesn't generate a thundering-herd PVE API call during boot.
// Operators who want an at-startup reconcile can call Reconcile()
// directly in main.go before Start.
func (r *Reconciler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Printf("reconciler: background loop disabled (interval=%s)", interval)
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		log.Printf("reconciler: background loop starting (interval=%s)", interval)
		for {
			select {
			case <-ctx.Done():
				log.Printf("reconciler: background loop exiting (ctx done)")
				return
			case <-t.C:
				cycleCtx, cancel := context.WithTimeout(ctx, interval)
				if _, err := r.Reconcile(cycleCtx); err != nil {
					log.Printf("reconciler: cycle: %v", err)
				}
				cancel()
			}
		}
	}()
}

// ReconcileVM runs a targeted reconcile for one Proxmox VMID, used
// after a provision/delete/migrate where waiting for the next
// background tick would leave a known-stale state visible to the UI.
//
// Implementation note: the divergence detector is set-based — it
// builds the full PVE-vs-DB diff before deciding. Doing that for a
// single VMID would need the full enrichment walk anyway (other VMs
// might also have changed in the meantime), so we just delegate to
// Reconcile. The "targeted" semantic the spec calls for is about
// timing (run NOW rather than on next tick), not scope.
//
// vmid is currently unused — kept in the signature so call sites
// document which VMID triggered the run, and so future per-VM fast
// paths can read it without breaking the API.
func (r *Reconciler) ReconcileVM(ctx context.Context, vmid int) (Report, error) {
	rep, err := r.Reconcile(ctx)
	if err != nil {
		log.Printf("reconciler: targeted reconcile after vmid=%d: %v", vmid, err)
	}
	return rep, err
}

// ReconcileVMByID is the report-less variant that satisfies the
// provision.PostOpReconciler interface. provision doesn't care about
// the report — it just wants the cycle kicked off — so the wrapper
// drops the Report and propagates the error.
func (r *Reconciler) ReconcileVMByID(ctx context.Context, vmid int) error {
	_, err := r.ReconcileVM(ctx, vmid)
	return err
}
