package provision_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// proxmoxNotRunningErr fakes the 500 body Proxmox returns when reboot is
// called against a stopped VM. The network-ops code should treat this as a
// no-op (cloud-init applies on next boot anyway).
var proxmoxNotRunningErr = &proxmox.HTTPError{
	Status: 500,
	Method: "POST",
	Path:   "/nodes/x/qemu/200/status/reboot",
	Body:   `{"data":null,"message":"VM 200 not running\n"}`,
}

// proxmoxConfigMissingErr fakes the 500 body Proxmox returns when the qemu
// config file is gone — the VM has drifted off this node.
var proxmoxConfigMissingErr = &proxmox.HTTPError{
	Status: 500,
	Method: "POST",
	Path:   "/nodes/x/qemu/200/config",
	Body:   `{"message":"Configuration file 'nodes/x/qemu-server/200.conf' does not exist\n","data":null}`,
}

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

	if _, err := svc.RenumberAllVMs(context.Background(), "10.0.0.1", "10.0.0.1", "10.0.0.5"); err == nil {
		t.Error("expected error when pool capacity < vm count, got nil")
	}
}

func TestRenumberAllVMs_DropsStrandedOldIPsSoTheyDoNotPolluteReserve(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, pool, database := newTestService(t, fake)

	// newTestService seeds the pool with 10.0.0.1..5. To simulate the user's
	// scenario, adopt a stranded "old-pool" address (192.168.0.151) into the
	// table the same way Reseed would have left it after a pool migration.
	// First we have to inject the row.
	if err := pool.Seed(context.Background(), "192.168.0.151", "192.168.0.151"); err != nil {
		t.Fatalf("seed stranded: %v", err)
	}
	// Allocate it to a VM that's about to be renumbered.
	seedManagedVM(t, database, "vm-stranded", 200, "192.168.0.151")
	if err := pool.AdoptAllocation(context.Background(), "192.168.0.151", 200, "vm-stranded"); err != nil {
		t.Fatalf("adopt stranded: %v", err)
	}

	// Renumber into the new pool [10.0.0.1, 10.0.0.5]. With the bug, the loop
	// would Release 192.168.0.151 back to free, and a *subsequent* Reserve
	// would pick it (lex < 10.0.0.x in 4-byte comparison? actually 10.0.0.x <
	// 192.168.0.x, so this case wouldn't reproduce — flip ranges to match the
	// real-world incident).
	// The user hit it with pool 192.168.50.* and stranded 192.168.0.* which
	// IS lex-lower than 192.168.50.* so freed-stranded-row WAS picked up.
	// Use that exact shape here:
	if _, _, _, err := pool.Reseed(context.Background(), "192.168.50.1", "192.168.50.5"); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	rep, err := svc.RenumberAllVMs(context.Background(), "192.168.50.1", "192.168.50.1", "192.168.50.5")
	if err != nil {
		t.Fatalf("RenumberAllVMs: %v", err)
	}
	if rep.Updated != 1 || len(rep.Failures) != 0 {
		t.Fatalf("rep = %+v; want Updated=1 Failures=0", rep)
	}

	// The stranded row must be GONE from the pool table — not lingering as
	// status=free where the next Reserve would pick it up.
	if _, err := pool.GetByIP(context.Background(), "192.168.0.151"); err == nil {
		t.Errorf("192.168.0.151 still in pool — should have been dropped on renumber")
	}

	// And the VM should now hold a 192.168.50.* address.
	var got db.VM
	if err := database.WithContext(context.Background()).First(&got, "vmid = ?", 200).Error; err != nil {
		t.Fatalf("re-read vm: %v", err)
	}
	if got.IP == "192.168.0.151" {
		t.Errorf("vm.IP = %q, expected to have moved into the new pool", got.IP)
	}
	if !strings.HasPrefix(got.IP, "192.168.50.") {
		t.Errorf("vm.IP = %q, want a 192.168.50.x address from the new pool", got.IP)
	}
}

func TestRenumberAllVMs_HappyPathReassignsIPsAndUpdatesDB(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")

	rep, err := svc.RenumberAllVMs(context.Background(), "10.0.0.1", "10.0.0.1", "10.0.0.5")
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

func TestForceGatewayUpdate_StoppedVMCountsAsSuccess(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Proxmox refuses to reboot a stopped VM with 500 + "not running" body.
	// The cloud-init drive is already updated; the VM picks up the new
	// gateway on next boot, so this should NOT count as a failure.
	fake.rebootVM = func(_ context.Context, _ string, _ int) (string, error) {
		return "", proxmoxNotRunningErr
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-a", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-b", 201, "10.0.0.3")

	rep, err := svc.ForceGatewayUpdate(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("ForceGatewayUpdate: %v", err)
	}
	if rep.Updated != 2 || len(rep.Failures) != 0 {
		t.Errorf("rep = %+v; want Updated=2 Failures=0 (stopped VMs are success)", rep)
	}
}

func TestForceGatewayUpdate_VMNotPresentOnNodeSurfacesCleanMessage(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.setCloudInit = func(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
		return proxmoxConfigMissingErr
	}
	svc, _, database := newTestService(t, fake)
	seedManagedVM(t, database, "vm-stale", 200, "10.0.0.2")

	rep, err := svc.ForceGatewayUpdate(context.Background(), "10.0.0.99")
	if err != nil {
		t.Fatalf("ForceGatewayUpdate: %v", err)
	}
	if rep.Updated != 0 || len(rep.Failures) != 1 {
		t.Fatalf("rep = %+v; want Updated=0 Failures=1", rep)
	}
	got := rep.Failures[0].Err
	if !strings.Contains(got, "vm not present on node") || !strings.Contains(got, "out of sync") {
		t.Errorf("failure message = %q, want clean drift message", got)
	}
}

// TestForceGatewayUpdate_HungVMTimesOutAndBatchContinues guards the bug that
// motivated NetworkOpPerVMTimeout: a single VM whose Proxmox reboot task hangs
// indefinitely must not wedge the entire batch. The first VM's WaitForTask
// blocks on the per-VM ctx; once the deadline fires, the loop records a
// failure and proceeds to the second VM.
func TestForceGatewayUpdate_HungVMTimesOutAndBatchContinues(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	var calls atomic.Int32
	fake.waitForTask = func(ctx context.Context, _, _ string, _ time.Duration) error {
		// First VM: simulate a Proxmox task stuck in "running" forever — block
		// until the per-VM ctx expires. Second VM: return immediately.
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	svc, _, database := newTestServiceOpts(t, fake, func(c *provision.Config) {
		c.NetworkOpPerVMTimeout = 50 * time.Millisecond
	})
	seedManagedVM(t, database, "vm-stuck", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-ok", 201, "10.0.0.3")

	done := make(chan struct{})
	var rep provision.NetworkOpReport
	var opErr error
	go func() {
		rep, opErr = svc.ForceGatewayUpdate(context.Background(), "10.0.0.99")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ForceGatewayUpdate hung — per-VM timeout did not unwedge the batch")
	}
	if opErr != nil {
		t.Fatalf("ForceGatewayUpdate: %v", opErr)
	}
	if rep.Updated != 1 || len(rep.Failures) != 1 {
		t.Fatalf("rep = %+v; want Updated=1 Failures=1 (vm-stuck times out, vm-ok succeeds)", rep)
	}
	if rep.Failures[0].VMID != 200 {
		t.Errorf("failure vmid = %d, want 200 (vm-stuck)", rep.Failures[0].VMID)
	}
	if !strings.Contains(rep.Failures[0].Err, "reboot") {
		t.Errorf("failure msg = %q, want reboot-related", rep.Failures[0].Err)
	}
}

// TestRenumberAllVMs_HungVMTimesOutAndBatchContinues is the renumber-side
// analogue: a stuck reboot on VM 1 must not stop the renumber for VM 2.
// Cloud-init was already written for VM 1, so the renumber accepts it as
// "config saved, IP not yet active" and still increments Updated.
func TestRenumberAllVMs_HungVMTimesOutAndBatchContinues(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	var calls atomic.Int32
	fake.waitForTask = func(ctx context.Context, _, _ string, _ time.Duration) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	svc, _, database := newTestServiceOpts(t, fake, func(c *provision.Config) {
		c.NetworkOpPerVMTimeout = 50 * time.Millisecond
	})
	seedManagedVM(t, database, "vm-stuck", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-ok", 201, "10.0.0.3")

	done := make(chan struct{})
	var rep provision.NetworkOpReport
	var opErr error
	go func() {
		rep, opErr = svc.RenumberAllVMs(context.Background(), "10.0.0.1", "10.0.0.1", "10.0.0.5")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RenumberAllVMs hung — per-VM timeout did not unwedge the batch")
	}
	if opErr != nil {
		t.Fatalf("RenumberAllVMs: %v", opErr)
	}
	// Both VMs get their cloud-init drive rewritten — vm-stuck's reboot poll
	// times out (logged, not a failure), vm-ok's reboot returns cleanly.
	if rep.Updated != 2 || len(rep.Failures) != 0 {
		t.Fatalf("rep = %+v; want Updated=2 Failures=0 (reboot timeout is non-fatal once cloud-init landed)", rep)
	}
	// Both VM rows must show fresh IPs.
	var got []db.VM
	if err := database.WithContext(context.Background()).Order("id ASC").Find(&got).Error; err != nil {
		t.Fatalf("list vms: %v", err)
	}
	if got[0].IP == "10.0.0.2" || got[1].IP == "10.0.0.3" {
		t.Errorf("vm IPs still original: %s, %s — renumber did not write back", got[0].IP, got[1].IP)
	}
}

// TestRenumberAllVMs_HungSetCloudInitTimesOutAsFailure covers the case where
// the per-VM deadline fires while SetCloudInit itself is still blocking — the
// cloud-init drive was *not* written, so the iteration is a clean failure
// (reservation rolled back) and the next VM still runs.
func TestRenumberAllVMs_HungSetCloudInitTimesOutAsFailure(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	var calls atomic.Int32
	fake.setCloudInit = func(ctx context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	svc, pool, database := newTestServiceOpts(t, fake, func(c *provision.Config) {
		c.NetworkOpPerVMTimeout = 50 * time.Millisecond
	})
	seedManagedVM(t, database, "vm-stuck", 200, "10.0.0.2")
	seedManagedVM(t, database, "vm-ok", 201, "10.0.0.3")

	rep, err := svc.RenumberAllVMs(context.Background(), "10.0.0.1", "10.0.0.1", "10.0.0.5")
	if err != nil {
		t.Fatalf("RenumberAllVMs: %v", err)
	}
	if rep.Updated != 1 || len(rep.Failures) != 1 {
		t.Fatalf("rep = %+v; want Updated=1 Failures=1", rep)
	}
	if rep.Failures[0].VMID != 200 || !strings.Contains(rep.Failures[0].Err, "set cloud-init") {
		t.Errorf("failure = %+v; want vmid=200 with set cloud-init message", rep.Failures[0])
	}
	// The new IP reserved for vm-stuck must have been rolled back so it's
	// available for the next reserve / not stranded as 'reserved' for the
	// reservation TTL.
	free, err := pool.CountFree(context.Background())
	if err != nil {
		t.Fatalf("CountFree: %v", err)
	}
	// Pool 10.0.0.1..5 = 5 addrs, .1 = gateway (allocated by Seed? no — Seed
	// just stocks the range). After renumber: vm-ok holds one, vm-stuck has
	// none. The reservation for the failed VM must have been released.
	if free < 3 {
		t.Errorf("free count = %d after rollback, expected ≥3 (failed reservation should have been released)", free)
	}
}
