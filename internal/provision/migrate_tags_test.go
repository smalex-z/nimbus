package provision_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
)

// seedTaggedVM inserts a running VM on "alpha" carrying a RequiredTags
// constraint, so the migrate gate has something to enforce.
func seedTaggedVM(t *testing.T, database *db.DB, requiredTags string) uint {
	t.Helper()
	row := db.VM{
		VMID:         200,
		Hostname:     "tagged-host",
		IP:           "10.0.0.1",
		Node:         "alpha",
		Tier:         "small",
		Status:       "running",
		RequiredTags: requiredTags,
	}
	if err := database.Create(&row).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	return row.ID
}

// seedNodeRows creates the db.Node rows the tag gate reads. avx2 is an
// auto-tag derived from has_avx2, never an operator-typed string, so
// these rows set the hardware column rather than Tags.
func seedNodeRows(t *testing.T, database *db.DB, avx2ByNode map[string]bool) {
	t.Helper()
	for name, avx2 := range avx2ByNode {
		row := db.Node{Name: name, CPUModel: "Intel Xeon", HasAVX2: avx2}
		if err := database.Create(&row).Error; err != nil {
			t.Fatalf("seed node %s: %v", name, err)
		}
	}
}

// The core of the regression: a VM pinned to cpu=x86-64-v3 must never be
// relocated onto a host without the v3 feature set. ComputeMigratePlan
// wouldn't offer beta, but MigrateAdmin takes an operator-supplied
// target_node directly, so the gate has to live here too.
func TestMigrateAdmin_RejectsTargetMissingRequiredTag(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)

	var migrateCalls atomic.Int32
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, _ bool) (string, error) {
		migrateCalls.Add(1)
		return "task:migrate", nil
	}

	svc, _, database := newTestService(t, fake)
	seedNodeRows(t, database, map[string]bool{"alpha": true, "beta": false})
	id := seedTaggedVM(t, database, "avx2")

	_, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError for AVX2-less target, got %T %v", err, err)
	}
	if !strings.Contains(conflict.Error(), "avx2") {
		t.Errorf("conflict message should name the missing tag, got: %s", conflict.Error())
	}
	// Fail closed: Proxmox must never have been asked to move the VM.
	if migrateCalls.Load() != 0 {
		t.Errorf("migrate dispatched despite failing the tag gate (%d calls)", migrateCalls.Load())
	}

	var got db.VM
	if err := database.First(&got, id).Error; err != nil {
		t.Fatalf("reload vm: %v", err)
	}
	if got.Node != "alpha" {
		t.Errorf("VM should still be on alpha, got %s", got.Node)
	}
}

func TestMigrateAdmin_AllowsTargetCarryingRequiredTag(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, _ bool) (string, error) {
		return "task:migrate", nil
	}

	svc, _, database := newTestService(t, fake)
	seedNodeRows(t, database, map[string]bool{"alpha": true, "beta": true})
	id := seedTaggedVM(t, database, "avx2")

	res, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	if err != nil {
		t.Fatalf("MigrateAdmin onto an AVX2-capable target: %v", err)
	}
	if res.TargetNode != "beta" {
		t.Errorf("unexpected target: %s", res.TargetNode)
	}
}

// An unconstrained VM must not pay for the gate — no RequiredTags means
// no node-meta lookup and no new rejection path.
func TestMigrateAdmin_UntaggedVMUnaffectedByGate(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	fake.migrateVM = func(_ context.Context, _ string, _ int, _ string, _ bool) (string, error) {
		return "task:migrate", nil
	}

	svc, _, database := newTestService(t, fake)
	// Deliberately no db.Node rows at all — an untagged VM must migrate
	// even when node metadata is absent entirely.
	id := seedTaggedVM(t, database, "")

	if _, err := svc.MigrateAdmin(context.Background(), id, "beta", false); err != nil {
		t.Fatalf("untagged VM should migrate freely: %v", err)
	}
}

// A node row that has never been observed (no has_avx2 yet) carries no
// avx2 tag, so it must be refused rather than optimistically allowed.
func TestMigrateAdmin_UnobservedTargetIsRefused(t *testing.T) {
	t.Parallel()
	fake := migrateFakePVE(t)
	svc, _, database := newTestService(t, fake)
	seedNodeRows(t, database, map[string]bool{"alpha": true})
	id := seedTaggedVM(t, database, "avx2")

	_, err := svc.MigrateAdmin(context.Background(), id, "beta", false)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError for unobserved target, got %T %v", err, err)
	}
}
