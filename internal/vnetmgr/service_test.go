package vnetmgr_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
	"nimbus/internal/vnetmgr"
)

// fakeSDN is the test double for vnetmgr.SDNClient. Each method is a
// function field the test overrides; defaults are no-ops returning nil
// so a test only has to wire what it cares about.
type fakeSDN struct {
	getZone       func(ctx context.Context, zone string) (*proxmox.SDNZone, error)
	createZone    func(ctx context.Context, z proxmox.SDNZone) error
	listVNets     func(ctx context.Context) ([]proxmox.SDNVNet, error)
	applySDN      func(ctx context.Context) error
	createVNet    func(ctx context.Context, v proxmox.SDNVNet) error
	deleteVNet    func(ctx context.Context, vnet string) error
	createSubnet  func(ctx context.Context, s proxmox.SDNSubnet) error
	deleteSubnet  func(ctx context.Context, vnet, subnet string) error
	createCalls   atomic.Int32
	applyCalls    atomic.Int32
	createVNetCnt atomic.Int32
	createSubCnt  atomic.Int32
	deleteVNetCnt atomic.Int32
	deleteSubCnt  atomic.Int32
}

func (f *fakeSDN) GetSDNZone(ctx context.Context, zone string) (*proxmox.SDNZone, error) {
	if f.getZone == nil {
		return nil, proxmox.ErrNotFound
	}
	return f.getZone(ctx, zone)
}

func (f *fakeSDN) CreateSDNZone(ctx context.Context, z proxmox.SDNZone) error {
	f.createCalls.Add(1)
	if f.createZone == nil {
		return nil
	}
	return f.createZone(ctx, z)
}

func (f *fakeSDN) ListSDNVNets(ctx context.Context) ([]proxmox.SDNVNet, error) {
	if f.listVNets == nil {
		return nil, nil
	}
	return f.listVNets(ctx)
}

func (f *fakeSDN) ApplySDN(ctx context.Context) error {
	f.applyCalls.Add(1)
	if f.applySDN == nil {
		return nil
	}
	return f.applySDN(ctx)
}

func (f *fakeSDN) CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error {
	f.createVNetCnt.Add(1)
	if f.createVNet == nil {
		return nil
	}
	return f.createVNet(ctx, v)
}

func (f *fakeSDN) DeleteSDNVNet(ctx context.Context, vnet string) error {
	f.deleteVNetCnt.Add(1)
	if f.deleteVNet == nil {
		return nil
	}
	return f.deleteVNet(ctx, vnet)
}

func (f *fakeSDN) CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error {
	f.createSubCnt.Add(1)
	if f.createSubnet == nil {
		return nil
	}
	return f.createSubnet(ctx, s)
}

func (f *fakeSDN) DeleteSDNSubnet(ctx context.Context, vnet, subnet string) error {
	f.deleteSubCnt.Add(1)
	if f.deleteSubnet == nil {
		return nil
	}
	return f.deleteSubnet(ctx, vnet, subnet)
}

// fakeSettings is a SettingsReader stub. Tests set Settings before
// invoking the service.
type fakeSettings struct {
	Settings *db.NetworkSettings
	Err      error
}

func (f *fakeSettings) GetNetworkSettings() (*db.NetworkSettings, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Settings, nil
}

func TestBootstrap_DisabledNoOp(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{SDNEnabled: false}})

	if err := svc.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if px.createCalls.Load() != 0 {
		t.Errorf("disabled bootstrap made %d create calls — should make 0", px.createCalls.Load())
	}
	if px.applyCalls.Load() != 0 {
		t.Errorf("disabled bootstrap made %d apply calls — should make 0", px.applyCalls.Load())
	}
}

func TestBootstrap_EnabledMissingZoneCreatesAndApplies(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return nil, proxmox.ErrNotFound
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	if err := svc.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if px.createCalls.Load() != 1 {
		t.Errorf("expected 1 create call, got %d", px.createCalls.Load())
	}
	if px.applyCalls.Load() != 1 {
		t.Errorf("expected 1 apply call, got %d", px.applyCalls.Load())
	}
}

func TestBootstrap_EnabledZoneAlreadyExistsNoOp(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return &proxmox.SDNZone{Zone: "nimbus", Type: "simple"}, nil
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	if err := svc.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if px.createCalls.Load() != 0 {
		t.Errorf("zone already exists, expected 0 create calls, got %d", px.createCalls.Load())
	}
	if px.applyCalls.Load() != 0 {
		t.Errorf("zone already exists, expected 0 apply calls, got %d", px.applyCalls.Load())
	}
}

func TestBootstrap_TypeMismatchReturnsTypedError(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return &proxmox.SDNZone{Zone: "nimbus", Type: "vxlan"}, nil
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	err := svc.Bootstrap(context.Background())
	var mismatch *vnetmgr.ZoneTypeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected ZoneTypeMismatchError, got %T %v", err, err)
	}
	if mismatch.WantType != "simple" || mismatch.ProxmoxType != "vxlan" {
		t.Errorf("mismatch fields = %+v, want simple↔vxlan", mismatch)
	}
}

func TestBootstrap_SDNPackageMissing(t *testing.T) {
	t.Parallel()
	// Proxmox returns 501 when the SDN package isn't installed at all
	// — we surface the structured ErrSDNPackageMissing so the handler
	// can render an actionable hint.
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return nil, &proxmox.HTTPError{Status: 501, Method: "GET", Path: "/cluster/sdn/zones/nimbus"}
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	err := svc.Bootstrap(context.Background())
	if !errors.Is(err, vnetmgr.ErrSDNPackageMissing) {
		t.Fatalf("expected ErrSDNPackageMissing, got %T %v", err, err)
	}
	if px.createCalls.Load() != 0 {
		t.Errorf("missing-pkg should not attempt create, got %d calls", px.createCalls.Load())
	}
}

func TestBootstrap_EmptyZoneNameRejected(t *testing.T) {
	t.Parallel()
	svc := vnetmgr.New(&fakeSDN{}, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "",
		SDNZoneType: "simple",
	}})

	err := svc.Bootstrap(context.Background())
	if err == nil {
		t.Fatal("expected error for empty zone name")
	}
}

func TestStatus_DisabledReportsCleanly(t *testing.T) {
	t.Parallel()
	svc := vnetmgr.New(&fakeSDN{}, &fakeSettings{Settings: &db.NetworkSettings{SDNEnabled: false}})

	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ZoneStatus != "disabled" {
		t.Errorf("ZoneStatus = %q, want disabled", st.ZoneStatus)
	}
}

func TestStatus_ActiveCountsVNets(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return &proxmox.SDNZone{Zone: "nimbus", Type: "simple"}, nil
		},
		listVNets: func(_ context.Context) ([]proxmox.SDNVNet, error) {
			return []proxmox.SDNVNet{
				{VNet: "nbu1", Zone: "nimbus"},
				{VNet: "nbu2", Zone: "nimbus"},
				// Out-of-zone VNet should not be counted.
				{VNet: "other", Zone: "external"},
			}, nil
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ZoneStatus != "active" {
		t.Errorf("ZoneStatus = %q, want active", st.ZoneStatus)
	}
	if st.VNetCount != 2 {
		t.Errorf("VNetCount = %d, want 2 (other-zone vnet should not be counted)", st.VNetCount)
	}
}

func TestStatus_PendingWhenZoneNotInProxmox(t *testing.T) {
	t.Parallel()
	px := &fakeSDN{
		getZone: func(_ context.Context, _ string) (*proxmox.SDNZone, error) {
			return nil, proxmox.ErrNotFound
		},
	}
	svc := vnetmgr.New(px, &fakeSettings{Settings: &db.NetworkSettings{
		SDNEnabled:  true,
		SDNZoneName: "nimbus",
		SDNZoneType: "simple",
	}})

	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ZoneStatus != "pending" {
		t.Errorf("ZoneStatus = %q, want pending", st.ZoneStatus)
	}
}
