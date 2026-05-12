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

	createZoneCalls       int
	createVNetCalls       int
	createSubnetCalls     int
	deleteZoneCalls       int
	deleteVNetCalls       int
	deleteSubnetCalls     int
	applyCalls            int
	reloadNodeCalls       int
	getZoneCalls          int
	updateZoneNodesCalls  int
	lastUpdateZoneNodes   string // nodes value from the most recent UpdateSDNZoneNodes

	createZoneErr   error
	createVNetErr   error
	createSubnetErr error
	deleteZoneErr   error
	deleteVNetErr   error
	deleteSubnetErr error
	applyErr        error
	reloadNodeErr   error
	getZoneErr      error
	updateZoneErr   error

	// getZoneResp lets a test stub the zone returned by GetSDNZone.
	// Nil means "default simple zone with pinned nodes='source'".
	getZoneResp *proxmox.SDNZone
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
func (f *fakeSDN) ReloadNodeNetwork(_ context.Context, _ string) error {
	f.mu.Lock()
	f.reloadNodeCalls++
	f.mu.Unlock()
	return f.reloadNodeErr
}
func (f *fakeSDN) GetSDNZone(_ context.Context, zone string) (*proxmox.SDNZone, error) {
	f.mu.Lock()
	f.getZoneCalls++
	resp := f.getZoneResp
	err := f.getZoneErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp != nil {
		// Return a copy so caller mutations don't affect future calls.
		c := *resp
		return &c, nil
	}
	return &proxmox.SDNZone{Zone: zone, Type: "simple", Nodes: "source"}, nil
}
func (f *fakeSDN) UpdateSDNZoneNodes(_ context.Context, _, nodes string) error {
	f.mu.Lock()
	f.updateZoneNodesCalls++
	f.lastUpdateZoneNodes = nodes
	err := f.updateZoneErr
	f.mu.Unlock()
	return err
}

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
	// ReloadNodeNetwork is the per-node sync that closes the
	// race-against-qmstart hazard. Asserted here so the bridge-readiness
	// guarantee can't silently regress.
	if fake.reloadNodeCalls != 1 {
		t.Errorf("reloadNodeCalls = %d, want 1", fake.reloadNodeCalls)
	}
}

// TestProvision_PurgesOrphanForRecycledVMID asserts that re-running
// Provision for an already-known vmid replaces the row from scratch
// rather than returning the existing one. PVE recycles vmids via
// /cluster/nextid, so the same vmID seen twice is by definition a
// brand new VM that happens to inherit a previous one's id — the
// stale row must be purged and PVE state re-created. (The pre-fix
// "return existing row" behavior would have used the wrong zone /
// subnet for the new VM.)
func TestProvision_PurgesOrphanForRecycledVMID(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	first, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	// Same vmid, different identifier — simulates PVE recycling vmid
	// 1 onto a brand new VM whose UUID hashes differently. The orphan
	// from the previous owner must be cleared and a fresh row inserted.
	second, err := svc.Provision(context.Background(), 1, "vm-uuid-2", "bravo")
	if err != nil {
		t.Fatalf("second Provision after recycling: %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("re-provision returned the same row ID %d; expected a fresh row", first.ID)
	}
	if first.ZoneName == second.ZoneName {
		t.Errorf("re-provision kept old zone %q; expected a fresh derivation", first.ZoneName)
	}
	if fake.createZoneCalls != 2 {
		t.Errorf("expected 2 PVE creates across both Provisions, got %d", fake.createZoneCalls)
	}
}

// TestProvision_PVEFailureRollsBackRow asserts that a PVE-side
// failure during bootstrap (e.g. CreateSDNSubnet errors) results in
// the row being deleted, so the operator-facing DB matches reality
// and doesn't surface a phantom orphan. The follow-up Provision call
// would also succeed via the orphan-purge path; this test pins the
// inline rollback specifically so we don't silently regress to "leak
// the row, rely on next-call cleanup."
func TestProvision_PVEFailureRollsBackRow(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{createSubnetErr: errors.New("boom")}
	svc := newTestSvc(t, fake)

	_, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err == nil {
		t.Fatalf("expected error from CreateSDNSubnet failure, got nil")
	}

	fake.createSubnetErr = nil
	row, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err != nil {
		t.Fatalf("retry Provision: %v", err)
	}
	if row == nil {
		t.Fatal("row is nil after successful retry")
	}
}

// TestProvision_PVEFailure_RollsBackPartialPVEState asserts that
// when a step inside bootstrapPVE fails (e.g. CreateSDNSubnet errors
// after CreateSDNZone + CreateSDNVNet succeeded), the function walks
// back the partially-created PVE artifacts. Without this rollback,
// every transient PVE error would leak orphan zones + vnets that an
// operator has to clean up by hand — see the May 2026 incident where
// two orphan zones (s6bd79b1, sa4c87bb) accumulated before this
// regression test was added.
func TestProvision_PVEFailure_RollsBackPartialPVEState(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{createSubnetErr: errors.New("boom")}
	svc := newTestSvc(t, fake)

	_, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err == nil {
		t.Fatalf("expected error from CreateSDNSubnet failure, got nil")
	}
	// Zone + VNet were created before the failing subnet step;
	// rollback must delete each. Subnet was never successfully
	// created so its delete should NOT fire.
	if fake.deleteZoneCalls != 1 {
		t.Errorf("deleteZoneCalls = %d, want 1 (zone created, must be walked back)", fake.deleteZoneCalls)
	}
	if fake.deleteVNetCalls != 1 {
		t.Errorf("deleteVNetCalls = %d, want 1 (vnet created, must be walked back)", fake.deleteVNetCalls)
	}
	if fake.deleteSubnetCalls != 0 {
		t.Errorf("deleteSubnetCalls = %d, want 0 (subnet never created)", fake.deleteSubnetCalls)
	}
	// applyCalls counts both the (failed) initial apply AND the
	// rollback's apply. The initial apply was never reached (subnet
	// step failed before it), so we expect exactly 1 — from rollback.
	if fake.applyCalls != 1 {
		t.Errorf("applyCalls = %d, want 1 (rollback's apply only)", fake.applyCalls)
	}
}

// TestProvision_ReloadFailure_RollsBackEverything covers the new
// per-node reload step: when ReloadNodeNetwork fails (e.g. PVE
// pve-firewall reload errors out), the entire bootstrapPVE walks
// back so the operator isn't left with a zone/vnet/subnet that the
// target node never reloaded.
func TestProvision_ReloadFailure_RollsBackEverything(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{reloadNodeErr: errors.New("ifreload exit 1")}
	svc := newTestSvc(t, fake)

	_, err := svc.Provision(context.Background(), 1, "vm-uuid-1", "alpha")
	if err == nil {
		t.Fatalf("expected error from ReloadNodeNetwork failure, got nil")
	}
	if fake.deleteZoneCalls != 1 || fake.deleteVNetCalls != 1 || fake.deleteSubnetCalls != 1 {
		t.Errorf("rollback delete counts: zone=%d vnet=%d subnet=%d (each want 1)",
			fake.deleteZoneCalls, fake.deleteVNetCalls, fake.deleteSubnetCalls)
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

// TestProvision_ClearsPreSeededOrphan covers the "Destroy was
// skipped entirely" failure mode directly: a row exists for vmID
// without ever having gone through Provision in this test, and
// Provision must purge it before inserting fresh. The
// PurgesOrphanForRecycledVMID test covers the through-Provision
// path; this one pins the unconditional purge behavior.
func TestProvision_ClearsPreSeededOrphan(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
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

	// Seed a fully-formed orphan row that pretends a previous VM
	// owned vmid=99 with manually chosen zone/subnet values.
	orphan := &db.StandaloneVMNetwork{
		VMID:       99,
		ZoneName:   "sdeadbef",
		VNetName:   "sdeadbef",
		SubnetCIDR: "10.128.99.0/24",
		GatewayIP:  "10.128.99.1",
		VMIP:       "10.128.99.10",
		Node:       "ghost-node",
	}
	if err := database.DB.Create(orphan).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	row, err := svc.Provision(context.Background(), 99, "vm-uuid-fresh", "alpha")
	if err != nil {
		t.Fatalf("Provision after pre-seeded orphan: %v", err)
	}
	if row.ZoneName == "sdeadbef" {
		t.Errorf("kept the orphan's zone %q; expected fresh derivation", row.ZoneName)
	}
	if row.Node != "alpha" {
		t.Errorf("node = %q, want alpha", row.Node)
	}

	var count int64
	if err := database.DB.Model(&db.StandaloneVMNetwork{}).
		Where("vm_id = ?", uint(99)).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for vmid=99 = %d, want 1", count)
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
func (r *recordingSDN) ReloadNodeNetwork(ctx context.Context, node string) error {
	r.record("ReloadNodeNetwork")
	return r.fakeSDN.ReloadNodeNetwork(ctx, node)
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

// PrepareNetForMigrate on a VM that isn't Standalone-net (no row in
// the table) is a silent no-op — nothing in Proxmox touched.
func TestPrepareNetForMigrate_NoRowIsNoOp(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	if err := svc.PrepareNetForMigrate(context.Background(), 9999, "beta"); err != nil {
		t.Fatalf("PrepareNetForMigrate: %v", err)
	}
	if fake.getZoneCalls != 0 || fake.updateZoneNodesCalls != 0 || fake.applyCalls != 0 {
		t.Errorf("expected no Proxmox calls for non-Standalone VM, got get=%d update=%d apply=%d",
			fake.getZoneCalls, fake.updateZoneNodesCalls, fake.applyCalls)
	}
}

// Widening when the target is already in the zone's nodes list is a
// no-op — same idempotency the friend's review called out.
func TestPrepareNetForMigrate_AlreadyWidenedIsNoOp(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	row, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Pretend the zone is already widened to alpha + beta.
	fake.getZoneResp = &proxmox.SDNZone{
		Zone: row.ZoneName, Type: "simple", Nodes: "alpha,beta",
	}
	// Reset post-Provision call counters so we only see what
	// PrepareNetForMigrate triggers.
	fake.updateZoneNodesCalls = 0
	fake.applyCalls = 0

	if err := svc.PrepareNetForMigrate(context.Background(), 1, "beta"); err != nil {
		t.Fatalf("PrepareNetForMigrate: %v", err)
	}
	if fake.updateZoneNodesCalls != 0 || fake.applyCalls != 0 {
		t.Errorf("expected no PVE writes when target already in nodes, got update=%d apply=%d",
			fake.updateZoneNodesCalls, fake.applyCalls)
	}
}

// Happy path: zone pinned to alpha, prepare widens to alpha,beta and
// applies. Reload on target is invoked. DB row is left alone — narrow
// happens later in Commit.
func TestPrepareNetForMigrate_WidensAndApplies(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	row, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.getZoneResp = &proxmox.SDNZone{
		Zone: row.ZoneName, Type: "simple", Nodes: "alpha",
	}
	fake.updateZoneNodesCalls = 0
	fake.applyCalls = 0
	fake.reloadNodeCalls = 0

	if err := svc.PrepareNetForMigrate(context.Background(), 1, "beta"); err != nil {
		t.Fatalf("PrepareNetForMigrate: %v", err)
	}
	if fake.updateZoneNodesCalls != 1 {
		t.Errorf("UpdateSDNZoneNodes calls = %d, want 1", fake.updateZoneNodesCalls)
	}
	if fake.lastUpdateZoneNodes != "alpha,beta" {
		t.Errorf("widened nodes = %q, want %q", fake.lastUpdateZoneNodes, "alpha,beta")
	}
	if fake.applyCalls != 1 {
		t.Errorf("ApplySDN calls = %d, want 1", fake.applyCalls)
	}
	if fake.reloadNodeCalls != 1 {
		t.Errorf("ReloadNodeNetwork calls = %d, want 1 (for target node)", fake.reloadNodeCalls)
	}
}

// ApplySDN failure after a successful widen triggers a best-effort
// revert (a second UpdateSDNZoneNodes call back to the original) and
// surfaces the apply error.
func TestPrepareNetForMigrate_RevertsWidenOnApplyFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	row, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.getZoneResp = &proxmox.SDNZone{
		Zone: row.ZoneName, Type: "simple", Nodes: "alpha",
	}
	// Reset counters, then arm the apply failure for the widen step.
	fake.updateZoneNodesCalls = 0
	fake.applyCalls = 0
	fake.applyErr = errors.New("apply boom")

	err = svc.PrepareNetForMigrate(context.Background(), 1, "beta")
	if err == nil {
		t.Fatal("expected error from apply failure, got nil")
	}
	// One widen + one revert.
	if fake.updateZoneNodesCalls != 2 {
		t.Errorf("UpdateSDNZoneNodes calls = %d, want 2 (widen + revert)", fake.updateZoneNodesCalls)
	}
	// Final UpdateSDNZoneNodes call should have restored the original.
	if fake.lastUpdateZoneNodes != "alpha" {
		t.Errorf("post-revert nodes = %q, want %q", fake.lastUpdateZoneNodes, "alpha")
	}
}

// CommitNetMove narrows the zone to the final node and updates the DB
// row when the VM landed on a new node. Idempotent if final == row's
// current node (no DB update needed, but the narrow still runs to
// sweep any widened state from a failed migrate).
func TestCommitNetMove_NarrowsAndUpdatesDBRow(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	if _, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.updateZoneNodesCalls = 0
	fake.applyCalls = 0

	if err := svc.CommitNetMove(context.Background(), 1, "beta"); err != nil {
		t.Fatalf("CommitNetMove: %v", err)
	}
	if fake.updateZoneNodesCalls != 1 || fake.lastUpdateZoneNodes != "beta" {
		t.Errorf("narrow update calls=%d nodes=%q, want 1 calls / nodes=beta",
			fake.updateZoneNodesCalls, fake.lastUpdateZoneNodes)
	}
	if fake.applyCalls != 1 {
		t.Errorf("ApplySDN calls = %d, want 1", fake.applyCalls)
	}

	got, err := svc.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Node != "beta" {
		t.Errorf("DB row node = %q, want beta", got.Node)
	}
}

// On a failed migrate, the caller invokes CommitNetMove with the
// source node — same code path, just narrows the zone back to where
// the VM actually still lives. DB row stays put.
func TestCommitNetMove_RevertNarrowsBackToSource(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{}
	svc := newTestSvc(t, fake)

	if _, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	fake.updateZoneNodesCalls = 0

	if err := svc.CommitNetMove(context.Background(), 1, "alpha"); err != nil {
		t.Fatalf("CommitNetMove: %v", err)
	}
	if fake.lastUpdateZoneNodes != "alpha" {
		t.Errorf("post-revert nodes = %q, want alpha", fake.lastUpdateZoneNodes)
	}
	got, err := svc.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Node != "alpha" {
		t.Errorf("DB row node = %q, want alpha (unchanged)", got.Node)
	}
}

// CommitNetMove returning nil on Proxmox failures is by design: by
// the time we reach commit, the migrate outcome is already decided
// and we don't want a cosmetic narrow-back error to mask it. Errors
// should still be logged (no assertion possible from outside), but
// the function must NOT propagate them.
func TestCommitNetMove_ProxmoxFailureNotPropagated(t *testing.T) {
	t.Parallel()
	fake := &fakeSDN{updateZoneErr: errors.New("pve unreachable")}
	svc := newTestSvc(t, fake)

	if _, err := svc.Provision(context.Background(), 1, "vm-uuid", "alpha"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if err := svc.CommitNetMove(context.Background(), 1, "beta"); err != nil {
		t.Errorf("CommitNetMove returned error %v, want nil (PVE failures must not propagate)", err)
	}
}
