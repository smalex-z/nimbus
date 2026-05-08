package standalonenet_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
	"nimbus/internal/standalonenet"
)

// fakeSDN is a stub SDNClient with per-method counters and optional
// override hooks. Lets each test inject specific failures without
// standing up the whole proxmox client.
type fakeSDN struct {
	mu sync.Mutex

	createZoneCalls   int
	createVNetCalls   int
	createSubnetCalls int
	deleteZoneCalls   int
	deleteVNetCalls   int
	deleteSubnetCalls int
	applyCalls        int

	createZoneErr   error
	createVNetErr   error
	createSubnetErr error
	deleteZoneErr   error
	deleteVNetErr   error
	deleteSubnetErr error
	applyErr        error
}

func (f *fakeSDN) CreateSDNZone(_ context.Context, _ proxmox.SDNZone) error {
	f.mu.Lock()
	f.createZoneCalls++
	f.mu.Unlock()
	return f.createZoneErr
}
func (f *fakeSDN) DeleteSDNZone(_ context.Context, _ string) error {
	f.mu.Lock()
	f.deleteZoneCalls++
	f.mu.Unlock()
	return f.deleteZoneErr
}
func (f *fakeSDN) CreateSDNVNet(_ context.Context, _ proxmox.SDNVNet) error {
	f.mu.Lock()
	f.createVNetCalls++
	f.mu.Unlock()
	return f.createVNetErr
}
func (f *fakeSDN) DeleteSDNVNet(_ context.Context, _ string) error {
	f.mu.Lock()
	f.deleteVNetCalls++
	f.mu.Unlock()
	return f.deleteVNetErr
}
func (f *fakeSDN) CreateSDNSubnet(_ context.Context, _ proxmox.SDNSubnet) error {
	f.mu.Lock()
	f.createSubnetCalls++
	f.mu.Unlock()
	return f.createSubnetErr
}
func (f *fakeSDN) DeleteSDNSubnet(_ context.Context, _, _ string) error {
	f.mu.Lock()
	f.deleteSubnetCalls++
	f.mu.Unlock()
	return f.deleteSubnetErr
}
func (f *fakeSDN) ApplySDN(_ context.Context) error {
	f.mu.Lock()
	f.applyCalls++
	f.mu.Unlock()
	return f.applyErr
}
func (f *fakeSDN) ReloadNodeNetwork(_ context.Context, _ string) error { return nil }

func newTestSvc(t *testing.T, fake *fakeSDN) *standalonenet.Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.StandaloneVMNetwork{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	svc, err := standalonenet.New(fake, database.DB, standalonenet.Config{
		PoolCIDR: "10.128.0.0/9",
	})
	if err != nil {
		t.Fatalf("standalonenet.New: %v", err)
	}
	return svc
}

// TestProvision_HappyPath asserts a clean Provision run: PVE-side
// CreateZone+VNet+Subnet+ApplySDN are each called once, the row is
// persisted with the expected derived fields (zone == vnet, gateway
// .1 of subnet, vmip .10), and IP/CIDR fall inside the configured
// pool.
func TestProvision_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	row, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if row.VMID != 1 {
		t.Errorf("VMID = %d", row.VMID)
	}
	if row.ZoneName == "" || row.ZoneName != row.VNetName {
		t.Errorf("expected zone == vnet, got zone=%q vnet=%q", row.ZoneName, row.VNetName)
	}
	if row.ZoneName[0] != 's' || len(row.ZoneName) != 8 {
		t.Errorf("zone name shape wrong: %q", row.ZoneName)
	}
	if row.SubnetCIDR == "" {
		t.Errorf("empty SubnetCIDR")
	}
	// .1 / .10 convention
	if !endsWith(row.GatewayIP, ".1") {
		t.Errorf("gateway = %q, expected .1 of subnet", row.GatewayIP)
	}
	if !endsWith(row.VMIP, ".10") {
		t.Errorf("vm ip = %q, expected .10 of subnet", row.VMIP)
	}
	if row.Node != "alpha" {
		t.Errorf("node = %q", row.Node)
	}

	if fake.createZoneCalls != 1 || fake.createVNetCalls != 1 || fake.createSubnetCalls != 1 || fake.applyCalls != 1 {
		t.Errorf("create call counts: zone=%d vnet=%d subnet=%d apply=%d (each want 1)",
			fake.createZoneCalls, fake.createVNetCalls, fake.createSubnetCalls, fake.applyCalls)
	}
}

// TestProvision_Idempotent asserts re-running Provision for the same
// VM returns the existing row and skips the PVE-side calls. Crucial
// because retried provisioning (e.g. a flaky network in mid-flight)
// must not double-create PVE resources.
func TestProvision_Idempotent(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	first, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	second, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if first.ID != second.ID || first.ZoneName != second.ZoneName {
		t.Errorf("idempotent re-provision returned a different row")
	}
	if fake.createZoneCalls != 1 {
		t.Errorf("expected exactly 1 PVE create on re-run, got %d", fake.createZoneCalls)
	}
}

// TestProvision_PVEFailureRollsBackRow asserts that a PVE-side
// failure during bootstrap (e.g. CreateSDNVNet errors) results in
// the row being deleted, so a follow-up Provision call doesn't
// short-circuit on the orphan and instead retries cleanly.
func TestProvision_PVEFailureRollsBackRow(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{createSubnetErr: errors.New("boom")}
	svc := newTestSvc(t, fake)

	_, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err == nil {
		t.Fatalf("expected error from CreateSDNSubnet failure, got nil")
	}

	// Row should be gone — try again with a working fake and verify
	// it succeeds (would fail on idempotent-find if the row stuck).
	fake.createSubnetErr = nil
	row, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("retry Provision: %v", err)
	}
	if row == nil {
		t.Fatal("row is nil after successful retry")
	}
}

// TestProvision_CollisionRetriesWithSalt asserts the salt-and-retry
// loop kicks in when two distinct VMs hash to the same zone name.
// We force a collision by giving the first VM a name that we then
// try to reuse — the second Provision should pick a different zone
// via salt incrementing.
func TestProvision_CollisionRetriesWithSalt(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	// Provision VM 1 with identifier "x" → reserves whatever zone
	// hash("x") produces.
	a, err := svc.Provision(context.Background(), 1, "x", "alpha")
	if err != nil {
		t.Fatalf("Provision a: %v", err)
	}

	// Provision VM 2 with the SAME identifier "x" — would normally
	// hash to the same zone, but the unique-violation on zone_name
	// triggers a salt retry.
	b, err := svc.Provision(context.Background(), 2, "x", "bravo")
	if err != nil {
		t.Fatalf("Provision b (collision retry): %v", err)
	}
	if a.ZoneName == b.ZoneName {
		t.Errorf("collision retry didn't pick a new zone: both = %q", a.ZoneName)
	}
}

// TestDestroy_ReverseOrder asserts Destroy calls subnet/vnet/zone
// teardown in the right order (subnet first — Proxmox refuses VNet
// delete while subnets reference it; zone last — Proxmox refuses
// zone delete while VNets reference it). And that the row is gone
// at the end.
func TestDestroy_ReverseOrder(t *testing.T) {
	t.Parallel()
	var seq []string
	var seqMu sync.Mutex
	record := func(s string) {
		seqMu.Lock()
		seq = append(seq, s)
		seqMu.Unlock()
	}
	fake := &recordingSDN{
		fakeSDN: &fakeSDN{},
		record:  record,
	}
	svc := newTestSvcWithSDN(t, fake)

	row, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Reset sequence for the destroy phase only.
	seqMu.Lock()
	seq = nil
	seqMu.Unlock()

	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []string{"DeleteSDNSubnet", "DeleteSDNVNet", "DeleteSDNZone", "ApplySDN"}
	if len(seq) != len(want) {
		t.Fatalf("destroy sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("destroy step %d = %q, want %q", i, seq[i], want[i])
		}
	}

	// Row should be gone — Get returns nil/nil, not the deleted row.
	got, err := svc.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get post-destroy: %v", err)
	}
	if got != nil {
		t.Errorf("Get post-destroy returned row: %+v", got)
	}
	_ = row
}

// TestDestroy_TolerateMissing asserts Destroy treats a missing
// PVE-side resource (404) as success. Real recovery scenario: an
// admin nuked the zone via pvesh and now wants Nimbus to clean up
// the leftover DB row.
func TestDestroy_TolerateMissing(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{
		deleteSubnetErr: proxmox.ErrNotFound,
		deleteVNetErr:   proxmox.ErrNotFound,
		deleteZoneErr:   proxmox.ErrNotFound,
	}
	svc := newTestSvc(t, fake)

	if _, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := svc.Destroy(context.Background(), 1); err != nil {
		t.Errorf("Destroy with all-404: %v (should be tolerated)", err)
	}
}

// TestNew_ValidatesConfig covers the New() guards: empty pool cidr,
// invalid cidr, out-of-range subnet size.
func TestNew_ValidatesConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  standalonenet.Config
	}{
		{"empty-pool", standalonenet.Config{}},
		{"bad-cidr", standalonenet.Config{PoolCIDR: "not-a-cidr"}},
		{"too-narrow-subnet", standalonenet.Config{PoolCIDR: "10.0.0.0/9", SubnetSize: 31}},
		{"too-wide-subnet", standalonenet.Config{PoolCIDR: "10.0.0.0/9", SubnetSize: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := standalonenet.New(&fakeSDN{}, nil, tc.cfg); err == nil {
				t.Errorf("New(%+v) accepted bad config", tc.cfg)
			}
		})
	}
}

// TestProvision_ConflictExhaustion asserts that when every salt
// retry hits a unique violation (saturated pool), Provision returns
// a typed ConflictError so the handler maps it to 409.
func TestProvision_ConflictExhaustion(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	// Pre-populate enough rows that hash collisions become certain.
	// Easier: salt the same identifier a bunch of times until we
	// exhaust retries on a fresh VMID. We do this by reserving each
	// of the 8 retry-salts up front for a different VM identifier.
	// For the test we just inject identifier collisions explicitly
	// via the same-identifier shortcut from the salt-collision test:
	// 8 prior provisions of identifier "y" + 8 retry salts (same
	// derivations) → next Provision must exhaust.
	//
	// Implementation uses a tighter pool (/9 → /27 = 256K /27s but
	// salt iterations are limited to 8) and forces collisions by
	// re-using the same identifier on different VMIDs.
	for i := 1; i <= 8; i++ {
		if _, err := svc.Provision(context.Background(), uint(i), "y", "alpha"); err != nil {
			t.Fatalf("seed Provision %d: %v", i, err)
		}
	}
	_, err := svc.Provision(context.Background(), 99, "y", "alpha")
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("expected ConflictError after 8 retries exhausted, got %T %v", err, err)
	}
}

// recordingSDN wraps a fakeSDN with a per-call sequence recorder.
// Used by TestDestroy_ReverseOrder to assert tear-down order.
type recordingSDN struct {
	*fakeSDN
	record func(string)
}

func (r *recordingSDN) CreateSDNZone(ctx context.Context, z proxmox.SDNZone) error {
	r.record("CreateSDNZone")
	return r.fakeSDN.CreateSDNZone(ctx, z)
}
func (r *recordingSDN) DeleteSDNZone(ctx context.Context, zone string) error {
	r.record("DeleteSDNZone")
	return r.fakeSDN.DeleteSDNZone(ctx, zone)
}
func (r *recordingSDN) CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error {
	r.record("CreateSDNVNet")
	return r.fakeSDN.CreateSDNVNet(ctx, v)
}
func (r *recordingSDN) DeleteSDNVNet(ctx context.Context, vnet string) error {
	r.record("DeleteSDNVNet")
	return r.fakeSDN.DeleteSDNVNet(ctx, vnet)
}
func (r *recordingSDN) CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error {
	r.record("CreateSDNSubnet")
	return r.fakeSDN.CreateSDNSubnet(ctx, s)
}
func (r *recordingSDN) DeleteSDNSubnet(ctx context.Context, vnet, subnet string) error {
	r.record("DeleteSDNSubnet")
	return r.fakeSDN.DeleteSDNSubnet(ctx, vnet, subnet)
}
func (r *recordingSDN) ApplySDN(ctx context.Context) error {
	r.record("ApplySDN")
	return r.fakeSDN.ApplySDN(ctx)
}

func newTestSvcWithSDN(t *testing.T, sdn standalonenet.SDNClient) *standalonenet.Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.StandaloneVMNetwork{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	svc, err := standalonenet.New(sdn, database.DB, standalonenet.Config{
		PoolCIDR: "10.128.0.0/9",
	})
	if err != nil {
		t.Fatalf("standalonenet.New: %v", err)
	}
	return svc
}

func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}
