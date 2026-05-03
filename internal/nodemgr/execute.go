package nodemgr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/nodescore"
)

// DrainEvent is one streamed update to the operator. The HTTP layer turns
// each event into one NDJSON line — the Type field tells the SPA how to
// render it.
//
// Type values:
//
//	"plan_locked"  — drain accepted, source flipped to "draining"
//	"vm_start"     — about to migrate this VM
//	"vm_done"      — migration succeeded
//	"vm_error"     — this VM aborted; batch continues
//	"complete"     — all VMs processed (Drained may be true or false)
//
// Unknown types should be ignored by the SPA — keeps the wire format
// forward-compat with future event kinds (per-step progress, etc.).
type DrainEvent struct {
	Type     string `json:"type"`
	VMID     int    `json:"vm_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Target   string `json:"target,omitempty"`
	Step     string `json:"step,omitempty"`
	Error    string `json:"error,omitempty"`
	// Only set on type=complete.
	Succeeded int  `json:"succeeded,omitempty"`
	Failed    int  `json:"failed,omitempty"`
	Drained   bool `json:"drained,omitempty"`
}

// DrainReporter is the callback the executor invokes for every event. The
// HTTP layer wraps it to write+flush NDJSON; tests inspect the slice it
// accumulates.
type DrainReporter func(DrainEvent)

// ExecuteRequest is what the operator confirms — one entry per VM with the
// destination they (possibly overridden from AutoPick) chose. The executor
// re-validates each destination against the live cluster snapshot just
// before issuing the migrate.
type ExecuteRequest struct {
	SourceNode string
	// Choices is keyed by VMID; value is the operator-confirmed target.
	// VMIDs not present in the map are treated as "no destination chosen"
	// and rejected at execute time (operator must select one).
	Choices map[int]string
}

// Execute runs the operator-confirmed drain. Steps:
//
//  1. Mark the source as "draining" (atomic — refuses if already draining).
//  2. For each VM, in vmid order:
//     a. Re-fetch cluster snapshot + recompute eligibility for the chosen
//     target. Ineligible → emit vm_error, continue.
//     b. Issue Proxmox migrate; emit vm_start, wait on the task.
//     c. On success: update db.VM.node, emit vm_done.
//     d. On failure: emit vm_error, continue.
//  3. After the batch, count remaining managed VMs on the source. Zero ⇒
//     flip source to "drained"; non-zero ⇒ flip back to "cordoned" so the
//     operator can retry from the same starting state.
//  4. Emit `complete` with the tally + final state.
//
// Drain is intentionally non-cancellable from the operator side: once the
// confirmation lands, every per-VM migration runs to completion (success or
// failure). Cancelling mid-flight would leave VMs in a half-migrated state
// (Proxmox itself can't gracefully cancel an in-flight migrate task).
func (s *Service) Execute(ctx context.Context, req ExecuteRequest, report DrainReporter) error {
	if !s.markDrainInFlight(req.SourceNode) {
		return ErrDrainInFlight
	}
	defer s.markDrainDone(req.SourceNode)

	// Lock state → "draining". Validate-then-write so a concurrent cordon
	// or stale request can't race past us.
	row, err := s.loadOrCreate(ctx, req.SourceNode)
	if err != nil {
		return fmt.Errorf("load source: %w", err)
	}
	if err := validTransition(row.LockState, "draining"); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", req.SourceNode).
		Updates(map[string]interface{}{
			"lock_state": "draining",
			"locked_at":  now,
			"updated_at": now,
		}).Error; err != nil {
		return fmt.Errorf("flip source to draining: %w", err)
	}
	report(DrainEvent{Type: "plan_locked"})

	// Iterate the managed VMs (snapshot at the start; new VMs aren't
	// possible since the scheduler now skips this node).
	managed, err := s.managedVMsOnNode(ctx, req.SourceNode)
	if err != nil {
		return fmt.Errorf("list managed vms: %w", err)
	}

	var succeeded, failed int
	for _, vm := range managed {
		target, ok := req.Choices[vm.VMID]
		if !ok || target == "" {
			report(DrainEvent{
				Type: "vm_error", VMID: vm.VMID, Hostname: vm.Hostname,
				Error: "no destination chosen for this vm",
			})
			failed++
			continue
		}

		if err := s.migrateOne(ctx, vm, target, report); err != nil {
			report(DrainEvent{
				Type: "vm_error", VMID: vm.VMID, Hostname: vm.Hostname,
				Target: target, Error: err.Error(),
			})
			failed++
			continue
		}
		report(DrainEvent{
			Type: "vm_done", VMID: vm.VMID, Hostname: vm.Hostname, Target: target,
		})
		succeeded++
	}

	// Decide the post-drain state. Recount managed VMs on the source —
	// any survivors mean the drain didn't fully evacuate.
	remaining, err := s.managedVMsOnNode(ctx, req.SourceNode)
	if err != nil {
		// Soft-fail: still emit complete, but conservatively flip to
		// cordoned so the operator doesn't blindly remove a node we
		// couldn't fully verify.
		_ = s.flipSourceTo(ctx, req.SourceNode, "cordoned")
		report(DrainEvent{
			Type: "complete", Succeeded: succeeded, Failed: failed, Drained: false,
		})
		return fmt.Errorf("recount remaining vms: %w", err)
	}
	finalState := "cordoned"
	drained := false
	if len(remaining) == 0 {
		finalState = "drained"
		drained = true
	}
	if err := s.flipSourceTo(ctx, req.SourceNode, finalState); err != nil {
		return fmt.Errorf("flip source to %s: %w", finalState, err)
	}
	report(DrainEvent{
		Type: "complete", Succeeded: succeeded, Failed: failed, Drained: drained,
	})
	return nil
}

// migrateOne performs the per-VM execution: re-validate, issue migrate,
// poll task, update DB. Returns nil on success.
func (s *Service) migrateOne(ctx context.Context, vm db.VM, target string, report DrainReporter) error {
	// Re-validate against live state. The plan was captured at preview
	// time; capacity, lock state, or cluster membership may have shifted.
	if err := s.validateTarget(ctx, vm, target); err != nil {
		return err
	}
	report(DrainEvent{
		Type: "vm_start", VMID: vm.VMID, Hostname: vm.Hostname,
		Target: target, Step: "migrating",
	})

	migrateCtx, cancel := context.WithTimeout(ctx, s.cfg.PerVMMigrateTimeout)
	defer cancel()

	taskID, err := s.px.MigrateVM(migrateCtx, vm.Node, vm.VMID, target, true)
	if err != nil {
		return fmt.Errorf("migrate request: %w", err)
	}
	if taskID != "" {
		// Migration tasks run on the SOURCE node — that's where the
		// memory copy + cutover orchestration lives.
		if err := s.px.WaitForTask(migrateCtx, vm.Node, taskID, s.cfg.TaskPollInterval); err != nil {
			return fmt.Errorf("migrate task: %w", err)
		}
	}

	// Update local row so the next reconcile pass sees the canonical
	// node placement immediately.
	if err := s.db.WithContext(ctx).Model(&db.VM{}).
		Where("id = ?", vm.ID).
		Update("node", target).Error; err != nil {
		// Don't fail the migration — Proxmox already moved the VM.
		// The IP/VM reconciler will re-derive node from cluster state
		// on the next pass.
		return nil
	}
	return nil
}

// validateTarget re-runs a single nodescore evaluation against the live
// cluster for one candidate destination. Returns an error string suitable
// for the per-VM error event when the destination is no longer eligible.
//
// This is the "re-validation at execution time" guarantee from the spec:
// even if the operator's preview said target=pve-2, between confirm and
// execute the cluster could change. Fail-fast keeps the batch honest.
func (s *Service) validateTarget(ctx context.Context, vm db.VM, target string) error {
	tier, ok := nodescore.Tiers[vm.Tier]
	if !ok {
		return fmt.Errorf("unknown tier %q", vm.Tier)
	}

	nodes, vms, _, err := s.clusterSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	rows, err := s.loadAllRows(ctx)
	if err != nil {
		return fmt.Errorf("load db rows: %w", err)
	}

	// Find the target's live state.
	var targetNode *nodescore.Node
	for _, n := range nodes {
		if n.Name == target {
			targetNode = &nodescore.Node{
				Name: n.Name, Status: n.Status, CPU: n.CPU,
				MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
				LockState: lockOrNone(rows[n.Name].LockState),
			}
			break
		}
	}
	if targetNode == nil {
		return fmt.Errorf("target node %q not in cluster", target)
	}

	// Build runtime for the target only — that's all Score needs.
	rt := nodescore.NodeRuntime{}
	for _, v := range vms {
		if v.Template != 0 || v.Node != target {
			continue
		}
		rt.VMCount++
		rt.CommittedMemBytes += v.MaxMem
	}

	env := nodescore.Env{
		TemplatesPresent: map[string]bool{target: true}, // migration doesn't need templates
	}
	got := nodescore.Score(*targetNode, tier, env, rt)
	if got.Score == 0 {
		return fmt.Errorf("target %s no longer eligible: %s", target, formatReasons(got.Reasons))
	}
	return nil
}

// flipSourceTo updates the lock state on the source node after the drain.
// On success ("drained") or failure ("cordoned" — operator can retry).
func (s *Service) flipSourceTo(ctx context.Context, name, state string) error {
	updates := map[string]interface{}{
		"lock_state": state,
		"updated_at": time.Now().UTC(),
	}
	if state == "none" {
		updates["locked_at"] = nil
		updates["locked_by"] = nil
		updates["lock_reason"] = ""
	}
	return s.db.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", name).
		Updates(updates).Error
}

// IsDrainInFlight reports whether a drain is in progress for the named
// node. Exposed so handlers can short-circuit conflicting requests with
// a 409 instead of waiting on the per-node lock.
func (s *Service) IsDrainInFlight(name string) bool {
	s.drainsMu.Lock()
	defer s.drainsMu.Unlock()
	return s.drainsInFlight[name]
}

// IsValidationError reports whether an executor error is "this destination
// is no longer eligible." Currently a string-prefix check; if the executor
// gains more typed errors, lift to a sentinel.
func IsValidationError(err error) bool {
	if err == nil {
		return false
	}
	return errorContains(err, "no longer eligible")
}

// errorContains is a wrapper around strings.Contains that handles wrapped
// errors via errors.Unwrap chain.
func errorContains(err error, substr string) bool {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if cur.Error() != "" && containsString(cur.Error(), substr) {
			return true
		}
	}
	return false
}

// containsString — strings.Contains alias to avoid pulling in strings here.
// Kept tiny so this file's dep surface stays at db/proxmox/nodescore.
func containsString(s, substr string) bool {
	if substr == "" {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
