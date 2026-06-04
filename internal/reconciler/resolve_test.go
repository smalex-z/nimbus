package reconciler_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
	"nimbus/internal/reconciler"
)

// fakeDeps is the minimal proxmox surface ResolveDeps requires. Each
// method records call args so tests can assert side effects without a
// real PVE.
type fakeDeps struct {
	mu sync.Mutex

	vmConfig      map[string]any
	vmConfigErr   error
	setTags       []string
	setDesc       string
	stopCalled    bool
	destroyCalled bool
	destroyErr    error
}

func (f *fakeDeps) GetVMConfig(_ context.Context, _ string, _ int) (map[string]any, error) {
	if f.vmConfigErr != nil {
		return nil, f.vmConfigErr
	}
	if f.vmConfig == nil {
		return map[string]any{}, nil
	}
	return f.vmConfig, nil
}
func (f *fakeDeps) SetVMTags(_ context.Context, _ string, _ int, tags []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setTags = append([]string(nil), tags...)
	return nil
}
func (f *fakeDeps) SetVMDescription(_ context.Context, _ string, _ int, desc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setDesc = desc
	return nil
}
func (f *fakeDeps) StopVM(_ context.Context, _ string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalled = true
	return "UPID:test", nil
}
func (f *fakeDeps) DestroyVM(_ context.Context, _ string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalled = true
	if f.destroyErr != nil {
		return "", f.destroyErr
	}
	return "UPID:test", nil
}
func (f *fakeDeps) WaitForTask(_ context.Context, _, _ string, _ time.Duration) error {
	return nil
}

func newReconcilerForResolve(t *testing.T) (*reconciler.Reconciler, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolve.db")
	database, err := db.New(path, &db.VM{}, &db.VMDivergence{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	r := reconciler.New(&fakePVE{}, database.DB)
	r.SetClock(func() time.Time { return time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC) })
	return r, database
}

// TestImportExternal_HappyPath: an external-unmanaged divergence
// becomes a vms row + Proxmox tag/description stamp + resolved
// divergence.
func TestImportExternal_HappyPath(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	div := db.VMDivergence{
		Type:            "external-unmanaged",
		VMID:            300,
		Node:            "alpha",
		Hostname:        "operator-built",
		DetectedAt:      time.Now(),
		FirstDetectedAt: time.Now(),
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed divergence: %v", err)
	}
	deps := &fakeDeps{vmConfig: map[string]any{
		"tags":        "production",
		"description": "ops VM",
		"smbios1":     "uuid=11111111-2222-3333-4444-555566667777",
	}}

	res, err := r.ImportExternal(context.Background(), div.ID, deps, reconciler.ResolveActor{Email: "admin@example.com"})
	if err != nil {
		t.Fatalf("ImportExternal: %v", err)
	}
	if res.NimbusID == "" || res.VMRowID == 0 {
		t.Errorf("ImportExternal result missing fields: %+v", res)
	}

	var vm db.VM
	if err := database.DB.First(&vm, res.VMRowID).Error; err != nil {
		t.Fatalf("read new vm: %v", err)
	}
	if vm.NimbusID != res.NimbusID {
		t.Errorf("vm.NimbusID = %q, want %q", vm.NimbusID, res.NimbusID)
	}
	if vm.SMBIOSID != "11111111-2222-3333-4444-555566667777" {
		t.Errorf("vm.SMBIOSID = %q, want smbios from cfg", vm.SMBIOSID)
	}
	if vm.Tier != "imported" {
		t.Errorf("vm.Tier = %q, want imported", vm.Tier)
	}

	// PVE was stamped with the new id.
	hasShort := false
	for _, tag := range deps.setTags {
		if tag == proxmox.NimbusIDTagPrefix+proxmox.ShortNimbusID(res.NimbusID) {
			hasShort = true
		}
	}
	if !hasShort {
		t.Errorf("PVE tags missing nimbus-id tag: %v", deps.setTags)
	}
	if deps.setDesc == "" || !contains(deps.setDesc, res.NimbusID) {
		t.Errorf("PVE description %q does not carry nimbus_id %s", deps.setDesc, res.NimbusID)
	}

	// Divergence flipped to resolved.
	var reloaded db.VMDivergence
	if err := database.DB.First(&reloaded, div.ID).Error; err != nil {
		t.Fatalf("reload divergence: %v", err)
	}
	if reloaded.ResolvedAt == nil || reloaded.ResolvedAction != "imported" || reloaded.ResolvedBy != "admin@example.com" {
		t.Errorf("divergence after resolve: resolved_at=%v action=%q by=%q", reloaded.ResolvedAt, reloaded.ResolvedAction, reloaded.ResolvedBy)
	}
}

// TestImportExternal_WrongType: Import on an orphan returns a typed
// error so the HTTP layer can map to 409.
func TestImportExternal_WrongType(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	div := db.VMDivergence{Type: "orphaned", VMID: 200, Node: "alpha", DetectedAt: time.Now(), FirstDetectedAt: time.Now()}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := r.ImportExternal(context.Background(), div.ID, &fakeDeps{}, reconciler.ResolveActor{})
	var wrong *reconciler.ErrWrongDivergenceType
	if !errors.As(err, &wrong) {
		t.Errorf("err = %v, want ErrWrongDivergenceType", err)
	}
}

// TestAdopt_LinksExistingRow: a tagged-orphan with a matching DB row
// updates the row's vmid + node and resolves the divergence.
func TestAdopt_LinksExistingRow(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	vm := db.VM{
		VMID: 500, Hostname: "remembered", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.50",
	}
	if err := database.Create(&vm).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	div := db.VMDivergence{
		Type: "external-tagged-orphan", VMID: 600, Node: "beta", NimbusID: id,
		DetectedAt: time.Now(), FirstDetectedAt: time.Now(),
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed div: %v", err)
	}

	if _, err := r.Adopt(context.Background(), div.ID, &fakeDeps{}, reconciler.ResolveActor{Email: "admin@example.com"}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	var reloaded db.VM
	if err := database.DB.First(&reloaded, vm.ID).Error; err != nil {
		t.Fatalf("reload vm: %v", err)
	}
	if reloaded.VMID != 600 || reloaded.Node != "beta" {
		t.Errorf("after Adopt: vmid=%d node=%q, want 600/beta", reloaded.VMID, reloaded.Node)
	}
}

// TestAdopt_NoMatchingRow: tagged-orphan with no DB row by that
// nimbus_id returns a clear error (operator's path forward is
// force-delete or wait for re-import).
func TestAdopt_NoMatchingRow(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	div := db.VMDivergence{
		Type: "external-tagged-orphan", VMID: 700, Node: "beta",
		NimbusID:   "0193abcd-4321-7000-89ab-cdef01234567",
		DetectedAt: time.Now(), FirstDetectedAt: time.Now(),
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := r.Adopt(context.Background(), div.ID, &fakeDeps{}, reconciler.ResolveActor{Email: "admin@example.com"})
	if err == nil || !contains(err.Error(), "no DB row") {
		t.Errorf("Adopt with no match should error mentioning 'no DB row', got: %v", err)
	}
}

// TestMarkDeleted_DropsRowAndResolves: orphan resolution removes the
// local row and flips the divergence to resolved.
func TestMarkDeleted_DropsRowAndResolves(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	vm := db.VM{
		VMID: 800, Hostname: "ghost", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running",
	}
	if err := database.Create(&vm).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	div := db.VMDivergence{
		Type: "orphaned", VMID: 800, Node: "alpha",
		DetectedAt: time.Now(), FirstDetectedAt: time.Now(),
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed div: %v", err)
	}

	if _, err := r.MarkDeleted(context.Background(), div.ID, &fakeDeps{}, reconciler.ResolveActor{Email: "admin@example.com"}); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	var count int64
	if err := database.DB.Unscoped().Model(&db.VM{}).Where("vmid = ?", 800).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("vm row count = %d, want 0 (mark-deleted should hard-delete)", count)
	}
}

// TestForceDelete_DestroysVMAndDropsRow: stops + destroys via the
// Proxmox surface and removes the local row.
func TestForceDelete_DestroysVMAndDropsRow(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	div := db.VMDivergence{
		Type: "external-tagged-orphan", VMID: 900, Node: "beta",
		NimbusID:        "0193abcd-4321-7000-89ab-cdef01234567",
		DetectedAt:      time.Now(),
		FirstDetectedAt: time.Now(),
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed div: %v", err)
	}
	deps := &fakeDeps{}
	if _, err := r.ForceDelete(context.Background(), div.ID, deps, reconciler.ResolveActor{Email: "admin@example.com"}); err != nil {
		t.Fatalf("ForceDelete: %v", err)
	}
	if !deps.stopCalled || !deps.destroyCalled {
		t.Errorf("expected Stop+Destroy on PVE, got stop=%v destroy=%v", deps.stopCalled, deps.destroyCalled)
	}
	var reloaded db.VMDivergence
	if err := database.DB.First(&reloaded, div.ID).Error; err != nil {
		t.Fatalf("reload divergence: %v", err)
	}
	if reloaded.ResolvedAction != "force-deleted" {
		t.Errorf("ResolvedAction = %q, want force-deleted", reloaded.ResolvedAction)
	}
}

// TestResolve_NotFoundForResolved: trying to resolve an already-
// resolved divergence returns ErrDivergenceNotFound so the HTTP
// layer surfaces 404 (preserves the idempotent UX).
func TestResolve_NotFoundForResolved(t *testing.T) {
	t.Parallel()
	r, database := newReconcilerForResolve(t)
	now := time.Now()
	div := db.VMDivergence{
		Type: "orphaned", VMID: 1000, Node: "alpha",
		DetectedAt: now, FirstDetectedAt: now,
		ResolvedAt: &now, ResolvedBy: "previous-admin", ResolvedAction: "marked-deleted",
	}
	if err := database.Create(&div).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := r.MarkDeleted(context.Background(), div.ID, &fakeDeps{}, reconciler.ResolveActor{})
	if !errors.Is(err, reconciler.ErrDivergenceNotFound) {
		t.Errorf("err = %v, want ErrDivergenceNotFound", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
