package nodemgr

import (
	"context"
	"fmt"
	"sort"

	"nimbus/internal/db"
	"nimbus/internal/nodescore"
	"nimbus/internal/proxmox"
)

// DrainPlan is the operator-reviewable migration plan. Migrations carries
// one entry per VM Nimbus knows about on the source node; Aggregate carries
// the projected per-destination capacity after applying the planned set.
//
// AutoPick (per row) is the recommended target; the operator may override
// to any Eligible node. Override empty means "use AutoPick". The same shape
// is sent back into Execute so the executor sees exactly what the operator
// confirmed.
type DrainPlan struct {
	SourceNode string             `json:"source_node"`
	Migrations []PlannedMigration `json:"migrations"`
	Aggregate  []NodeProjection   `json:"aggregate"`
	// HasBlocked is a derived flag the SPA gates the "Start drain" button
	// on. True iff any row has Eligible == [] or Override pointing at a
	// disabled target (the executor would refuse).
	HasBlocked bool `json:"has_blocked"`
}

// PlannedMigration describes one VM's planned move.
type PlannedMigration struct {
	VMID     int              `json:"vm_id"`
	VMRowID  uint             `json:"vm_row_id"`
	Hostname string           `json:"hostname"`
	Tier     string           `json:"tier"`
	AutoPick string           `json:"auto_pick"`
	Override string           `json:"override,omitempty"`
	Eligible []EligibleTarget `json:"eligible"`
	Warnings []DrainWarning   `json:"warnings,omitempty"`
}

// EligibleTarget is one option in the per-row destination dropdown. Score
// is the raw nodescore output (higher = better fit); Disabled is true when
// the node fails a hard gate (cordoned, offline, no capacity, etc.) — the
// SPA renders these dimmed with the DisabledReason as a tooltip.
type EligibleTarget struct {
	Node            string  `json:"node"`
	Score           float64 `json:"score"`
	ProjectedRAMPct float64 `json:"projected_ram_pct"`
	Disabled        bool    `json:"disabled"`
	DisabledReason  string  `json:"disabled_reason,omitempty"`
}

// NodeProjection is the per-destination footer row: current vs. planned
// VM count + RAM%, with a severity classification used to colour the row.
type NodeProjection struct {
	Node           string  `json:"node"`
	CurrentVMCount int     `json:"current_vm_count"`
	PlannedVMCount int     `json:"planned_vm_count"`
	CurrentRAMPct  float64 `json:"current_ram_pct"`
	PlannedRAMPct  float64 `json:"planned_ram_pct"`
	Severity       string  `json:"severity"` // "ok"/"caution"/"high"
}

// DrainWarning is one severity-tagged note attached to a row. The executor
// stays disabled while any row carries a "blocked" warning.
type DrainWarning struct {
	Severity string `json:"severity"` // "soft"/"caution"/"high"/"blocked"
	Message  string `json:"message"`
}

// ComputePlan builds the drain plan for the named source node. Reads live
// cluster telemetry + the local VM table, scores every (vm, candidate-node)
// pair via nodescore, and assembles the per-row dropdowns + footer.
//
// The plan reflects the cluster as it stands NOW; the executor re-validates
// each VM's destination at migration time so a destination that fills up
// between preview and execute aborts cleanly without taking the batch with
// it. See Execute.
func (s *Service) ComputePlan(ctx context.Context, sourceNode string) (*DrainPlan, error) {
	// 1. Live snapshot.
	nodes, vms, storeRows, err := s.clusterSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	// 2. Local row state for lock filtering.
	dbNodes, err := s.loadAllRows(ctx)
	if err != nil {
		return nil, err
	}
	// 3. VMs to migrate: every Nimbus-managed VM on this node.
	managed, err := s.managedVMsOnNode(ctx, sourceNode)
	if err != nil {
		return nil, err
	}

	// Build candidate set: every node OTHER than the source.
	candidates := make([]nodescore.Node, 0, len(nodes)-1)
	templatesPresent := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.Name == sourceNode {
			continue
		}
		// For migration, "templates present" is irrelevant — Proxmox moves
		// the disk, no clone happens. Bypass the template gate by claiming
		// every candidate has the template.
		templatesPresent[n.Name] = true
		row := dbNodes[n.Name]
		candidates = append(candidates, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
			LockState: lockOrNone(row.LockState),
		})
	}

	// Aggregate per-node committed mem from the cluster snapshot. Keyed
	// by node so we can mutate as we plan migrations.
	committedMem := make(map[string]uint64, len(nodes))
	currentVMCount := make(map[string]int, len(nodes))
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		committedMem[vm.Node] += vm.MaxMem
		currentVMCount[vm.Node]++
	}
	// Capacity (MaxMem) per node for projection arithmetic.
	maxMemByNode := make(map[string]uint64, len(nodes))
	for _, n := range nodes {
		maxMemByNode[n.Name] = n.MaxMem
	}

	// Disk telemetry: build the same StorageByNode map provision.pickNode
	// uses so the drain plan's disk gate fires up front, surfacing
	// no-room errors in the modal instead of letting the migrate call
	// fail mid-batch. Disabled when the operator hasn't configured a
	// VM-disk pool (cfg.VMDiskStorage empty).
	var storageByNodeOpt map[string]nodescore.StorageInfo
	if s.cfg.VMDiskStorage != "" {
		storageByNodeOpt = make(map[string]nodescore.StorageInfo, len(nodes))
		// Shared pools repeat per node with identical capacity; the
		// first row wins, then stamp the same StorageInfo onto every
		// node so the disk gate sees the same pool everywhere.
		var sharedInfo *nodescore.StorageInfo
		for _, st := range storeRows {
			if st.Storage != s.cfg.VMDiskStorage {
				continue
			}
			info := nodescore.StorageInfo{TotalBytes: st.Total, UsedBytes: st.Used}
			if st.Shared == 1 {
				if sharedInfo == nil {
					sharedInfo = &info
				}
				continue
			}
			storageByNodeOpt[st.Node] = info
		}
		if sharedInfo != nil {
			for _, n := range nodes {
				storageByNodeOpt[n.Name] = *sharedInfo
			}
		}
	}
	// Track planned disk additions per target so each subsequent VM in
	// the loop sees the running total (mirrors plannedAdd for memory).
	plannedDiskAdd := make(map[string]uint64)

	plan := &DrainPlan{SourceNode: sourceNode, Migrations: make([]PlannedMigration, 0, len(managed))}
	plannedAdd := make(map[string]uint64) // additional committed mem to add per dest

	for _, vm := range managed {
		tier, ok := nodescore.Tiers[vm.Tier]
		if !ok {
			// Unknown tier — emit a row that's blocked so the operator
			// sees it rather than silently dropping the VM.
			plan.Migrations = append(plan.Migrations, PlannedMigration{
				VMID: vm.VMID, VMRowID: vm.ID, Hostname: vm.Hostname, Tier: vm.Tier,
				Eligible: []EligibleTarget{},
				Warnings: []DrainWarning{{
					Severity: "blocked",
					Message:  fmt.Sprintf("unknown tier %q — fix the VM row before draining", vm.Tier),
				}},
			})
			continue
		}

		// Per-VM runtime: shared across candidates (vm count + committed
		// mem are properties of the destination, not the moving VM).
		runtime := make(map[string]nodescore.NodeRuntime, len(candidates))
		for _, c := range candidates {
			runtime[c.Name] = nodescore.NodeRuntime{
				VMCount:           currentVMCount[c.Name] + countPlanned(plan.Migrations, c.Name),
				CommittedMemBytes: committedMem[c.Name] + plannedAdd[c.Name],
			}
		}

		// Build a per-VM view of disk pools that already counts what
		// earlier-planned migrations promised — otherwise two large VMs
		// could both be planned to land on a node that only fits one.
		var perVMStorage map[string]nodescore.StorageInfo
		if storageByNodeOpt != nil {
			perVMStorage = make(map[string]nodescore.StorageInfo, len(storageByNodeOpt))
			for name, si := range storageByNodeOpt {
				si.UsedBytes += plannedDiskAdd[name]
				perVMStorage[name] = si
			}
		}

		// VM workload drives the Profile selection so a migrated DB VM
		// still prefers a memory-optimized destination. Empty (legacy
		// row) → tier-default.
		workload := nodescore.WorkloadType(vm.WorkloadType)
		if workload == "" {
			workload = nodescore.DefaultWorkloadForTier(vm.Tier)
		}

		env := nodescore.Env{
			TemplatesPresent: templatesPresent,
			StorageByNode:    perVMStorage,
			Workload:         workload,
		}
		decisions := nodescore.Evaluate(candidates, runtime, tier, env)
		winner, _ := nodescore.Pick(decisions)

		row := PlannedMigration{
			VMID: vm.VMID, VMRowID: vm.ID, Hostname: vm.Hostname, Tier: vm.Tier,
		}
		if winner != nil {
			row.AutoPick = winner.Node.Name
		}

		// Build dropdown options from every candidate, sorted by score
		// desc so the recommendation sits at the top. Disabled options
		// ride along for transparency (per spec).
		row.Eligible = buildEligible(decisions, maxMemByNode, committedMem, plannedAdd, tier)

		// Per-row warnings keyed off the AutoPick (initial state). The
		// SPA recomputes these locally as the operator overrides; we
		// only seed the initial view.
		row.Warnings = warningsForTarget(row.AutoPick, row.Eligible)

		// Apply this VM's planned mem AND disk to the destination's
		// running tally so the next VM's runtime + storage view see them.
		if winner != nil {
			plannedAdd[winner.Node.Name] += uint64(tier.MemMB) * (1 << 20)
			plannedDiskAdd[winner.Node.Name] += uint64(tier.DiskGB) * (1 << 30)
		}

		plan.Migrations = append(plan.Migrations, row)
	}

	plan.Aggregate = computeAggregate(candidates, currentVMCount, committedMem, plannedAdd, maxMemByNode, plan.Migrations)
	plan.HasBlocked = anyBlocked(plan.Migrations)
	return plan, nil
}

// anyBlocked is true when any migration row carries a blocked-severity
// warning OR no eligible (non-disabled) target.
func anyBlocked(migs []PlannedMigration) bool {
	for _, m := range migs {
		for _, w := range m.Warnings {
			if w.Severity == "blocked" {
				return true
			}
		}
		if !hasUsableTarget(m) {
			return true
		}
	}
	return false
}

func hasUsableTarget(m PlannedMigration) bool {
	for _, e := range m.Eligible {
		if !e.Disabled {
			return true
		}
	}
	return false
}

// countPlanned counts how many migrations earlier in the slice have already
// targeted node n. Used so the next VM's projected runtime is "current +
// what earlier rows already promised."
func countPlanned(migs []PlannedMigration, node string) int {
	c := 0
	for _, m := range migs {
		if m.AutoPick == node {
			c++
		}
	}
	return c
}

// buildEligible turns a Decision slice into the dropdown options. Sorted
// by score desc so the recommended target sits at the top; ineligible
// options ride along disabled.
func buildEligible(
	decisions []nodescore.Decision,
	maxMemByNode map[string]uint64,
	committedMem, plannedAdd map[string]uint64,
	tier nodescore.Tier,
) []EligibleTarget {
	out := make([]EligibleTarget, 0, len(decisions))
	for _, d := range decisions {
		opt := EligibleTarget{Node: d.Node.Name, Score: d.Result.Score}
		if d.Result.Score == 0 {
			opt.Disabled = true
			opt.DisabledReason = formatReasons(d.Result.Reasons)
		}
		if maxMem := maxMemByNode[d.Node.Name]; maxMem > 0 {
			projected := committedMem[d.Node.Name] + plannedAdd[d.Node.Name] + uint64(tier.MemMB)*(1<<20)
			opt.ProjectedRAMPct = float64(projected) / float64(maxMem) * 100
		}
		out = append(out, opt)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Eligible (non-disabled) first; within each group, higher score
		// wins. Stable sort keeps input order for ties.
		if out[i].Disabled != out[j].Disabled {
			return !out[i].Disabled
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// warningsForTarget returns the per-row warnings appropriate for the
// currently-selected target. Severity ladder:
//
//	blocked  — target is disabled (no capacity, cordoned, etc.)
//	high     — projected RAM > 85 %
//	caution  — projected RAM 60-85 %
//	(none)   — < 60 %, no flag
func warningsForTarget(targetName string, options []EligibleTarget) []DrainWarning {
	if targetName == "" {
		return []DrainWarning{{Severity: "blocked", Message: "no eligible destination"}}
	}
	for _, opt := range options {
		if opt.Node != targetName {
			continue
		}
		if opt.Disabled {
			return []DrainWarning{{Severity: "blocked", Message: opt.DisabledReason}}
		}
		switch {
		case opt.ProjectedRAMPct > 85:
			return []DrainWarning{{
				Severity: "high",
				Message:  fmt.Sprintf("%s will be at %.0f%% RAM — risk of memory pressure", targetName, opt.ProjectedRAMPct),
			}}
		case opt.ProjectedRAMPct > 60:
			return []DrainWarning{{
				Severity: "caution",
				Message:  fmt.Sprintf("%s will be at %.0f%% RAM", targetName, opt.ProjectedRAMPct),
			}}
		}
		return nil
	}
	return []DrainWarning{{Severity: "blocked", Message: "selected target is not eligible"}}
}

// computeAggregate returns the per-destination projection footer.
func computeAggregate(
	candidates []nodescore.Node,
	currentVMCount map[string]int,
	committedMem, plannedAdd map[string]uint64,
	maxMemByNode map[string]uint64,
	migs []PlannedMigration,
) []NodeProjection {
	// recount per destination from migs so the aggregate matches what's
	// shown in the table (rather than relying on plannedAdd which tracks
	// AutoPick only).
	plannedCount := make(map[string]int, len(candidates))
	for _, m := range migs {
		t := m.AutoPick
		if m.Override != "" {
			t = m.Override
		}
		if t != "" {
			plannedCount[t]++
		}
	}
	out := make([]NodeProjection, 0, len(candidates))
	for _, c := range candidates {
		total := maxMemByNode[c.Name]
		var curPct, planPct float64
		if total > 0 {
			curPct = float64(committedMem[c.Name]) / float64(total) * 100
			planPct = float64(committedMem[c.Name]+plannedAdd[c.Name]) / float64(total) * 100
		}
		sev := "ok"
		switch {
		case planPct > 85:
			sev = "high"
		case planPct > 60:
			sev = "caution"
		}
		out = append(out, NodeProjection{
			Node:           c.Name,
			CurrentVMCount: currentVMCount[c.Name],
			PlannedVMCount: currentVMCount[c.Name] + plannedCount[c.Name],
			CurrentRAMPct:  curPct,
			PlannedRAMPct:  planPct,
			Severity:       sev,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func formatReasons(reasons []nodescore.Reason) string {
	if len(reasons) == 0 {
		return ""
	}
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, string(r))
	}
	return joinTags(parts)
}

// clusterSnapshot wraps the parallel fan-out the planner needs.
func (s *Service) clusterSnapshot(ctx context.Context) (
	[]proxmox.Node, []proxmox.ClusterVM, []proxmox.ClusterStorage, error,
) {
	type result struct {
		nodes []proxmox.Node
		vms   []proxmox.ClusterVM
		store []proxmox.ClusterStorage
		err   error
	}
	nodesCh := make(chan result, 1)
	vmsCh := make(chan result, 1)
	storeCh := make(chan result, 1)
	go func() {
		ns, err := s.px.GetNodes(ctx)
		nodesCh <- result{nodes: ns, err: err}
	}()
	go func() {
		vs, err := s.px.GetClusterVMs(ctx)
		vmsCh <- result{vms: vs, err: err}
	}()
	go func() {
		st, err := s.px.GetClusterStorage(ctx)
		storeCh <- result{store: st, err: err}
	}()
	nr, vr, sr := <-nodesCh, <-vmsCh, <-storeCh
	if nr.err != nil {
		return nil, nil, nil, fmt.Errorf("get nodes: %w", nr.err)
	}
	if vr.err != nil {
		return nil, nil, nil, fmt.Errorf("get cluster vms: %w", vr.err)
	}
	if sr.err != nil {
		return nil, nil, nil, fmt.Errorf("get cluster storage: %w", sr.err)
	}
	return nr.nodes, vr.vms, sr.store, nil
}

// managedVMsOnNode returns the Nimbus-managed VMs Proxmox would migrate.
// "Managed" = a row in the vms table with vmid + node set, status not in
// the pre-Proxmox states (creating/failed). Same filter the network ops
// use.
func (s *Service) managedVMsOnNode(ctx context.Context, node string) ([]db.VM, error) {
	var vms []db.VM
	err := s.db.WithContext(ctx).
		Where("node = ? AND vmid > 0 AND status NOT IN ?", node, []string{"creating", "failed"}).
		Order("vmid ASC").
		Find(&vms).Error
	if err != nil {
		return nil, fmt.Errorf("list managed vms on %s: %w", node, err)
	}
	return vms, nil
}

func (s *Service) loadAllRows(ctx context.Context) (map[string]db.Node, error) {
	var rows []db.Node
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]db.Node, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out, nil
}
