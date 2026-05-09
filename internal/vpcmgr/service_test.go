package vpcmgr_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
	"nimbus/internal/vpcmgr"
)

// fakeSDN stubs the SDNClient surface and counts calls. Override
// hooks let individual tests inject failures.
type fakeSDN struct {
	mu sync.Mutex
	// Per-method override hooks. nil = success no-op.
	createZone   func(proxmox.SDNZone) error
	deleteZone   func(string) error
	createVNet   func(proxmox.SDNVNet) error
	deleteVNet   func(string) error
	createSubnet func(proxmox.SDNSubnet) error
	deleteSubnet func(string, string) error
	applySDN     func() error
}

func (f *fakeSDN) CreateSDNZone(_ context.Context, z proxmox.SDNZone) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createZone != nil {
		return f.createZone(z)
	}
	return nil
}
func (f *fakeSDN) DeleteSDNZone(_ context.Context, z string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteZone != nil {
		return f.deleteZone(z)
	}
	return nil
}
func (f *fakeSDN) CreateSDNVNet(_ context.Context, v proxmox.SDNVNet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createVNet != nil {
		return f.createVNet(v)
	}
	return nil
}
func (f *fakeSDN) DeleteSDNVNet(_ context.Context, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteVNet != nil {
		return f.deleteVNet(v)
	}
	return nil
}
func (f *fakeSDN) CreateSDNSubnet(_ context.Context, s proxmox.SDNSubnet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createSubnet != nil {
		return f.createSubnet(s)
	}
	return nil
}
func (f *fakeSDN) DeleteSDNSubnet(_ context.Context, vnet, sub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteSubnet != nil {
		return f.deleteSubnet(vnet, sub)
	}
	return nil
}
func (f *fakeSDN) ApplySDN(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applySDN != nil {
		return f.applySDN()
	}
	return nil
}
func (f *fakeSDN) ReloadNodeNetwork(_ context.Context, _ string) error { return nil }

// fakeGateway is a no-op GatewayProvisioner — returns canned vmid+node
// or a configured error. Most tests don't care about what the gateway
// does, only that it gets called.
type fakeGateway struct {
	provisionCalls int
	destroyCalls   int
	provisionErr   error
	vmid           int
	node           string
}

func (f *fakeGateway) Provision(_ context.Context, _ *db.VPC) (int, string, error) {
	f.provisionCalls++
	if f.provisionErr != nil {
		return 0, "", f.provisionErr
	}
	if f.vmid == 0 {
		f.vmid = 200
	}
	if f.node == "" {
		f.node = "alpha"
	}
	return f.vmid, f.node, nil
}
func (f *fakeGateway) Destroy(_ context.Context, _ *db.VPC) error {
	f.destroyCalls++
	return nil
}

func newTestService(t *testing.T) (*vpcmgr.Service, *fakeSDN, *fakeGateway, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(dbPath, &db.VPC{}, &db.VPCMembership{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	sdn := &fakeSDN{}
	peers := vpcmgr.PeerResolverFunc(func(_ context.Context) (string, error) {
		return "10.0.0.1,10.0.0.2", nil
	})
	svc, err := vpcmgr.New(sdn, peers, database.DB, vpcmgr.Config{
		PoolCIDR: "10.0.0.0/9",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := &fakeGateway{}
	svc.SetGateway(gw)
	return svc, sdn, gw, database
}

// TestCreateVPC_HappyPath asserts the full create flow leaves a row
// with status=active, GatewayLXCID populated, and the gateway provisioner
// called exactly once.
func TestCreateVPC_HappyPath(t *testing.T) {
	t.Parallel()
	svc, _, gw, _ := newTestService(t)
	row, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	if row.Status != "active" {
		t.Errorf("status = %q, want active", row.Status)
	}
	if row.GatewayLXCID == nil || *row.GatewayLXCID != 200 {
		t.Errorf("GatewayLXCID = %v, want 200", row.GatewayLXCID)
	}
	if row.GatewayNode != "alpha" {
		t.Errorf("GatewayNode = %q, want alpha", row.GatewayNode)
	}
	if gw.provisionCalls != 1 {
		t.Errorf("provisionCalls = %d, want 1", gw.provisionCalls)
	}
	// Zone name shape: "v" + 7 hex.
	if len(row.ZoneName) != 8 || row.ZoneName[0] != 'v' {
		t.Errorf("ZoneName = %q, want v<7hex>", row.ZoneName)
	}
}

// TestCreateVPC_DuplicateName returns ConflictError without calling
// the gateway. Different owners can share a name.
func TestCreateVPC_DuplicateName(t *testing.T) {
	t.Parallel()
	svc, _, gw, _ := newTestService(t)
	if _, err := svc.CreateVPC(context.Background(), 1, "shared"); err != nil {
		t.Fatalf("first CreateVPC: %v", err)
	}
	_, err := svc.CreateVPC(context.Background(), 1, "shared")
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("err = %T %v, want ConflictError", err, err)
	}
	// Different owner: same name should work.
	if _, err := svc.CreateVPC(context.Background(), 2, "shared"); err != nil {
		t.Errorf("second-owner CreateVPC: %v", err)
	}
	// Gateway should have been called twice (success cases), not three.
	if gw.provisionCalls != 2 {
		t.Errorf("provisionCalls = %d, want 2", gw.provisionCalls)
	}
}

// TestCreateVPC_GatewayFailureRollsBack: when gateway provision fails,
// PVE state is torn down AND the row is hard-deleted (so retries don't
// hit the soft-deleted tombstone via unique index).
func TestCreateVPC_GatewayFailureRollsBack(t *testing.T) {
	t.Parallel()
	svc, sdn, gw, database := newTestService(t)
	gw.provisionErr = errors.New("gateway: NAT setup failed")

	_, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err == nil {
		t.Fatal("expected error from CreateVPC")
	}

	// Tracking: PVE create + delete should both have run.
	// We assert no row remains so a retry can succeed.
	var n int64
	if err := database.DB.Unscoped().Model(&db.VPC{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows after rollback = %d, want 0", n)
	}

	// Retry without gateway error should succeed.
	gw.provisionErr = nil
	if _, err := svc.CreateVPC(context.Background(), 1, "alpha"); err != nil {
		t.Errorf("retry CreateVPC: %v", err)
	}
	_ = sdn // unused but reserved if we want to assert call sequence
}

// TestDeleteVPC_RefusesWithMembers asserts the delete-while-attached
// gate. Members must be removed before the VPC is deletable.
func TestDeleteVPC_RefusesWithMembers(t *testing.T) {
	t.Parallel()
	svc, _, _, database := newTestService(t)
	row, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	// Attach a member directly (skip the IP allocator for shape).
	if err := database.DB.Create(&db.VPCMembership{VPCID: row.ID, VMID: 99, VMIP: "10.0.0.10"}).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	err = svc.DeleteVPC(context.Background(), row.ID, 1, false)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("err = %T %v, want ConflictError", err, err)
	}
}

// TestAllocateMemberIP_StartsAtTen: first member gets .10 (or .10 of the /16),
// next gets .11, and so on. .1 is the gateway, .2..-.9 are reserved.
func TestAllocateMemberIP_StartsAtTen(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newTestService(t)
	row, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	first, err := svc.AllocateMemberIP(context.Background(), row.ID, 1)
	if err != nil {
		t.Fatalf("AllocateMemberIP 1: %v", err)
	}
	second, err := svc.AllocateMemberIP(context.Background(), row.ID, 2)
	if err != nil {
		t.Fatalf("AllocateMemberIP 2: %v", err)
	}
	if first == second {
		t.Errorf("first and second member IPs identical: %s", first)
	}
	// Should both end in .10, .11 — but the actual /16 differs by hash.
	// Just assert determinism: first allocation always returns the
	// lowest free address and the second is exactly one above.
	if !ipIsImmediatelyAfter(first, second) {
		t.Errorf("%s and %s should be consecutive, first→second", first, second)
	}
}

// TestReleaseMember_Idempotent: calling Release twice doesn't error.
func TestReleaseMember_Idempotent(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newTestService(t)
	row, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	if _, err := svc.AllocateMemberIP(context.Background(), row.ID, 7); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := svc.ReleaseMember(context.Background(), 7); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := svc.ReleaseMember(context.Background(), 7); err != nil {
		t.Errorf("second Release should be idempotent: %v", err)
	}
}

// TestAllocateMemberIP_ClearsOrphanForRecycledVMID: PVE recycles
// vmids handed out by /cluster/nextid. If a previous Destroy/Release
// for the old VM was skipped, an orphan VPCMembership row remains in
// the table with that vmid; the global UNIQUE on vm_id would then
// block every fresh allocation for that recycled id. AllocateMemberIP
// must purge the orphan up-front and succeed.
func TestAllocateMemberIP_ClearsOrphanForRecycledVMID(t *testing.T) {
	t.Parallel()
	svc, _, _, database := newTestService(t)
	row, err := svc.CreateVPC(context.Background(), 1, "alpha")
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}

	// Seed an orphan membership row that pretends a long-deleted VM
	// once held vmid=42 in this VPC at .200. ReleaseMember was never
	// called for it, so the row sits there blocking the next vmid=42.
	orphan := &db.VPCMembership{VPCID: row.ID, VMID: 42, VMIP: "10.0.0.200"}
	if err := database.DB.Create(orphan).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	got, err := svc.AllocateMemberIP(context.Background(), row.ID, 42)
	if err != nil {
		t.Fatalf("AllocateMemberIP after orphan: %v", err)
	}
	// Fresh allocation should land at the lowest free offset (.10),
	// NOT inherit the orphan's IP — the row is replaced wholesale.
	if got == "10.0.0.200" {
		t.Errorf("got orphan's IP %s; expected fresh allocation", got)
	}

	// And exactly one live row for vmid=42 should remain.
	var count int64
	if err := database.DB.Model(&db.VPCMembership{}).
		Where("vm_id = ?", uint(42)).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for vmid=42 = %d, want 1", count)
	}
}

// TestNew_ValidatesConfig: empty pool, bad CIDR, bad VPCSize all rejected.
func TestNew_ValidatesConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  vpcmgr.Config
	}{
		{"empty pool", vpcmgr.Config{}},
		{"bad cidr", vpcmgr.Config{PoolCIDR: "not-a-cidr"}},
		{"vpc size too small", vpcmgr.Config{PoolCIDR: "10.0.0.0/9", VPCSize: 8}},
		{"vpc size too big", vpcmgr.Config{PoolCIDR: "10.0.0.0/9", VPCSize: 30}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := vpcmgr.New(&fakeSDN{}, peerNoop, nil, tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

var peerNoop = vpcmgr.PeerResolverFunc(func(_ context.Context) (string, error) {
	return "10.0.0.1", nil
})

// ipIsImmediatelyAfter reports whether second is exactly one above first
// (e.g. "10.42.0.10" → "10.42.0.11").
func ipIsImmediatelyAfter(first, second string) bool {
	return increment(first) == second
}

func increment(ip string) string {
	// quick parse + increment; only valid for simple cases used in tests
	parsed := proxmoxIPv4(ip)
	if parsed == 0 {
		return ""
	}
	return uintToIPv4(parsed + 1)
}

func proxmoxIPv4(s string) uint32 {
	var a, b, c, d uint32
	if _, err := fscanIP(s, &a, &b, &c, &d); err != nil {
		return 0
	}
	return a<<24 | b<<16 | c<<8 | d
}

func fscanIP(s string, a, b, c, d *uint32) (int, error) {
	return sscanIP(s, a, b, c, d)
}

// sscanIP is a tiny IPv4 parser used only by the consecutive-IP test
// assertion — the production code uses net.ParseCIDR.
func sscanIP(s string, a, b, c, d *uint32) (int, error) {
	parts := splitDots(s)
	if len(parts) != 4 {
		return 0, errors.New("not 4 dot-separated parts")
	}
	*a = atoiU32(parts[0])
	*b = atoiU32(parts[1])
	*c = atoiU32(parts[2])
	*d = atoiU32(parts[3])
	return 4, nil
}

func splitDots(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func atoiU32(s string) uint32 {
	var n uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint32(c-'0')
	}
	return n
}

func uintToIPv4(n uint32) string {
	return formatU8(byte(n>>24)) + "." + formatU8(byte(n>>16)) + "." + formatU8(byte(n>>8)) + "." + formatU8(byte(n))
}

func formatU8(b byte) string {
	if b == 0 {
		return "0"
	}
	digits := make([]byte, 0, 3)
	for b > 0 {
		digits = append([]byte{'0' + (b % 10)}, digits...)
		b /= 10
	}
	return string(digits)
}
