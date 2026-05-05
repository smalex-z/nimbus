package nodemgr_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/nodemgr"
	"nimbus/internal/proxmox"
)

const gib = uint64(1 << 30)

// fakePVE implements nodemgr.ProxmoxClient with operator-controllable
// state. Lets tests script "first call returns X, second call returns Y"
// behaviour for re-validation race coverage.
type fakePVE struct {
	mu sync.Mutex

	nodes        []proxmox.Node
	clusterVMs   []proxmox.ClusterVM
	clusterStore []proxmox.ClusterStorage
	addresses    map[string]string
	clusterName  string
	version      string

	migrations    []migrateCall
	migrateErr    error
	deleteNodeErr error

	// Hook fired right after each MigrateVM call returns. Tests use it
	// to mutate cluster state mid-batch (e.g. fill a node) so the next
	// migration's re-validation triggers.
	afterMigrate func(call migrateCall)
}

type migrateCall struct {
	Source string
	VMID   int
	Target string
	Online bool
}

func (f *fakePVE) GetNodes(_ context.Context) ([]proxmox.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proxmox.Node, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}

func (f *fakePVE) GetClusterVMs(_ context.Context) ([]proxmox.ClusterVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proxmox.ClusterVM, len(f.clusterVMs))
	copy(out, f.clusterVMs)
	return out, nil
}

func (f *fakePVE) GetClusterStorage(_ context.Context) ([]proxmox.ClusterStorage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]proxmox.ClusterStorage, len(f.clusterStore))
	copy(out, f.clusterStore)
	return out, nil
}

func (f *fakePVE) GetNodeStatus(_ context.Context, _ string) (*proxmox.NodeStatus, error) {
	return &proxmox.NodeStatus{}, nil
}

func (f *fakePVE) ListDisks(_ context.Context, _ string) ([]proxmox.Disk, error) {
	return nil, nil
}

func (f *fakePVE) ListPCIDevices(_ context.Context, _ string) ([]proxmox.PCIDevice, error) {
	return nil, nil
}

func (f *fakePVE) NodeAddresses(_ context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.addresses))
	for k, v := range f.addresses {
		out[k] = v
	}
	return out, nil
}

func (f *fakePVE) ClusterName(_ context.Context) (string, error) { return f.clusterName, nil }
func (f *fakePVE) Version(_ context.Context) (string, error)     { return f.version, nil }

func (f *fakePVE) GetClusterStatus(_ context.Context) ([]proxmox.ClusterStatusEntry, error) {
	// Fakes don't drive Binding tests; empty slice is enough to satisfy
	// the interface so List/ComputePlan/Execute paths compile.
	return nil, nil
}

func (f *fakePVE) MigrateVM(_ context.Context, source string, vmid int, target string, online bool) (string, error) {
	f.mu.Lock()
	call := migrateCall{Source: source, VMID: vmid, Target: target, Online: online}
	f.migrations = append(f.migrations, call)
	err := f.migrateErr
	hook := f.afterMigrate
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	if hook != nil {
		hook(call)
	}
	return "", nil // empty UPID skips WaitForTask
}

func (f *fakePVE) WaitForTask(_ context.Context, _, _ string, _ time.Duration) error { return nil }

func (f *fakePVE) DeleteNode(_ context.Context, _ string) error { return f.deleteNodeErr }

// newTestService spins up a temp SQLite DB + nodemgr.Service. Returns the
// service, the underlying *db.DB (so tests can seed db.VM rows), and the
// fake Proxmox client.
func newTestService(t *testing.T) (*nodemgr.Service, *db.DB, *fakePVE) {
	return newTestServiceWithStorage(t, "")
}

// newTestServiceWithStorage builds the service with a configured
// VMDiskStorage pool so the drain plan's disk gate fires. Empty string
// disables the gate (matches the legacy default).
func newTestServiceWithStorage(t *testing.T, pool string) (*nodemgr.Service, *db.DB, *fakePVE) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "nimbus.db")
	database, err := db.New(dbPath, &db.User{}, &db.VM{}, &db.Node{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	fake := &fakePVE{}
	svc := nodemgr.New(database.DB, fake, nodemgr.Config{
		PerVMMigrateTimeout: 5 * time.Second,
		TaskPollInterval:    10 * time.Millisecond,
		VacateMissThreshold: 3,
		VMDiskStorage:       pool,
	})
	return svc, database, fake
}

// TestCordonFlipsState verifies the lock state transition works and
// timestamps/actor get recorded.
func TestCordonFlipsState(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	ctx := context.Background()
	// Touch List once so the row exists with default lock state.
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}

	if _, err := svc.Cordon(ctx, nodemgr.CordonRequest{
		NodeName: "alpha", Reason: "scheduled maintenance", ActorID: 7,
	}); err != nil {
		t.Fatalf("Cordon: %v", err)
	}

	var row db.Node
	if err := database.WithContext(ctx).Where("name = ?", "alpha").First(&row).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.LockState != "cordoned" {
		t.Errorf("LockState = %q, want cordoned", row.LockState)
	}
	if row.LockedBy == nil || *row.LockedBy != 7 {
		t.Errorf("LockedBy = %v, want 7", row.LockedBy)
	}
	if row.LockReason != "scheduled maintenance" {
		t.Errorf("LockReason = %q", row.LockReason)
	}
	if row.LockedAt == nil {
		t.Errorf("LockedAt = nil; want a timestamp")
	}
}

// TestUncordonClearsLockContext verifies that uncordon clears the lock
// metadata so a subsequent cordon doesn't show stale reason text.
func TestUncordonClearsLockContext(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib}}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := svc.Cordon(ctx, nodemgr.CordonRequest{NodeName: "alpha", Reason: "first"}); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if _, err := svc.Uncordon(ctx, "alpha"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	var row db.Node
	if err := database.WithContext(ctx).Where("name = ?", "alpha").First(&row).Error; err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.LockState != "none" {
		t.Errorf("LockState = %q, want none", row.LockState)
	}
	if row.LockedBy != nil || row.LockedAt != nil || row.LockReason != "" {
		t.Errorf("lock context not cleared: by=%v at=%v reason=%q", row.LockedBy, row.LockedAt, row.LockReason)
	}
}

// TestUncordonRefusedDuringDrain verifies the executor's in-flight lock
// blocks concurrent state changes — operators can't yank a drain mid-flight.
func TestUncordonRefusedDuringDrain(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Seed one VM on alpha so Execute has work to do.
	if err := database.WithContext(ctx).Create(&db.VM{
		VMID: 100, Hostname: "vm1", IP: "10.0.0.10", Node: "alpha",
		Tier: "small", OSTemplate: "ubuntu-24.04", Status: "running",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	// Block migrations until released so we can observe the in-flight lock.
	release := make(chan struct{})
	fake.afterMigrate = func(_ migrateCall) { <-release }

	executeDone := make(chan error, 1)
	go func() {
		executeDone <- svc.Execute(ctx, nodemgr.ExecuteRequest{
			SourceNode: "alpha",
			Choices:    map[int]string{100: "beta"},
		}, func(_ nodemgr.DrainEvent) {})
	}()

	// Wait for the executor to call MigrateVM (and block on release).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if svc.IsDrainInFlight("alpha") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !svc.IsDrainInFlight("alpha") {
		close(release)
		<-executeDone
		t.Fatalf("drain never started")
	}

	if _, err := svc.Uncordon(ctx, "alpha"); !errors.Is(err, nodemgr.ErrDrainInFlight) {
		close(release)
		<-executeDone
		t.Fatalf("Uncordon during drain: err = %v, want ErrDrainInFlight", err)
	}

	close(release)
	if err := <-executeDone; err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestExecuteRevalidatesEachVM is the core correctness test for the
// "destination changed between preview and execute" case from the spec's
// acceptance criteria.
//
// Setup: two managed VMs on alpha, both planned to land on beta. Beta
// has just enough capacity for ONE small VM. The first migration succeeds;
// the after-migrate hook flips beta into a state where it no longer has
// capacity, so the second migration's re-validation must abort cleanly
// without aborting the rest of the batch (only one VM left, so the test
// instead verifies the re-validation kicks at all).
func TestExecuteRevalidatesEachVM(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	// Beta starts with plenty of headroom. The afterMigrate hook flips
	// it to cordoned after the first migration so the second VM's
	// re-validation rejects with "no longer eligible".
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, vmid := range []int{100, 101} {
		if err := database.WithContext(ctx).Create(&db.VM{
			VMID: vmid, Hostname: "vm" + string(rune('0'+vmid-100)),
			IP:   "10.0.0." + string(rune('0'+vmid-100)),
			Node: "alpha", Tier: "small",
			OSTemplate: "ubuntu-24.04", Status: "running",
		}).Error; err != nil {
			t.Fatalf("seed vm %d: %v", vmid, err)
		}
	}
	// Pre-cordon beta — but only AFTER the first migration is initiated.
	// Doing it here directly would block both VMs at the planner stage.
	var migratedFirst bool
	fake.afterMigrate = func(_ migrateCall) {
		if migratedFirst {
			return
		}
		migratedFirst = true
		// Use the service's own Cordon (which also writes to DB) so the
		// second VM's re-validation reads the cordoned state.
		if _, err := svc.Cordon(ctx, nodemgr.CordonRequest{NodeName: "beta", Reason: "filled up mid-drain"}); err != nil {
			t.Errorf("mid-drain cordon: %v", err)
		}
	}

	// Capture per-VM events so we can assert one succeeded + one failed.
	var events []nodemgr.DrainEvent
	report := func(e nodemgr.DrainEvent) { events = append(events, e) }

	err := svc.Execute(ctx, nodemgr.ExecuteRequest{
		SourceNode: "alpha",
		Choices:    map[int]string{100: "beta", 101: "beta"},
	}, report)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var done, errs int
	for _, e := range events {
		switch e.Type {
		case "vm_done":
			done++
		case "vm_error":
			errs++
		}
	}
	if done != 1 {
		t.Errorf("vm_done count = %d, want 1", done)
	}
	if errs != 1 {
		t.Errorf("vm_error count = %d, want 1 (re-validation must fire)", errs)
	}
	// Source should NOT be drained — one VM stuck behind, so the
	// executor should flip back to cordoned for retry.
	var alpha db.Node
	if err := database.WithContext(ctx).Where("name = ?", "alpha").First(&alpha).Error; err != nil {
		t.Fatalf("load alpha row: %v", err)
	}
	if alpha.LockState != "cordoned" {
		t.Errorf("alpha LockState after partial drain = %q, want cordoned", alpha.LockState)
	}
}

// TestExecuteFullSuccessFlipsToDrained verifies the happy path: every VM
// migrates, source flips to drained, complete event reports drained=true.
func TestExecuteFullSuccessFlipsToDrained(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := database.WithContext(ctx).Create(&db.VM{
		VMID: 100, Hostname: "vm1", IP: "10.0.0.10", Node: "alpha",
		Tier: "small", OSTemplate: "ubuntu-24.04", Status: "running",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	// Simulate the migration actually moving the VM by updating the
	// fake's cluster state in the after-migrate hook. (Otherwise the
	// post-batch recount still sees the VM on alpha and the source
	// flips to "cordoned" instead of "drained".)
	fake.afterMigrate = func(call migrateCall) {
		// Move the local DB row — that's what managedVMsOnNode reads.
		_ = database.WithContext(ctx).Model(&db.VM{}).
			Where("vmid = ?", call.VMID).
			Update("node", call.Target).Error
	}

	var events []nodemgr.DrainEvent
	report := func(e nodemgr.DrainEvent) { events = append(events, e) }

	if err := svc.Execute(ctx, nodemgr.ExecuteRequest{
		SourceNode: "alpha",
		Choices:    map[int]string{100: "beta"},
	}, report); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	final := events[len(events)-1]
	if final.Type != "complete" {
		t.Fatalf("last event = %+v, want type=complete", final)
	}
	if !final.Drained || final.Succeeded != 1 || final.Failed != 0 {
		t.Errorf("complete = %+v, want drained=true succeeded=1 failed=0", final)
	}

	var alpha db.Node
	_ = database.WithContext(ctx).Where("name = ?", "alpha").First(&alpha).Error
	if alpha.LockState != "drained" {
		t.Errorf("alpha LockState = %q, want drained", alpha.LockState)
	}
}

// TestComputePlan_NoBlockedRows asserts that a typical drain produces a
// plan with no blocked warnings + a recommended target per VM.
func TestComputePlan_NoBlockedRows(t *testing.T) {
	t.Parallel()

	svc, database, fake := newTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
		{Name: "gamma", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, vmid := range []int{100, 101} {
		if err := database.WithContext(ctx).Create(&db.VM{
			VMID: vmid, Hostname: "vm" + string(rune('0'+vmid-100)),
			IP:   "10.0.0." + string(rune('0'+vmid-100)),
			Node: "alpha", Tier: "small",
			OSTemplate: "ubuntu-24.04", Status: "running",
		}).Error; err != nil {
			t.Fatalf("seed vm %d: %v", vmid, err)
		}
	}
	plan, err := svc.ComputePlan(ctx, "alpha")
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.HasBlocked {
		t.Errorf("HasBlocked = true; expected free cluster to have a usable target for every VM")
	}
	if len(plan.Migrations) != 2 {
		t.Errorf("len(Migrations) = %d, want 2", len(plan.Migrations))
	}
	for _, m := range plan.Migrations {
		if m.AutoPick == "" {
			t.Errorf("vm %d has no AutoPick", m.VMID)
		}
		if m.AutoPick == "alpha" {
			t.Errorf("vm %d picked source node %q as destination", m.VMID, m.AutoPick)
		}
		// At least one eligible non-disabled option.
		ok := false
		for _, e := range m.Eligible {
			if !e.Disabled {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("vm %d has no usable destination in Eligible", m.VMID)
		}
	}
}

// TestComputePlan_DiskGateFires asserts the drain plan now considers
// disk capacity. Before the disk-consistency fix this test would fail —
// the planner left StorageByNode nil and the disk gate never tripped,
// so a 100GB VM could be planned onto a 10GB-free destination and only
// fail at execute time.
func TestComputePlan_DiskGateFires(t *testing.T) {
	t.Parallel()
	svc, database, fake := newTestServiceWithStorage(t, "local-lvm")
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "beta", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib},
	}
	// beta has only 5 GiB free in the configured pool — too small for
	// the medium tier's 30 GiB requirement.
	fake.clusterStore = []proxmox.ClusterStorage{
		{Storage: "local-lvm", Node: "alpha", Total: 500 * gib, Used: 100 * gib, Shared: 0},
		{Storage: "local-lvm", Node: "beta", Total: 500 * gib, Used: 495 * gib, Shared: 0},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := database.WithContext(ctx).Create(&db.VM{
		VMID: 100, Hostname: "vm1", IP: "10.0.0.10", Node: "alpha",
		Tier: "medium", OSTemplate: "ubuntu-24.04", Status: "running",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	plan, err := svc.ComputePlan(ctx, "alpha")
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Migrations) != 1 {
		t.Fatalf("Migrations len = %d, want 1", len(plan.Migrations))
	}
	row := plan.Migrations[0]
	// beta should be present in Eligible but flagged disabled with the
	// insufficient-disk reason — this is the "still shows them, dimmed,
	// with a hover tooltip" behaviour the spec calls for.
	var betaOpt *nodemgr.EligibleTarget
	for i := range row.Eligible {
		if row.Eligible[i].Node == "beta" {
			betaOpt = &row.Eligible[i]
			break
		}
	}
	if betaOpt == nil {
		t.Fatalf("beta missing from Eligible options")
	}
	if !betaOpt.Disabled {
		t.Errorf("beta.Disabled = false; want true (only 5GB free, need 30GB)")
	}
	// AutoPick should NOT be beta — the planner picked nothing
	// (only one candidate, and it's disabled), so the row is blocked.
	if row.AutoPick == "beta" {
		t.Errorf("AutoPick = beta; want empty (disk-too-full)")
	}
	if !plan.HasBlocked {
		t.Errorf("HasBlocked = false; want true (no usable destination)")
	}
}

// TestRemoveRefusedUntilDrained verifies the lock-state gate on Remove.
func TestRemoveRefusedUntilDrained(t *testing.T) {
	t.Parallel()

	svc, _, fake := newTestService(t)
	fake.nodes = []proxmox.Node{{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib}}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}

	if err := svc.Remove(ctx, "alpha"); !errors.Is(err, nodemgr.ErrNotDrained) {
		t.Errorf("Remove on none state: err = %v, want ErrNotDrained", err)
	}

	if _, err := svc.Cordon(ctx, nodemgr.CordonRequest{NodeName: "alpha"}); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if err := svc.Remove(ctx, "alpha"); !errors.Is(err, nodemgr.ErrNotDrained) {
		t.Errorf("Remove on cordoned state: err = %v, want ErrNotDrained", err)
	}
}
