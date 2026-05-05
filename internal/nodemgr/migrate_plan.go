package nodemgr

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/nodescore"
)

// MigratePlan is the placement preview for moving a single VM. The
// migrate modal renders Eligible as the destination dropdown — same
// vocabulary as drain's per-row Eligible, but evaluated for one VM
// with no other migrations in flight (no plannedAdd accumulation).
// AutoPick is the highest-scoring eligible target — what nodescore
// would select if this were a fresh placement.
//
// Source isn't a candidate (you can't migrate a VM to where it
// already is); the SPA renders the source's "RAM after this VM
// leaves" stat client-side from listNodes data, so it's not part of
// this payload.
type MigratePlan struct {
	VMID       int              `json:"vm_id"`
	VMRowID    uint             `json:"vm_row_id"`
	Hostname   string           `json:"hostname"`
	Tier       string           `json:"tier"`
	SourceNode string           `json:"source_node"`
	AutoPick   string           `json:"auto_pick"`
	Eligible   []EligibleTarget `json:"eligible"`
}

// ComputeMigratePlan scores every cluster node for a single-VM
// migration. Mirrors ComputePlan's per-row evaluation but skips the
// drain-specific accumulation (plannedAdd) so each candidate's score
// + projected RAM% reflects "this VM lands here, nothing else moves."
//
// The handler maps gorm.ErrRecordNotFound to a 404; unknown-tier and
// other in-band failures surface as wrapped errors. Like ComputePlan,
// the disk gate stays permissive (storeRows is intentionally unused)
// — the actual migrate call is the authoritative fail-fast for
// destination-storage incompatibility.
func (s *Service) ComputeMigratePlan(ctx context.Context, vmID uint) (*MigratePlan, error) {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, vmID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", vmID)}
		}
		return nil, fmt.Errorf("get vm %d: %w", vmID, err)
	}

	tier, ok := nodescore.Tiers[vm.Tier]
	if !ok {
		return nil, &internalerrors.ValidationError{Field: "tier", Message: fmt.Sprintf("unknown tier %q", vm.Tier)}
	}

	nodes, vms, _, err := s.clusterSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	dbNodes, err := s.loadAllRows(ctx)
	if err != nil {
		return nil, err
	}

	// Candidates = every node OTHER than the source. Same filter
	// ComputePlan uses — Proxmox can't migrate a VM to where it
	// already runs, and surfacing the source as "current node"
	// adds noise without payoff.
	candidates := make([]nodescore.Node, 0, len(nodes)-1)
	templatesPresent := make(map[string]bool, len(nodes))
	maxMemByNode := make(map[string]uint64, len(nodes))
	for _, n := range nodes {
		maxMemByNode[n.Name] = n.MaxMem
		if n.Name == vm.Node {
			continue
		}
		// "Templates present" is irrelevant to migration — Proxmox
		// moves the disk, no clone happens. Bypass the gate uniformly.
		templatesPresent[n.Name] = true
		row := dbNodes[n.Name]
		candidates = append(candidates, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
			LockState: lockOrNone(row.LockState),
		})
	}

	// Per-candidate runtime: current state of the cluster, NO
	// plannedAdd accumulation. This is the key difference from
	// ComputePlan — drain's projected RAM% reflects all VMs migrating
	// per their auto_pick; for a single-VM move the honest read is
	// "what does it look like with just this one landing?"
	committedMem := make(map[string]uint64, len(nodes))
	currentVMCount := make(map[string]int, len(nodes))
	for _, v := range vms {
		if v.Template != 0 {
			continue
		}
		committedMem[v.Node] += v.MaxMem
		currentVMCount[v.Node]++
	}
	runtime := make(map[string]nodescore.NodeRuntime, len(candidates))
	for _, c := range candidates {
		runtime[c.Name] = nodescore.NodeRuntime{
			VMCount:           currentVMCount[c.Name],
			CommittedMemBytes: committedMem[c.Name],
		}
	}

	env := nodescore.Env{
		TemplatesPresent: templatesPresent,
		// StorageByNode left nil — same disk-gate-permissive stance
		// the drain plan takes.
	}
	decisions := nodescore.Evaluate(candidates, runtime, tier, env)
	winner, _ := nodescore.Pick(decisions)

	plan := &MigratePlan{
		VMID:       vm.VMID,
		VMRowID:    vm.ID,
		Hostname:   vm.Hostname,
		Tier:       vm.Tier,
		SourceNode: vm.Node,
		Eligible: buildEligible(
			decisions,
			maxMemByNode,
			committedMem,
			map[string]uint64{}, // no plannedAdd — single-VM move
			tier,
		),
	}
	if winner != nil {
		plan.AutoPick = winner.Node.Name
	}
	return plan, nil
}
