package provision

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"nimbus/internal/db"
)

// vacateMissThreshold is the number of consecutive reconcile cycles during
// which a VM row must be missing from Proxmox before we soft-delete it.
// Mirrors the IP reconciler's VACATE_MISS_THRESHOLD default — same forgiveness
// window, same operator intuition.
const vacateMissThreshold = 3

// VMSyncReport is the structured outcome of one ReconcileVMs run, also the
// JSON body of POST /api/vms/reconcile.
type VMSyncReport struct {
	Migrated   []VMMigration `json:"migrated"`
	Renamed    []VMRename    `json:"renamed"`
	Missed     []VMMiss      `json:"missed"`
	Deleted    []VMDeleted   `json:"deleted"`
	NoOps      int           `json:"no_ops"`
	SnapshotAt time.Time     `json:"snapshot_at"`
}

// VMMigration records a row whose Proxmox node moved out from under it
// (operator ran qm migrate outside nimbus). vms.node is updated in place.
type VMMigration struct {
	VMRowID  uint   `json:"vm_row_id"`
	VMID     int    `json:"vmid"`
	Hostname string `json:"hostname"`
	FromNode string `json:"from_node"`
	ToNode   string `json:"to_node"`
}

// VMRename records a row whose Proxmox display name diverged from the
// local hostname. Most often this means the operator renamed the VM
// directly in PVE (or that VMID was reused — Proxmox reassigns freed
// VMIDs, so a destroyed-and-recreated VM at the same ID shows up here
// instead of as Missed). The reconciler treats Proxmox as the source
// of truth and updates vms.hostname to match.
type VMRename struct {
	VMRowID  uint   `json:"vm_row_id"`
	VMID     int    `json:"vmid"`
	FromName string `json:"from_name"`
	ToName   string `json:"to_name"`
	Node     string `json:"node"`
}

// VMMiss records a row that Proxmox didn't return this cycle but hasn't yet
// crossed the soft-delete threshold. Useful for the UI's "going stale" chip.
type VMMiss struct {
	VMRowID      uint   `json:"vm_row_id"`
	VMID         int    `json:"vmid"`
	Hostname     string `json:"hostname"`
	Node         string `json:"node"`
	MissedCycles int    `json:"missed_cycles"`
}

// VMDeleted records a row the reconciler just soft-deleted because it crossed
// the miss threshold. Soft-delete (gorm.DeletedAt) means the row is recoverable
// by an operator who restores its deleted_at to NULL.
type VMDeleted struct {
	VMRowID  uint   `json:"vm_row_id"`
	VMID     int    `json:"vmid"`
	Hostname string `json:"hostname"`
	Node     string `json:"node"`
}

// errEmptyClusterSnapshot is returned when GetClusterVMs succeeds but returns
// zero VMs. Refusing to act guards against a Proxmox API returning a stale or
// empty response — the alternative would be to soft-delete every row in one
// pass.
var errEmptyClusterSnapshot = errors.New("cluster snapshot is empty — refusing to soft-delete every vm row")

// UnreachableNodesFunc returns the set of node names the local host can't
// currently reach over TCP. The reconciler treats a VM that's missing from
// the cluster snapshot as "still alive, just unreachable" when its host node
// is in this set — no missed_cycles bump, no soft-delete. Wired up in
// main.go via SetUnreachableNodesProbe; nil disables the guard (legacy
// behaviour where every missing VM gets reaped after the threshold).
type UnreachableNodesFunc func(ctx context.Context) map[string]bool

// SetUnreachableNodesProbe installs the per-cycle reachability check. Safe
// to call before/after the reconcile loop has started; reads happen at the
// top of each ReconcileVMs invocation so a freshly-installed probe takes
// effect on the next cycle.
func (s *Service) SetUnreachableNodesProbe(f UnreachableNodesFunc) {
	s.unreachableNodes = f
}

// ReconcileVMs walks the local vms table and converges it to the cluster
// snapshot. Per row, in order:
//
//  1. If found and the Proxmox display name disagrees with the local
//     hostname, update vms.hostname to match Proxmox. Catches operator
//     renames in PVE and stale rows whose VMID was reused after an
//     out-of-band destroy.
//  2. Same vmid found on the same node → reset MissedCycles, no-op.
//  3. Same vmid found on a *different* node → update vms.node (someone
//     ran qm migrate outside nimbus).
//  4. vmid not found anywhere in the cluster → bump MissedCycles. After
//     vacateMissThreshold consecutive misses, soft-delete the row.
//
// Refuses to act if the snapshot returned zero VMs — that's almost always a
// Proxmox API hiccup and acting on it would wipe every row at once.
func (s *Service) ReconcileVMs(ctx context.Context) (VMSyncReport, error) {
	// Initialize the slices (rather than leaving them nil) so the JSON
	// encoder emits "[]" instead of "null" for empty results — the SPA
	// reads .length on these without a guard.
	rep := VMSyncReport{
		Migrated:   []VMMigration{},
		Renamed:    []VMRename{},
		Missed:     []VMMiss{},
		Deleted:    []VMDeleted{},
		SnapshotAt: time.Now().UTC(),
	}

	cluster, err := s.px.GetClusterVMs(ctx)
	if err != nil {
		return rep, fmt.Errorf("get cluster vms: %w", err)
	}
	if len(cluster) == 0 {
		return rep, errEmptyClusterSnapshot
	}

	byVMID := make(map[int]struct{ Node, Name string }, len(cluster))
	for _, c := range cluster {
		byVMID[c.VMID] = struct{ Node, Name string }{Node: c.Node, Name: c.Name}
	}

	var rows []db.VM
	if err := s.db.WithContext(ctx).
		Where("vmid > 0").
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return rep, fmt.Errorf("list vms: %w", err)
	}

	// One reachability snapshot per cycle — fans out TCP dials so a transient
	// node outage doesn't soft-delete every VM living there.
	var unreachable map[string]bool
	if s.unreachableNodes != nil {
		unreachable = s.unreachableNodes(ctx)
	}

	for _, vm := range rows {
		px, found := byVMID[vm.VMID]
		// Hostname sync runs first when the VMID is present anywhere in
		// the cluster — both the same-node and migrated branches benefit
		// from a corrected display name. The local copy of vm.Hostname
		// gets updated in place so subsequent log lines and report
		// entries show the new name rather than the stale one.
		if found && px.Name != "" && px.Name != vm.Hostname {
			if err := s.handleVMRenamed(ctx, vm, px.Name, &rep); err == nil {
				vm.Hostname = px.Name
			}
		}
		switch {
		case !found:
			// Missing from cluster snapshot AND host node is currently
			// unreachable → almost certainly a node outage, not a manual
			// destroy. Don't bump missed_cycles; the row stays intact and
			// gets re-evaluated next cycle.
			if unreachable[vm.Node] {
				rep.NoOps++
				continue
			}
			s.handleVMMissing(ctx, vm, &rep)
		case px.Node != vm.Node:
			s.handleVMMigrated(ctx, vm, px.Node, &rep)
		default:
			s.handleVMSeen(ctx, vm, &rep)
		}
	}

	return rep, nil
}

// handleVMRenamed updates vms.hostname to match Proxmox's display name.
// Proxmox is the source of truth for the operator-visible name: this
// also covers the VMID-reuse case where a destroyed VM's row hadn't
// soft-deleted before a new VM was created at the same VMID — the
// reconciler can't tell those apart from a tag/identity perspective,
// but the name disagreement is observable and refreshes the row to at
// least describe what's actually there.
//
// The OS-level hostname (set by cloud-init at first boot) isn't
// touched — only the Nimbus DB row's display name. SSH-into-the-VM
// prompts may still show the old name; that's a cosmetic in-guest
// artifact, not a Nimbus-side stale state.
func (s *Service) handleVMRenamed(ctx context.Context, vm db.VM, newName string, rep *VMSyncReport) error {
	if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).
		Update("hostname", newName).Error; err != nil {
		log.Printf("vm-reconcile: rename hostname vmid=%d row=%d %q→%q: %v",
			vm.VMID, vm.ID, vm.Hostname, newName, err)
		return err
	}
	log.Printf("vm-reconcile: renamed vmid=%d %q → %q (proxmox is source of truth)",
		vm.VMID, vm.Hostname, newName)
	rep.Renamed = append(rep.Renamed, VMRename{
		VMRowID:  vm.ID,
		VMID:     vm.VMID,
		FromName: vm.Hostname,
		ToName:   newName,
		Node:     vm.Node,
	})
	return nil
}

// handleVMSeen resets MissedCycles to zero (idempotent — only writes when
// non-zero to keep the SQLite single-writer happy on no-op cycles).
func (s *Service) handleVMSeen(ctx context.Context, vm db.VM, rep *VMSyncReport) {
	if vm.MissedCycles > 0 {
		if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).
			Update("missed_cycles", 0).Error; err != nil {
			log.Printf("vm-reconcile: reset missed_cycles vmid=%d row=%d: %v", vm.VMID, vm.ID, err)
		}
	}
	rep.NoOps++
}

// handleVMMigrated updates vms.node to reflect the new Proxmox location.
// Resets MissedCycles too — the VM is alive, just elsewhere.
func (s *Service) handleVMMigrated(ctx context.Context, vm db.VM, newNode string, rep *VMSyncReport) {
	if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).Updates(map[string]any{
		"node":          newNode,
		"missed_cycles": 0,
	}).Error; err != nil {
		log.Printf("vm-reconcile: update node vmid=%d row=%d %s→%s: %v", vm.VMID, vm.ID, vm.Node, newNode, err)
		return
	}
	log.Printf("vm-reconcile: migrated vmid=%d (%s) %s → %s", vm.VMID, vm.Hostname, vm.Node, newNode)
	rep.Migrated = append(rep.Migrated, VMMigration{
		VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname,
		FromNode: vm.Node, ToNode: newNode,
	})
}

// handleVMMissing increments MissedCycles. When the post-increment value
// crosses vacateMissThreshold, soft-deletes the row.
func (s *Service) handleVMMissing(ctx context.Context, vm db.VM, rep *VMSyncReport) {
	next := vm.MissedCycles + 1
	if next < vacateMissThreshold {
		if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).
			Update("missed_cycles", next).Error; err != nil {
			log.Printf("vm-reconcile: bump missed_cycles vmid=%d row=%d: %v", vm.VMID, vm.ID, err)
			return
		}
		rep.Missed = append(rep.Missed, VMMiss{
			VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname, Node: vm.Node,
			MissedCycles: next,
		})
		return
	}
	// Threshold reached — soft-delete. gorm.DeletedAt is on the embedded
	// gorm.Model so a plain Delete() flips deleted_at instead of truncating.
	if err := s.db.WithContext(ctx).Delete(&db.VM{}, vm.ID).Error; err != nil {
		log.Printf("vm-reconcile: soft-delete vmid=%d row=%d: %v", vm.VMID, vm.ID, err)
		return
	}
	log.Printf("vm-reconcile: soft-deleted vmid=%d (%s) — missing for %d cycles", vm.VMID, vm.Hostname, next)
	// Also release the IP allocation so the slot returns to the pool.
	if vm.IP != "" {
		if err := s.pool.Release(ctx, vm.IP); err != nil {
			log.Printf("vm-reconcile: release ip %s for vmid=%d: %v", vm.IP, vm.VMID, err)
		}
	}
	rep.Deleted = append(rep.Deleted, VMDeleted{
		VMRowID: vm.ID, VMID: vm.VMID, Hostname: vm.Hostname, Node: vm.Node,
	})
}
