package nodemgr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
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
	// Destroy template VMs before pvecm delnode — once the node is out
	// of the cluster, its qemu API is unreachable from the rest of the
	// cluster and the templates would orphan as ghost VMIDs.
	// Best-effort: a Proxmox failure here is logged but doesn't block
	// the removal — the operator can clean up stragglers manually, and
	// the dangling node_templates rows are cleared regardless so a
	// future re-add re-bootstraps cleanly.
	s.destroyNodeTemplates(ctx, name)

	// Probe whether the node is still in the cluster. The operator
	// may have already run `pvecm delnode` manually (e.g. on a retry
	// after the API-token 403); if so we skip the API call and go
	// straight to local DB cleanup. GetNodes failure leaves us in
	// the cautious "still present, try delete" branch.
	stillInCluster := true
	if nodes, lerr := s.px.GetNodes(ctx); lerr == nil {
		stillInCluster = false
		for _, n := range nodes {
			if n.Name == name {
				stillInCluster = true
				break
			}
		}
	}
	if stillInCluster {
		if err := s.px.DeleteNode(ctx, name); err != nil {
			// Proxmox gates `pvecm delnode` (DELETE /cluster/config/nodes)
			// to user == "root@pam" exactly — API tokens fail with 403
			// even when root-owned. Surface a structured error so the
			// handler can show the operator the manual command.
			var httpErr *proxmox.HTTPError
			if errors.As(err, &httpErr) && httpErr.Status == http.StatusForbidden {
				return &ManualDelnodeRequiredError{Node: name}
			}
			return fmt.Errorf("proxmox delete node: %w", err)
		}
	} else {
		log.Printf("nodemgr.Remove(%s): node already absent from cluster (manually delnoded?); proceeding to local cleanup", name)
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

// destroyNodeTemplates removes every template VM Nimbus provisioned onto
// `name` and clears the node_templates rows. Best-effort — individual
// DestroyVM failures are logged and the loop continues, so a single
// misbehaving template VM doesn't strand the whole removal flow.
func (s *Service) destroyNodeTemplates(ctx context.Context, name string) {
	var rows []db.NodeTemplate
	if err := s.db.WithContext(ctx).
		Where("node = ?", name).
		Find(&rows).Error; err != nil {
		log.Printf("nodemgr.Remove(%s): list templates: %v", name, err)
		return
	}
	for _, r := range rows {
		taskID, err := s.px.DestroyVM(ctx, name, r.VMID)
		if err != nil {
			log.Printf("nodemgr.Remove(%s): destroy template vmid=%d os=%s: %v",
				name, r.VMID, r.OS, err)
			continue
		}
		if taskID != "" {
			if err := s.px.WaitForTask(ctx, name, taskID, s.cfg.TaskPollInterval); err != nil {
				log.Printf("nodemgr.Remove(%s): wait destroy task vmid=%d: %v",
					name, r.VMID, err)
				// fall through to row delete — Proxmox accepted the call
			}
		}
	}
	if err := s.db.WithContext(ctx).
		Where("node = ?", name).
		Delete(&db.NodeTemplate{}).Error; err != nil {
		log.Printf("nodemgr.Remove(%s): clear node_templates rows: %v", name, err)
	}
}
