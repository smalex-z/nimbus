package provision_test

import (
	"context"
	"strings"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

func seedManagedVM(t *testing.T, database *db.DB, hostname string, vmid int, ip string) {
	t.Helper()
	if err := database.Create(&db.VM{
		VMID:       vmid,
		Hostname:   hostname,
		IP:         ip,
		Node:       "alpha",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		Status:     "running",
	}).Error; err != nil {
		t.Fatalf("seed vm %s: %v", hostname, err)
	}
}

func TestForceGatewayUpdate_PushesNewGatewayKeepsIP(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, database := newTestService(t, fake)

	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")

	rep, err := svc.ForceGatewayUpdate(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("ForceGatewayUpdate: %v", err)
	}
	if rep.Updated != 2 || len(rep.Failures) != 0 {
		t.Fatalf("rep = %+v; want Updated=2 Failures=0", rep)
	}

	// The last cloud-init call is recorded on the fake — confirm it carries
	// the new gateway with the VM's existing IP.
	got := fake.cloudInitArgs.Load()
	if got == nil {
		t.Fatal("no SetCloudInit call recorded")
	}
	if !strings.Contains(got.IPConfig0, "gw=10.0.0.99") {
		t.Errorf("ipconfig0 = %q, want gw=10.0.0.99", got.IPConfig0)
	}
	if !strings.Contains(got.IPConfig0, "ip=10.0.0.3") {
		t.Errorf("ipconfig0 = %q, want ip=10.0.0.3 (vm-b's existing IP)", got.IPConfig0)
	}
}

func TestForceGatewayUpdate_RejectsEmptyGateway(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	if _, err := svc.ForceGatewayUpdate(context.Background(), ""); err == nil {
		t.Error("expected error for empty gateway, got nil")
	}
}

func TestForceGatewayUpdate_PerVMFailureDoesNotAbortBatch(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// First call fails, second succeeds — batch should report 1/1.
	calls := 0
	fake.setCloudInit = func(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
		calls++
		if calls == 1 {
			return errAPIDown
		}
		return nil
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")

	rep, err := svc.ForceGatewayUpdate(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("ForceGatewayUpdate: %v", err)
	}
	if rep.Updated != 1 || len(rep.Failures) != 1 {
		t.Errorf("rep = %+v; want Updated=1 Failures=1", rep)
	}
}

func TestRenumberAllVMs_RejectsWhenPoolTooSmall(t *testing.T) {
	t.Parallel()
	svc, pool, database := newTestService(t, happyFakePVE(t))
	// Pool has 5 free addresses; allocate 3 of them so only 2 are free,
	// then seed 3 VMs — pool capacity is 2, demand is 3 → should refuse.
	for i := 0; i < 3; i++ {
		if _, err := pool.Reserve(context.Background(), "filler"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")
	seedManagedVM(t, database, "vm-c", 202, "10.0.0.4")

	if _, err := svc.RenumberAllVMs(context.Background(), "10.0.0.1"); err == nil {
		t.Error("expected error when pool capacity < vm count, got nil")
	}
}

func TestRenumberAllVMs_HappyPathReassignsIPsAndUpdatesDB(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")

	rep, err := svc.RenumberAllVMs(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("RenumberAllVMs: %v", err)
	}
	if rep.Updated != 2 || len(rep.Failures) != 0 {
		t.Fatalf("rep = %+v; want Updated=2 Failures=0", rep)
	}

	// VM rows should now show fresh IPs from the pool, distinct from the
	// originals. Pool was seeded 10.0.0.1..5; existing VMs held .2 and .3.
	// After Reserve cycles, the lowest-free addresses get handed out — at
	// minimum the new IPs differ from the old ones.
	var vms []db.VM
	if err := database.WithContext(context.Background()).Order("id ASC").Find(&vms).Error; err != nil {
		t.Fatalf("list vms: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("vm count = %d, want 2", len(vms))
	}
	if vms[0].IP == "10.0.0.2" && vms[1].IP == "10.0.0.3" {
		t.Errorf("VMs still hold their original IPs (%s, %s) — renumber did not write back", vms[0].IP, vms[1].IP)
	}
}

// errAPIDown is a sentinel for the per-VM-failure batch test.
var errAPIDown = &fakeAPIErr{msg: "proxmox unreachable"}

type fakeAPIErr struct{ msg string }

func (e *fakeAPIErr) Error() string { return e.msg }
