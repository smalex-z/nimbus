package provision

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
)

// MigrationResult is the outcome of a successful MigrateAdmin call.
// Mode reports whether Proxmox accepted the live migration (online) or
// whether the VM had to be stopped first (offline). WasStopped reflects
// whether the VM was running at the start of the call and we power-cycled
// it across the migration; the SPA uses it to phrase "VM was briefly
// offline during migration."
type MigrationResult struct {
	Mode       MigrationMode
	TargetNode string
	WasStopped bool
}

// MigrationMode is the wire-shape of MigrationResult.Mode.
type MigrationMode string

const (
	MigrationModeOnline  MigrationMode = "online"
	MigrationModeOffline MigrationMode = "offline"
)

// MigrateAdmin moves a Nimbus-managed VM to a new cluster node.
//
// Flow:
//  1. Validate the target — exists, is online, isn't the source.
//  2. If the VM is running:
//     a. Try the live migration via MigrateVM(online=true).
//     b. On success, update the local row and return mode=online.
//     c. On failure, return *errors.OnlineMigrationFailedError unless
//     allowOffline=true, in which case fall through to (3).
//  3. Offline path (VM stopped, or running + allowOffline + online failed):
//     a. If running, gracefully shut the VM down (Proxmox shutdown task).
//     b. Migrate with online=false.
//     c. If we shut it down in (a), start it again on the new node.
//
// The handler maps OnlineMigrationFailedError to a 409 with a structured
// `code: "online_migration_failed"` payload so the SPA can prompt the
// admin to retry with `allow_offline: true`.
//
// Eligibility check is intentionally light (target exists + online +
// not-same-node). Capacity gating is left to Proxmox itself: if the
// target lacks RAM, the migrate task fails and we surface the upstream
// reason. Migration is moving an existing VM — the nodescore "would I
// place a NEW VM here" gate that drain uses is the wrong question.
func (s *Service) MigrateAdmin(ctx context.Context, id uint, target string, allowOffline bool) (*MigrationResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, &internalerrors.ValidationError{Field: "target_node", Message: "target_node is required"}
	}

	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return nil, fmt.Errorf("get vm %d: %w", id, err)
	}

	if err := s.validateMigrateTarget(ctx, vm.Node, target); err != nil {
		return nil, err
	}

	wasRunning := strings.EqualFold(vm.Status, "running")

	// 1. Try online migration when the VM is running.
	if wasRunning {
		online, err := s.tryOnlineMigration(ctx, vm, target)
		if err == nil {
			return online, nil
		}
		var onlineFail *internalerrors.OnlineMigrationFailedError
		if !errors.As(err, &onlineFail) {
			// Non-online-class failure — surface as-is. We don't retry
			// offline because the cause is something offline won't fix
			// (e.g. ctx canceled, transport error).
			return nil, err
		}
		if !allowOffline {
			return nil, err
		}
		log.Printf("migrate vm=%d source=%s target=%s: online failed, falling back to offline: %s",
			vm.ID, vm.Node, target, onlineFail.Reason)
	}

	// 2. Offline path — stop if running, migrate, start if we stopped it.
	wasStopped := false
	if wasRunning {
		if err := s.stopForMigration(ctx, vm); err != nil {
			return nil, fmt.Errorf("stop vm %d before offline migrate: %w", vm.ID, err)
		}
		wasStopped = true
	}

	if err := s.migrateOffline(ctx, vm, target); err != nil {
		// If we stopped the VM but the migration itself failed, try to
		// power it back on so the admin isn't left with a dark VM. The
		// migrate failure is the original cause — surface it after.
		if wasStopped {
			if startErr := s.startAfterFailedMigrate(ctx, vm); startErr != nil {
				log.Printf("migrate vm=%d: offline migrate failed AND restart-in-place failed: %v", vm.ID, startErr)
			}
		}
		return nil, fmt.Errorf("offline migrate: %w", err)
	}

	if wasStopped {
		// VM has now landed on `target`. Start it there.
		if err := s.startAfterMigration(ctx, vm.VMID, target); err != nil {
			// Migration succeeded; only the restart failed. Don't roll
			// back the migration — surface a partial-success error so
			// the admin can start it themselves on the new node.
			return nil, fmt.Errorf("started offline migration successfully but failed to restart vm on %s: %w", target, err)
		}
	}

	if err := s.updateVMNode(ctx, vm.ID, target); err != nil {
		log.Printf("migrate vm=%d: db node update failed (reconciler will fix): %v", vm.ID, err)
	}

	return &MigrationResult{
		Mode:       MigrationModeOffline,
		TargetNode: target,
		WasStopped: wasStopped,
	}, nil
}

// tryOnlineMigration runs the migrate request with online=1 and waits for
// the source-node task. Wraps every recognisable failure mode as
// *OnlineMigrationFailedError so MigrateAdmin can decide whether to fall
// back. Context cancellation surfaces verbatim — that isn't an
// "online unavailable" condition, just an aborted request.
func (s *Service) tryOnlineMigration(ctx context.Context, vm db.VM, target string) (*MigrationResult, error) {
	taskID, err := s.px.MigrateVM(ctx, vm.Node, vm.VMID, target, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &internalerrors.OnlineMigrationFailedError{Reason: err.Error()}
	}
	if taskID != "" {
		// Migration tasks run on the SOURCE node — that's where the
		// memory copy + cutover orchestration lives.
		if err := s.px.WaitForTask(ctx, vm.Node, taskID, taskPollInterval(s.cfg.PollInterval)); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &internalerrors.OnlineMigrationFailedError{Reason: err.Error()}
		}
	}

	if err := s.updateVMNode(ctx, vm.ID, target); err != nil {
		log.Printf("migrate vm=%d: db node update failed (reconciler will fix): %v", vm.ID, err)
	}

	return &MigrationResult{
		Mode:       MigrationModeOnline,
		TargetNode: target,
		WasStopped: false,
	}, nil
}

// stopForMigration issues a graceful shutdown and waits for it.
//
// We use shutdown rather than stop so the guest gets a chance to flush
// disks and unmount cleanly. If the guest agent is misbehaving the task
// will eventually time out — the route timeout in the router (35min)
// covers the worst case. We don't fall back to stop here because a hung
// shutdown usually indicates the VM is in a bad enough state that a
// pulled-plug stop would cause more harm than good.
func (s *Service) stopForMigration(ctx context.Context, vm db.VM) error {
	taskID, err := s.px.ShutdownVM(ctx, vm.Node, vm.VMID)
	if err != nil {
		if isAlreadyInTargetState(VMOpShutdown, err) {
			return nil
		}
		return fmt.Errorf("shutdown request: %w", err)
	}
	if taskID == "" {
		return nil
	}
	if err := s.px.WaitForTask(ctx, vm.Node, taskID, taskPollInterval(s.cfg.PollInterval)); err != nil {
		if isAlreadyInTargetState(VMOpShutdown, err) {
			return nil
		}
		return fmt.Errorf("shutdown task: %w", err)
	}
	return nil
}

// migrateOffline runs the migrate request with online=0 and waits.
// The VM must already be stopped — Proxmox refuses online=0 on a running
// VM.
func (s *Service) migrateOffline(ctx context.Context, vm db.VM, target string) error {
	taskID, err := s.px.MigrateVM(ctx, vm.Node, vm.VMID, target, false)
	if err != nil {
		return fmt.Errorf("migrate request: %w", err)
	}
	if taskID == "" {
		return nil
	}
	return s.px.WaitForTask(ctx, vm.Node, taskID, taskPollInterval(s.cfg.PollInterval))
}

// startAfterMigration starts the VM on the new node and waits.
func (s *Service) startAfterMigration(ctx context.Context, vmid int, target string) error {
	taskID, err := s.px.StartVM(ctx, target, vmid)
	if err != nil {
		if isAlreadyInTargetState(VMOpStart, err) {
			return nil
		}
		return fmt.Errorf("start request: %w", err)
	}
	if taskID == "" {
		return nil
	}
	if err := s.px.WaitForTask(ctx, target, taskID, taskPollInterval(s.cfg.PollInterval)); err != nil {
		if isAlreadyInTargetState(VMOpStart, err) {
			return nil
		}
		return fmt.Errorf("start task: %w", err)
	}
	return nil
}

// startAfterFailedMigrate is the "we already stopped this VM and now the
// migration itself failed" recovery — bring it back up on the SOURCE node
// so the admin doesn't end up with a dark VM. Best-effort; logged failures
// don't override the original migration error.
func (s *Service) startAfterFailedMigrate(ctx context.Context, vm db.VM) error {
	return s.startAfterMigration(ctx, vm.VMID, vm.Node)
}

// validateMigrateTarget runs the cheap, deterministic checks the issue
// spec calls for: target node exists in the cluster, is online, and isn't
// the source. Capacity / template / lock-state gating is left to Proxmox
// itself — the upstream error message is more honest than re-implementing
// the policy here.
func (s *Service) validateMigrateTarget(ctx context.Context, source, target string) error {
	if target == source {
		return &internalerrors.ConflictError{Message: "target_node is the same as the VM's current node"}
	}
	nodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return fmt.Errorf("list cluster nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Name == target {
			if !strings.EqualFold(n.Status, "online") {
				return &internalerrors.ConflictError{Message: fmt.Sprintf("target_node %q is not online (status=%s)", target, n.Status)}
			}
			return nil
		}
	}
	return &internalerrors.NotFoundError{Resource: "node", ID: target}
}

// updateVMNode sets vms.node = target in the local cache so the next
// reconcile pass sees the new placement immediately. Failures are
// non-fatal — Proxmox already moved the VM, the reconciler will catch up.
func (s *Service) updateVMNode(ctx context.Context, id uint, target string) error {
	return s.db.WithContext(ctx).Model(&db.VM{}).
		Where("id = ?", id).
		Update("node", target).Error
}

// taskPollInterval falls back to a sensible default when cfg.PollInterval
// hasn't been set (tests, mostly — production seeds it from config).
func taskPollInterval(configured time.Duration) time.Duration {
	if configured <= 0 {
		return 3 * time.Second
	}
	return configured
}
