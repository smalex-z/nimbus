package nodemgr_test

import (
	"context"
	"errors"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
)

// seedMigrateVM inserts the VM under test and returns its DB row id.
// Tests vary node + tier; status doesn't matter for ComputeMigratePlan
// (which scores destinations regardless of run state).
func seedMigrateVM(t *testing.T, database *db.DB, node, tier string) uint {
	t.Helper()
	row := db.VM{
		VMID:     200,
		Hostname: "test-vm",
		IP:       "10.0.0.1",
		Node:     node,
		Tier:     tier,
		Status:   "running",
	}
	if err := database.Create(&row).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	return row.ID
}

func TestComputeMigratePlan_PicksEligibleTarget(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 8 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
		{Name: "gamma", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 4 * gib},
	}
	id := seedMigrateVM(t, database, "alpha", "small")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}
	if plan.SourceNode != "alpha" {
		t.Errorf("SourceNode = %q, want alpha", plan.SourceNode)
	}
	if plan.AutoPick == "" {
		t.Errorf("AutoPick is empty — expected a winner among eligible nodes")
	}
	if plan.AutoPick == "alpha" {
		t.Errorf("AutoPick = alpha; source node must not be a candidate")
	}
	// Eligible covers every NON-source node.
	if len(plan.Eligible) != 2 {
		t.Errorf("len(Eligible) = %d, want 2 (cluster has 3 nodes minus source)", len(plan.Eligible))
	}
	// AutoPick should be the highest-scoring option — first in the
	// sorted list (eligible first, then desc by score).
	if len(plan.Eligible) > 0 && plan.Eligible[0].Node != plan.AutoPick {
		t.Errorf("eligible[0]=%s but auto_pick=%s; sort should put winner first",
			plan.Eligible[0].Node, plan.AutoPick)
	}
}

func TestComputeMigratePlan_NoSourceInEligible(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	id := seedMigrateVM(t, database, "alpha", "small")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}
	for _, e := range plan.Eligible {
		if e.Node == "alpha" {
			t.Errorf("source node %q surfaced in Eligible — must be filtered out", e.Node)
		}
	}
}

func TestComputeMigratePlan_VMNotFound(t *testing.T) {
	t.Parallel()
	svc, _, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}

	_, err := svc.ComputeMigratePlan(context.Background(), 9999)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %T %v", err, err)
	}
	if notFound.Resource != "vm" {
		t.Errorf("expected Resource=vm, got %s", notFound.Resource)
	}
}

func TestComputeMigratePlan_UnknownTier(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	id := seedMigrateVM(t, database, "alpha", "weird-tier")

	_, err := svc.ComputeMigratePlan(context.Background(), id)
	var validation *internalerrors.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected ValidationError for unknown tier, got %T %v", err, err)
	}
}

func TestComputeMigratePlan_OfflineCandidateMarkedDisabled(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "offline", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "gamma", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	id := seedMigrateVM(t, database, "alpha", "small")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}
	if plan.AutoPick == "beta" {
		t.Errorf("AutoPick = beta; offline node should not be selected")
	}
	for _, e := range plan.Eligible {
		if e.Node == "beta" && !e.Disabled {
			t.Errorf("beta should be Disabled (status=offline)")
		}
	}
}

func TestComputeMigratePlan_ProjectedRAMReflectsCurrentLoad(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
		{Name: "gamma", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	// Pre-existing committed VMs on beta drag its projection up.
	fake.clusterVMs = []proxmox.ClusterVM{
		{VMID: 50, Node: "beta", Name: "loaded", Status: "running", MaxMem: 8 * gib},
		{VMID: 51, Node: "beta", Name: "also-loaded", Status: "running", MaxMem: 4 * gib},
	}
	id := seedMigrateVM(t, database, "alpha", "small")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}

	var betaPct, gammaPct float64
	for _, e := range plan.Eligible {
		switch e.Node {
		case "beta":
			betaPct = e.ProjectedRAMPct
		case "gamma":
			gammaPct = e.ProjectedRAMPct
		}
	}
	if betaPct <= gammaPct {
		t.Errorf("expected beta projected RAM (%.0f%%) > gamma (%.0f%%); beta is loaded",
			betaPct, gammaPct)
	}
	// Auto pick should prefer the less loaded node.
	if plan.AutoPick != "gamma" {
		t.Errorf("AutoPick = %q, want gamma (less loaded)", plan.AutoPick)
	}
}

// seedTaggedMigrateVM inserts a VM carrying a RequiredTags constraint.
func seedTaggedMigrateVM(t *testing.T, database *db.DB, node, tier, requiredTags string) uint {
	t.Helper()
	row := db.VM{
		VMID:         200,
		Hostname:     "tagged-vm",
		IP:           "10.0.0.1",
		Node:         node,
		Tier:         tier,
		Status:       "running",
		RequiredTags: requiredTags,
	}
	if err := database.Create(&row).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	return row.ID
}

// seedAVX2Nodes writes the db.Node rows whose has_avx2 column drives the
// avx2 auto-tag.
func seedAVX2Nodes(t *testing.T, database *db.DB, avx2ByNode map[string]bool) {
	t.Helper()
	for name, avx2 := range avx2ByNode {
		if err := database.Create(&db.Node{Name: name, CPUModel: "Intel Xeon", HasAVX2: avx2}).Error; err != nil {
			t.Fatalf("seed node %s: %v", name, err)
		}
	}
}

// The migrate planner must apply the VM's host-aggregate filter, not just
// capacity. Before this was wired, the destination dropdown happily
// offered a non-AVX2 node for a VM pinned to cpu=x86-64-v3.
func TestComputeMigratePlan_HonoursRequiredTags(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 8 * gib},
		// beta is the roomiest node, so a capacity-only scorer would
		// auto-pick it. It has no AVX2, so the tag filter must win.
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
		{Name: "gamma", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 4 * gib},
	}
	seedAVX2Nodes(t, database, map[string]bool{"alpha": true, "beta": false, "gamma": true})
	id := seedTaggedMigrateVM(t, database, "alpha", "small", "avx2")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}
	if plan.AutoPick != "gamma" {
		t.Errorf("AutoPick = %q, want gamma (the only AVX2-capable non-source node)", plan.AutoPick)
	}
	for _, e := range plan.Eligible {
		if e.Node == "beta" && !e.Disabled {
			t.Errorf("beta lacks avx2 but was offered as an enabled destination: %+v", e)
		}
	}
}

// With no constraint, the filter must not narrow anything.
func TestComputeMigratePlan_UntaggedVMIgnoresNodeTags(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 8 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	seedAVX2Nodes(t, database, map[string]bool{"alpha": true, "beta": false})
	id := seedMigrateVM(t, database, "alpha", "small")

	plan, err := svc.ComputeMigratePlan(context.Background(), id)
	if err != nil {
		t.Fatalf("ComputeMigratePlan: %v", err)
	}
	if plan.AutoPick != "beta" {
		t.Errorf("AutoPick = %q, want beta (no constraint, most free RAM)", plan.AutoPick)
	}
}
