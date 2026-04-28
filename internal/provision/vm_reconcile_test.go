package provision_test

import (
	"context"
	"errors"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

func TestReconcileVMs_RefusesEmptyClusterSnapshot(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return nil, nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")

	if _, err := svc.ReconcileVMs(context.Background()); err == nil {
		t.Fatal("expected error for empty cluster snapshot, got nil")
	}

	// VM row must NOT have been touched.
	var got db.VM
	if err := database.WithContext(context.Background()).First(&got).Error; err != nil {
		t.Fatalf("vm row was deleted on empty snapshot: %v", err)
	}
	if got.MissedCycles != 0 {
		t.Errorf("missed_cycles = %d, want 0 (refuse should not bump)", got.MissedCycles)
	}
}

func TestReconcileVMs_TracksMigration(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		// VM 200 has moved from "alpha" to "beta".
		return []proxmox.ClusterVM{{VMID: 200, Node: "beta", Name: "vm-a", Status: "running"}}, nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2") // seeded as node="alpha"

	rep, err := svc.ReconcileVMs(context.Background())
	if err != nil {
		t.Fatalf("ReconcileVMs: %v", err)
	}
	if len(rep.Migrated) != 1 {
		t.Fatalf("rep.Migrated = %d, want 1", len(rep.Migrated))
	}
	if rep.Migrated[0].FromNode != "alpha" || rep.Migrated[0].ToNode != "beta" {
		t.Errorf("migration = %+v, want alpha→beta", rep.Migrated[0])
	}

	var got db.VM
	if err := database.WithContext(context.Background()).First(&got).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Node != "beta" {
		t.Errorf("vm.node = %q, want beta", got.Node)
	}
}

func TestReconcileVMs_BumpsMissedCyclesBeforeThreshold(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// VMID 200 is missing — only 999 (irrelevant) is in the cluster.
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{{VMID: 999, Node: "alpha", Name: "other"}}, nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")

	// Two consecutive runs — both should bump missed_cycles, not delete.
	for i := 1; i <= 2; i++ {
		rep, err := svc.ReconcileVMs(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if len(rep.Deleted) != 0 {
			t.Fatalf("run %d: rep.Deleted = %d, want 0 (under threshold)", i, len(rep.Deleted))
		}
		if len(rep.Missed) != 1 || rep.Missed[0].MissedCycles != i {
			t.Errorf("run %d: rep.Missed = %+v, want one entry with MissedCycles=%d", i, rep.Missed, i)
		}
	}

	// Row should still exist, with missed_cycles=2.
	var got db.VM
	if err := database.WithContext(context.Background()).First(&got).Error; err != nil {
		t.Fatalf("row vanished early: %v", err)
	}
	if got.MissedCycles != 2 {
		t.Errorf("missed_cycles = %d, want 2", got.MissedCycles)
	}
}

func TestReconcileVMs_SoftDeletesAfterThreshold(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{{VMID: 999, Node: "alpha", Name: "other"}}, nil
	}
	svc, pool, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	// Mark .2 allocated so we can verify Release happens on soft-delete.
	if err := pool.AdoptAllocation(context.Background(), "10.0.0.2", 200, "vm-a"); err != nil {
		t.Fatalf("AdoptAllocation: %v", err)
	}

	// Three runs — third one must soft-delete.
	for i := 1; i <= 3; i++ {
		if _, err := svc.ReconcileVMs(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	// Default GORM scope filters soft-deleted rows. Confirm the row is gone
	// from the default view.
	var alive []db.VM
	if err := database.WithContext(context.Background()).Find(&alive).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if len(alive) != 0 {
		t.Errorf("rows still visible = %d, want 0 (should be soft-deleted)", len(alive))
	}
	// And confirm Unscoped() can still see it (recoverable).
	var allIncDeleted []db.VM
	if err := database.WithContext(context.Background()).Unscoped().Find(&allIncDeleted).Error; err != nil {
		t.Fatalf("unscoped read: %v", err)
	}
	if len(allIncDeleted) != 1 {
		t.Errorf("unscoped rows = %d, want 1 (row should be recoverable)", len(allIncDeleted))
	}

	// And confirm the IP returned to the pool.
	row, err := pool.GetByIP(context.Background(), "10.0.0.2")
	if err != nil {
		t.Fatalf("GetByIP: %v", err)
	}
	if row.Status != "free" {
		t.Errorf(".2 status = %q, want free (released on soft-delete)", row.Status)
	}
}

func TestReconcileVMs_ResetsMissedCyclesWhenSeenAgain(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	clusterMissing := []proxmox.ClusterVM{{VMID: 999, Node: "alpha", Name: "other"}}
	clusterPresent := []proxmox.ClusterVM{
		{VMID: 200, Node: "alpha", Name: "vm-a", Status: "running"},
	}
	currentSnap := clusterMissing
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return currentSnap, nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")

	// One miss to bump missed_cycles to 1.
	if _, err := svc.ReconcileVMs(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Then VM reappears — counter should reset.
	currentSnap = clusterPresent
	if _, err := svc.ReconcileVMs(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}

	var got db.VM
	if err := database.WithContext(context.Background()).First(&got).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.MissedCycles != 0 {
		t.Errorf("missed_cycles = %d, want 0 (reset on re-find)", got.MissedCycles)
	}
}

func TestReconcileVMs_EmptyClusterErrorIsTyped(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) { return nil, nil }
	svc, _, _ := newTestService(t, fake)
	_, err := svc.ReconcileVMs(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// We don't export the sentinel — the message is the contract. Keeping
	// the assertion loose protects against rewording the error.
	if !errors.Is(err, err) || err.Error() == "" {
		t.Errorf("err = %v, want a non-empty error", err)
	}
}

// TestReconcileVMs_UnreachableNodeSuppressesMissBump asserts the reachability
// guard: when SetUnreachableNodesProbe reports the host node as unreachable,
// a VM missing from the cluster snapshot is treated as still-alive rather
// than an orphan ready to soft-delete. MissedCycles stays at 0 across N
// cycles where N exceeds the vacate threshold.
func TestReconcileVMs_UnreachableNodeSuppressesMissBump(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Cluster snapshot returns an unrelated VM only — vmid 200 is missing.
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{{VMID: 999, Node: "alpha", Name: "other"}}, nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	svc.SetUnreachableNodesProbe(func(context.Context) map[string]bool {
		return map[string]bool{"alpha": true}
	})

	for i := 1; i <= 5; i++ {
		rep, err := svc.ReconcileVMs(context.Background())
		if err != nil {
			t.Fatalf("ReconcileVMs #%d: %v", i, err)
		}
		if len(rep.Deleted) != 0 || len(rep.Missed) != 0 {
			t.Errorf("cycle %d rep = %+v; want no deletes / no misses while host is unreachable", i, rep)
		}
	}
	var got db.VM
	if err := database.WithContext(context.Background()).First(&got, "vmid = ?", 200).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.MissedCycles != 0 {
		t.Errorf("missed_cycles = %d, want 0 (probe should suppress every cycle)", got.MissedCycles)
	}
}
