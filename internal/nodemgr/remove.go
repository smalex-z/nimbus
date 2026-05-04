package nodemgr

import (
	"context"
	"fmt"

	"nimbus/internal/db"
)

// Remove tears the node down: validates state, asks Proxmox to delete it
// from the cluster (pvecm delnode equivalent), removes the local row.
//
// Hard gates, in order:
//  1. Lock state must be "drained" — anything else is a bug in the SPA
//     (Remove button stays hidden until then).
//  2. The node must not be Nimbus's own host — pulling it bricks the API
//     mid-call.
//  3. There must be zero managed VMs on the node (re-checked here even
//     though Drain produced "drained" — defends against a manual VM
//     creation between drain completion and remove click).
//
// On success the row is hard-deleted; AutoMigrate doesn't recreate it
// since Proxmox no longer reports the node.
func (s *Service) Remove(ctx context.Context, name string) error {
	if s.IsDrainInFlight(name) {
		return ErrDrainInFlight
	}
	if name == s.cfg.SelfHostName && s.cfg.SelfHostName != "" {
		return ErrSelfHosted
	}
	row, err := s.loadOrCreate(ctx, name)
	if err != nil {
		return fmt.Errorf("load node: %w", err)
	}
	if lockOrNone(row.LockState) != "drained" {
		return fmt.Errorf("%w: lock state is %s", ErrNotDrained, lockOrNone(row.LockState))
	}
	managed, err := s.managedVMsOnNode(ctx, name)
	if err != nil {
		return fmt.Errorf("recount managed vms: %w", err)
	}
	if len(managed) > 0 {
		return fmt.Errorf("%d managed vm(s) reappeared on this node — re-drain before removing", len(managed))
	}
	if err := s.px.DeleteNode(ctx, name); err != nil {
		return fmt.Errorf("proxmox delete node: %w", err)
	}
	if err := s.db.WithContext(ctx).
		Where("name = ?", name).
		Delete(&db.Node{}).Error; err != nil {
		// Proxmox already removed the node — this is a stale-DB problem,
		// not a hard failure. Log via the returned error so callers can
		// surface it; the next reconcile pass will prune the row.
		return fmt.Errorf("delete db row: %w", err)
	}
	return nil
}
