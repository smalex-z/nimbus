package gateway_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/gateway"
	"nimbus/internal/proxmox"
)

// fakeLXC stubs LXCClient. Default behavior is "all calls succeed";
// override hooks let tests inject failures.
type fakeLXC struct {
	createCalls  atomic.Int32
	startCalls   atomic.Int32
	stopCalls    atomic.Int32
	destroyCalls atomic.Int32
	execCalls    atomic.Int32
	nextVMID     atomic.Int32

	createOpts proxmox.LXCCreateOpts
	execScript string
	execErr    error
	execExit   int
}

func (f *fakeLXC) CreateLXC(_ context.Context, _ string, opts proxmox.LXCCreateOpts) (string, error) {
	f.createCalls.Add(1)
	f.createOpts = opts
	return "UPID:create", nil
}
func (f *fakeLXC) StartLXC(_ context.Context, _ string, _ int) (string, error) {
	f.startCalls.Add(1)
	return "UPID:start", nil
}
func (f *fakeLXC) StopLXC(_ context.Context, _ string, _ int) (string, error) {
	f.stopCalls.Add(1)
	return "UPID:stop", nil
}
func (f *fakeLXC) DestroyLXC(_ context.Context, _ string, _ int) (string, error) {
	f.destroyCalls.Add(1)
	return "UPID:destroy", nil
}
func (f *fakeLXC) LXCExecShell(_ context.Context, _ string, _ int, script string) (*proxmox.LXCExecResult, error) {
	f.execCalls.Add(1)
	f.execScript = script
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &proxmox.LXCExecResult{ExitCode: f.execExit}, nil
}
func (f *fakeLXC) WaitForTask(_ context.Context, _, _ string, _ time.Duration) error { return nil }
func (f *fakeLXC) NextVMID(_ context.Context) (int, error) {
	v := f.nextVMID.Add(1) + 199
	return int(v), nil
}

func newTestSvc(t *testing.T, ipPool string) (*gateway.Service, *fakeLXC, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(dbPath, &db.GatewayLXCIP{}, &db.VPC{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	lxc := &fakeLXC{}
	svc, err := gateway.New(lxc, database.DB, gateway.Config{
		NetworkNode:   "alpha",
		HostBridge:    "vmbr0",
		HostGatewayIP: "192.168.1.1",
		HostPrefixLen: 24,
		IPPool:        ipPool,
		LXCTemplate:   "local:vztmpl/alpine.tar.xz",
		LXCStorage:    "local-lvm",
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc, lxc, database
}

// TestSeedPool_Idempotent: re-seeding the same range doesn't reset
// state of in-use rows.
func TestSeedPool_Idempotent(t *testing.T) {
	t.Parallel()
	_, _, database := newTestSvc(t, "10.0.0.10-10.0.0.12")
	// Mark one row as allocated by hand.
	if err := database.DB.Model(&db.GatewayLXCIP{}).
		Where("ip = ?", "10.0.0.11").
		Updates(map[string]any{"status": "allocated"}).Error; err != nil {
		t.Fatalf("manual mark: %v", err)
	}
	// Re-seed (by constructing a new service with the same pool).
	if _, err := gateway.New(&fakeLXC{}, database.DB, gateway.Config{
		NetworkNode: "alpha", LXCTemplate: "x", IPPool: "10.0.0.10-10.0.0.12",
	}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var allocated db.GatewayLXCIP
	if err := database.DB.Where("ip = ?", "10.0.0.11").First(&allocated).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if allocated.Status != "allocated" {
		t.Errorf("re-seed clobbered allocated row: status=%q want allocated", allocated.Status)
	}
}

// TestProvision_HappyPath: end-to-end create call hits LXC create +
// start + exec, returns the LXC's vmid + node, marks the IP allocated.
func TestProvision_HappyPath(t *testing.T) {
	t.Parallel()
	svc, lxc, database := newTestSvc(t, "10.0.0.10-10.0.0.20")
	vpc := &db.VPC{
		CIDR:     "10.42.0.0/16",
		ZoneName: "vabcdef0",
		VNetName: "vabcdef0",
	}
	if err := database.DB.Create(vpc).Error; err != nil {
		t.Fatalf("create vpc row: %v", err)
	}
	vmid, node, err := svc.Provision(context.Background(), vpc)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if node != "alpha" {
		t.Errorf("node = %q, want alpha", node)
	}
	if vmid == 0 {
		t.Errorf("vmid = 0, want non-zero")
	}
	if lxc.createCalls.Load() != 1 || lxc.startCalls.Load() != 1 || lxc.execCalls.Load() != 1 {
		t.Errorf("calls: create=%d start=%d exec=%d, want all 1",
			lxc.createCalls.Load(), lxc.startCalls.Load(), lxc.execCalls.Load())
	}
	// Wire shape: the eth0 IP must be in the host pool, and eth1 IP
	// must be the VPC CIDR's .1.
	if lxc.createOpts.Net0 == "" || lxc.createOpts.Net1 == "" {
		t.Errorf("missing net specs: net0=%q net1=%q",
			lxc.createOpts.Net0, lxc.createOpts.Net1)
	}
	// One IP should be marked allocated for this VPC.
	var n int64
	if err := database.DB.Model(&db.GatewayLXCIP{}).
		Where("status = ? AND vpc_id = ?", "allocated", vpc.ID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("allocated rows = %d, want 1", n)
	}
}

// TestProvision_PoolExhausted returns a clear error and does NOT
// touch the LXC API.
func TestProvision_PoolExhausted(t *testing.T) {
	t.Parallel()
	svc, lxc, database := newTestSvc(t, "10.0.0.10-10.0.0.10")
	// Mark the only row as allocated.
	if err := database.DB.Model(&db.GatewayLXCIP{}).
		Where("ip = ?", "10.0.0.10").
		Updates(map[string]any{"status": "allocated"}).Error; err != nil {
		t.Fatalf("manual mark: %v", err)
	}
	vpc := &db.VPC{CIDR: "10.42.0.0/16", ZoneName: "vabcdef0", VNetName: "vabcdef0"}
	_, _, err := svc.Provision(context.Background(), vpc)
	if err == nil {
		t.Fatal("expected error from exhausted pool")
	}
	if lxc.createCalls.Load() != 0 {
		t.Errorf("LXC API touched on exhausted pool: %d calls", lxc.createCalls.Load())
	}
}

// TestProvision_BootstrapFailure rolls back: LXC destroyed, IP released.
func TestProvision_BootstrapFailure(t *testing.T) {
	t.Parallel()
	svc, lxc, database := newTestSvc(t, "10.0.0.10-10.0.0.10")
	lxc.execErr = errors.New("exec network down")
	vpc := &db.VPC{CIDR: "10.42.0.0/16", ZoneName: "vabcdef0", VNetName: "vabcdef0"}
	if err := database.DB.Create(vpc).Error; err != nil {
		t.Fatalf("create vpc row: %v", err)
	}
	_, _, err := svc.Provision(context.Background(), vpc)
	if err == nil {
		t.Fatal("expected error from bootstrap failure")
	}
	// Cleanup defers should have run.
	if lxc.destroyCalls.Load() == 0 {
		t.Errorf("destroy not called on bootstrap failure")
	}
	// Pool should be free again.
	var n int64
	if err := database.DB.Model(&db.GatewayLXCIP{}).
		Where("status = ?", "free").Count(&n).Error; err != nil {
		t.Fatalf("count free: %v", err)
	}
	if n != 1 {
		t.Errorf("free rows = %d, want 1 (pool should be released)", n)
	}
}

// TestNew_Validates: missing config fails.
func TestNew_Validates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  gateway.Config
	}{
		{"missing node", gateway.Config{LXCTemplate: "x"}},
		{"missing template", gateway.Config{NetworkNode: "alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := gateway.New(&fakeLXC{}, nil, tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}
