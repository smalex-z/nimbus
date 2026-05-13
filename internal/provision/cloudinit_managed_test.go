package provision_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"nimbus/internal/provision"
)

// SweepLegacyCIData over an empty fleet must return zero counts without
// touching Proxmox — no rows in the local DB means there's nothing to
// sweep.
func TestSweepLegacyCIData_EmptyFleet(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	var cfgCalls atomic.Int32
	fake.getVMConfig = func(ctx context.Context, n string, vmid int) (map[string]any, error) {
		cfgCalls.Add(1)
		return map[string]any{}, nil
	}

	svc, _, _ := newTestService(t, fake)

	got, err := svc.SweepLegacyCIData(context.Background())
	if err != nil {
		t.Fatalf("SweepLegacyCIData: %v", err)
	}
	if got.Total != 0 || got.OK != 0 || len(got.Failed) != 0 {
		t.Errorf("result = %+v, want zero-valued", got)
	}
	if cfgCalls.Load() != 0 {
		t.Errorf("GetVMConfig called %d times on empty fleet, want 0", cfgCalls.Load())
	}
}

// Mixed fleet exercises every branch the sweep cares about:
//   - ide2 already a managed cloudinit drive → no-op, counted OK.
//   - ide2 empty → attach managed drive, counted OK.
//   - GetVMConfig fails → per-VM failure, recorded in Failed, loop continues.
//   - ide2 holds a non-Nimbus drive → refused by swap, recorded in Failed.
func TestSweepLegacyCIData_MixedFleetTalliesAndContinuesOnError(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)

	// Per-VM config keyed by vmid so each row hits a different code path.
	configs := map[int]map[string]any{
		201: {"ide2": "local-lvm:cloudinit"},                          // already managed
		202: {"scsi0": "local-lvm:vm-202-disk-0,size=10G"},            // empty ide2 → attach
		204: {"ide2": "local:iso/some-user-attached.iso,media=cdrom"}, // foreign ide2 → swap refuses
	}
	fake.getVMConfig = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid == 203 {
			return nil, errors.New("proxmox 500: node offline")
		}
		cfg, ok := configs[vmid]
		if !ok {
			return map[string]any{}, nil
		}
		return cfg, nil
	}

	var detachCalls, attachCalls atomic.Int32
	fake.detachDrive = func(_ context.Context, _ string, _ int, _ string) error {
		detachCalls.Add(1)
		return nil
	}
	fake.attachCloudInitDrive = func(_ context.Context, _ string, _ int, _, _ string) error {
		attachCalls.Add(1)
		return nil
	}

	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "already-managed", 201, "10.0.0.1")
	seedManagedVM(t, database, "empty-ide2", 202, "10.0.0.2")
	seedManagedVM(t, database, "config-error", 203, "10.0.0.3")
	seedManagedVM(t, database, "foreign-drive", 204, "10.0.0.4")

	got, err := svc.SweepLegacyCIData(context.Background())
	if err != nil {
		t.Fatalf("SweepLegacyCIData: %v", err)
	}
	if got.Total != 4 {
		t.Errorf("Total = %d, want 4", got.Total)
	}
	if got.OK != 2 {
		t.Errorf("OK = %d, want 2 (managed-already + empty-ide2)", got.OK)
	}
	if len(got.Failed) != 2 {
		t.Fatalf("Failed = %d entries, want 2 (config-error + foreign-drive)", len(got.Failed))
	}

	gotFailedVMIDs := map[int]string{}
	for _, f := range got.Failed {
		gotFailedVMIDs[f.VMID] = f.Error
	}
	if _, ok := gotFailedVMIDs[203]; !ok {
		t.Errorf("expected vmid=203 in Failed (GetVMConfig error), got %+v", got.Failed)
	}
	if _, ok := gotFailedVMIDs[204]; !ok {
		t.Errorf("expected vmid=204 in Failed (foreign ide2), got %+v", got.Failed)
	}

	// Only vmid=202 (empty ide2) reaches the attach step. The already-
	// managed VM short-circuits before any drive op; the foreign-ide2
	// VM is refused before detach; the config-error VM never gets past
	// GetVMConfig.
	if got := attachCalls.Load(); got != 1 {
		t.Errorf("AttachCloudInitDrive called %d times, want 1 (only vmid=202 attaches)", got)
	}
	if got := detachCalls.Load(); got != 0 {
		t.Errorf("DetachDrive called %d times, want 0 (no legacy Nimbus ISO present in fixtures)", got)
	}
}

// Cluster-level errors (e.g. DB list failure) propagate as the function
// error — the operator needs to retry. Per-VM failures must NOT escalate
// to this level.
func TestSweepLegacyCIData_DBListFailurePropagates(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	// Close the underlying DB to force list error.
	sqlDB, err := database.DB.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err = svc.SweepLegacyCIData(context.Background())
	if err == nil {
		t.Fatal("expected error from closed DB, got nil")
	}
	if want := "list vms"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to mention %q", err.Error(), want)
	}
}

// The actual user scenario: a pre-D-boot VM with a per-node Nimbus
// cidata ISO at ide2. Sweep must detach the ISO, delete the file, and
// attach a managed cloudinit drive so the VM is migratable.
func TestSweepLegacyCIData_ConvertsLegacyNimbusISO(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)

	const vmid = 101
	fake.getVMConfig = func(_ context.Context, _ string, gotVMID int) (map[string]any, error) {
		return map[string]any{
			"ide2":  fmt.Sprintf("local:iso/nimbus-vm-%d.iso,media=cdrom", gotVMID),
			"scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,size=10G", gotVMID),
		}, nil
	}

	var detachCalls, attachCalls, deleteCalls atomic.Int32
	var deletedVolids []string
	var attachStorage string
	fake.detachDrive = func(_ context.Context, _ string, _ int, slot string) error {
		if slot != "ide2" {
			t.Errorf("DetachDrive slot = %q, want ide2", slot)
		}
		detachCalls.Add(1)
		return nil
	}
	fake.deleteVolume = func(_ context.Context, _, volid string) error {
		deleteCalls.Add(1)
		deletedVolids = append(deletedVolids, volid)
		return nil
	}
	fake.attachCloudInitDrive = func(_ context.Context, _ string, _ int, slot, storage string) error {
		if slot != "ide2" {
			t.Errorf("AttachCloudInitDrive slot = %q, want ide2", slot)
		}
		attachStorage = storage
		attachCalls.Add(1)
		return nil
	}

	svc, _, database := newTestServiceOpts(t, fake, func(c *provision.Config) {
		// CIDataStorage = "local" lets the swap recognize "local:iso/nimbus-vm-{vmid}.iso"
		// as the Nimbus pattern and detach + delete it.
		c.CIDataStorage = "local"
	})
	seedManagedVM(t, database, "legacy-vm", vmid, "10.0.0.1")

	got, err := svc.SweepLegacyCIData(context.Background())
	if err != nil {
		t.Fatalf("SweepLegacyCIData: %v", err)
	}
	if got.OK != 1 || len(got.Failed) != 0 {
		t.Fatalf("result = %+v, want OK=1 Failed=0", got)
	}
	if detachCalls.Load() != 1 || deleteCalls.Load() != 1 || attachCalls.Load() != 1 {
		t.Errorf("calls: detach=%d delete=%d attach=%d, want each=1",
			detachCalls.Load(), deleteCalls.Load(), attachCalls.Load())
	}
	if want := fmt.Sprintf("local:iso/nimbus-vm-%d.iso", vmid); len(deletedVolids) != 1 || deletedVolids[0] != want {
		t.Errorf("DeleteStorageVolume volid = %v, want [%q]", deletedVolids, want)
	}
	// Managed drive must land on the boot-disk storage (local-lvm,
	// derived from the scsi0 fixture), not the cidata storage.
	if attachStorage != "local-lvm" {
		t.Errorf("AttachCloudInitDrive storage = %q, want local-lvm (boot disk storage)", attachStorage)
	}
}
