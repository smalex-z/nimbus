package vnetmgr_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/ippool"
	"nimbus/internal/vnetmgr"
)

// fakePool tracks Seed/Drop calls without spinning up the real ippool
// machinery — vnetmgr just needs to know the calls happened with the
// right vnet name.
type fakePool struct {
	seedCalls  int
	dropCalls  int
	seedFor    []string // vnets seeded
	dropFor    []string // vnets dropped
	dropErrFor string   // if set, DropSubnet for this vnet returns ErrSubnetInUse
	seedErrFor string   // if set, SeedSubnet for this vnet returns an error
}

func (f *fakePool) SeedSubnet(_ context.Context, vnet, _, _ string) error {
	f.seedCalls++
	f.seedFor = append(f.seedFor, vnet)
	if f.seedErrFor == vnet {
		return errors.New("simulated seed failure")
	}
	return nil
}

func (f *fakePool) DropSubnet(_ context.Context, vnet string) error {
	f.dropCalls++
	f.dropFor = append(f.dropFor, vnet)
	if f.dropErrFor == vnet {
		return ippool.ErrSubnetInUse
	}
	return nil
}

// fakeVMRefs lets tests force the "VMs still attached" failure mode
// of DeleteSubnet.
type fakeVMRefs struct {
	count int
	err   error
}

func (f *fakeVMRefs) CountVMsOnSubnet(_ context.Context, _ uint) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.count, nil
}

// newSubnetTestSvc spins up a fresh SQLite DB + a vnetmgr.Service wired
// with the SDN/pool/VM-ref fakes. Default settings: SDN enabled, zone
// "nimbus", supernet 10.42.0.0/16, subnet size /24.
func newSubnetTestSvc(t *testing.T) (*vnetmgr.Service, *fakeSDN, *fakePool, *fakeVMRefs, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vnetmgr.db")
	database, err := db.New(path,
		&db.User{}, &db.NetworkSettings{}, &db.UserSubnet{},
		&db.VM{}, &db.NodeTemplate{}, &db.SSHKey{},
		ippool.Model(),
	)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	// Seed the singleton settings row with SDN enabled.
	if err := database.Save(&db.NetworkSettings{
		ID:                1,
		SDNEnabled:        true,
		SDNZoneName:       "nimbus",
		SDNZoneType:       "simple",
		SDNSubnetSupernet: "10.42.0.0/16",
		SDNSubnetSize:     24,
	}).Error; err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	px := &fakeSDN{}
	pool := &fakePool{}
	vmRefs := &fakeVMRefs{}
	svc := vnetmgr.New(px, &settingsFromDB{db: database}).
		WithDB(database.DB).
		WithPool(pool).
		WithVMRefCounter(vmRefs)
	return svc, px, pool, vmRefs, database
}

// settingsFromDB is a SettingsReader that reads NetworkSettings from
// a real DB so tests can mutate the row mid-flight (e.g., to disable
// SDN and assert CreateSubnet fails).
type settingsFromDB struct {
	db *db.DB
}

func (s *settingsFromDB) GetNetworkSettings() (*db.NetworkSettings, error) {
	var row db.NetworkSettings
	err := s.db.First(&row, 1).Error
	return &row, err
}

func TestCreateSubnet_HappyPath(t *testing.T) {
	t.Parallel()
	svc, px, pool, _, _ := newSubnetTestSvc(t)

	row, err := svc.CreateSubnet(context.Background(), vnetmgr.CreateSubnetRequest{
		OwnerID: 1,
		Name:    "default",
	})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if row.ID == 0 {
		t.Errorf("row.ID is zero; expected populated post-insert")
	}
	if row.VNet == "" || row.VNet[:3] != "nbu" {
		t.Errorf("VNet = %q, expected nbu-prefixed", row.VNet)
	}
	if row.Subnet != "10.42.0.0/24" {
		t.Errorf("Subnet = %q, want 10.42.0.0/24 (first carve)", row.Subnet)
	}
	if row.Gateway != "10.42.0.1" {
		t.Errorf("Gateway = %q, want 10.42.0.1", row.Gateway)
	}
	if row.PoolStart != "10.42.0.10" {
		t.Errorf("PoolStart = %q, want 10.42.0.10", row.PoolStart)
	}
	if px.createVNetCnt.Load() != 1 || px.createSubCnt.Load() != 1 || px.applyCalls.Load() != 1 {
		t.Errorf("expected 1 vnet+subnet+apply; got vnet=%d sub=%d apply=%d",
			px.createVNetCnt.Load(), px.createSubCnt.Load(), px.applyCalls.Load())
	}
	if pool.seedCalls != 1 {
		t.Errorf("expected 1 seed, got %d", pool.seedCalls)
	}
}

func TestCreateSubnet_FirstFreeCarving(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	// First carve → 10.42.0.0/24
	a, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "a"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if a.Subnet != "10.42.0.0/24" {
		t.Errorf("first carve = %q, want 10.42.0.0/24", a.Subnet)
	}
	// Second carve → 10.42.1.0/24 (next free /24)
	b, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "b"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if b.Subnet != "10.42.1.0/24" {
		t.Errorf("second carve = %q, want 10.42.1.0/24", b.Subnet)
	}
	// Different user, third carve → 10.42.2.0/24 (carving is global,
	// not per-user — supernet is shared across users).
	c, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 2, Name: "c"})
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	if c.Subnet != "10.42.2.0/24" {
		t.Errorf("third carve = %q, want 10.42.2.0/24", c.Subnet)
	}
}

func TestCreateSubnet_RejectsDuplicateNamePerOwner(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	_, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "web"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "web"})
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError on duplicate, got %T %v", err, err)
	}
	// Different owner, same name → allowed.
	_, err = svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 2, Name: "web"})
	if err != nil {
		t.Errorf("different owner same name should succeed, got %v", err)
	}
}

func TestCreateSubnet_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	// Note: uppercase is normalized to lowercase before validation, so
	// "Has-Caps" → "has-caps" → valid. Validation rejects only shapes
	// that don't match the DNS-label pattern after lowercasing.
	bad := []string{"", "1starts-with-digit", "ends-with-dash-", "with spaces"}
	for _, name := range bad {
		_, err := svc.CreateSubnet(context.Background(), vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: name})
		var validation *internalerrors.ValidationError
		if !errors.As(err, &validation) {
			t.Errorf("name %q: expected ValidationError, got %T %v", name, err, err)
		}
	}
}

func TestCreateSubnet_RefusesWhenSDNDisabled(t *testing.T) {
	t.Parallel()
	svc, _, _, _, database := newSubnetTestSvc(t)
	if err := database.Model(&db.NetworkSettings{}).Where("id = ?", 1).
		Update("sdn_enabled", false).Error; err != nil {
		t.Fatalf("disable sdn: %v", err)
	}
	_, err := svc.CreateSubnet(context.Background(), vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "x"})
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError for SDN-disabled, got %T %v", err, err)
	}
}

func TestCreateSubnet_SetDefaultClearsOthers(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	first, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "a", SetDefault: true})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if !first.IsDefault {
		t.Errorf("first.IsDefault = false, expected true")
	}

	second, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "b", SetDefault: true})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	// Re-read first; should no longer be default.
	got, err := svc.GetSubnet(ctx, first.ID, 1)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if got.IsDefault {
		t.Errorf("first should be non-default after second was set as default")
	}
	if !second.IsDefault {
		t.Errorf("second.IsDefault = false, expected true")
	}
}

func TestEnsureDefault_LazyCreatesOnFirstCall(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	row, err := svc.EnsureDefault(ctx, 1)
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if row.Name != "default" {
		t.Errorf("name = %q, want 'default'", row.Name)
	}
	if !row.IsDefault {
		t.Errorf("IsDefault = false, expected true")
	}
	// Second call returns the same row.
	row2, err := svc.EnsureDefault(ctx, 1)
	if err != nil {
		t.Fatalf("EnsureDefault (idempotent): %v", err)
	}
	if row2.ID != row.ID {
		t.Errorf("idempotent call returned different id: %d vs %d", row2.ID, row.ID)
	}
}

func TestDeleteSubnet_RefusedWhileVMsAttached(t *testing.T) {
	t.Parallel()
	svc, _, _, vmRefs, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	row, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "x"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	vmRefs.count = 3 // simulate 3 VMs attached

	err = svc.DeleteSubnet(ctx, row.ID, 1)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T %v", err, err)
	}
}

func TestDeleteSubnet_RefusedForOnlyDefault(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	row, err := svc.EnsureDefault(ctx, 1)
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	err = svc.DeleteSubnet(ctx, row.ID, 1)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError for only-default delete, got %T %v", err, err)
	}
}

func TestDeleteSubnet_HappyPathTearsDownProxmoxAndPool(t *testing.T) {
	t.Parallel()
	svc, px, pool, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	// Two subnets so we can delete the first without tripping the
	// only-default guard.
	a, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "a", SetDefault: true})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "b", SetDefault: true}); err != nil {
		t.Fatalf("create b: %v", err)
	}

	if err := svc.DeleteSubnet(ctx, a.ID, 1); err != nil {
		t.Fatalf("DeleteSubnet: %v", err)
	}
	if px.deleteVNetCnt.Load() != 1 || px.deleteSubCnt.Load() != 1 {
		t.Errorf("expected 1 vnet+subnet delete; got vnet=%d sub=%d",
			px.deleteVNetCnt.Load(), px.deleteSubCnt.Load())
	}
	if pool.dropCalls != 1 {
		t.Errorf("expected 1 pool drop, got %d", pool.dropCalls)
	}
}

func TestGetSubnet_OwnerGated(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	row, err := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "x"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	// Owner 2 trying to read user 1's subnet → NotFound (never disclose).
	_, err = svc.GetSubnet(ctx, row.ID, 2)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected NotFoundError, got %T %v", err, err)
	}
}

func TestSetDefault_FlipsExclusively(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSubnetTestSvc(t)
	ctx := context.Background()

	a, _ := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "a", SetDefault: true})
	b, _ := svc.CreateSubnet(ctx, vnetmgr.CreateSubnetRequest{OwnerID: 1, Name: "b"})

	if err := svc.SetDefault(ctx, b.ID, 1); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	got, _ := svc.GetSubnet(ctx, a.ID, 1)
	if got.IsDefault {
		t.Errorf("a should be non-default after b was set as default")
	}
	got, _ = svc.GetSubnet(ctx, b.ID, 1)
	if !got.IsDefault {
		t.Errorf("b should be default")
	}
}
