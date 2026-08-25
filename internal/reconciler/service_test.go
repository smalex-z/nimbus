package reconciler_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
	"nimbus/internal/reconciler"
)

// fakePVE is the minimum surface the reconciler talks to.
type fakePVE struct {
	details []proxmox.ClusterVMDetail
	err     error
}

func (f *fakePVE) GetClusterVMDetails(_ context.Context) ([]proxmox.ClusterVMDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.details, nil
}

func newReconciler(t *testing.T, pve *fakePVE) (*reconciler.Reconciler, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.db")
	database, err := db.New(path, &db.VM{}, &db.VMDivergence{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	r := reconciler.New(pve, database.DB)
	r.SetClock(func() time.Time { return time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC) })
	return r, database
}

// withMarker returns a ClusterVMDetail with the nimbus tags + description
// marker already stamped — what a Nimbus-managed VM looks like on PVE.
func withMarker(vmid int, node, name, nimbusID, status string) proxmox.ClusterVMDetail {
	short := proxmox.ShortNimbusID(nimbusID)
	tags := []string{proxmox.NimbusMarkerTag}
	if short != "" {
		tags = append(tags, proxmox.NimbusIDTagPrefix+short)
	}
	return proxmox.ClusterVMDetail{
		VMID:        vmid,
		Node:        node,
		Name:        name,
		Status:      status,
		Tags:        tags,
		Description: proxmox.EncodeNimbusDescription("small", "ubuntu-22.04", nimbusID),
	}
}

// TestReconcile_HealthyMatchOnNimbusID: a DB row + PVE VM that share
// nimbus_id are reported as healthy, no divergence row inserted.
func TestReconcile_HealthyMatchOnNimbusID(t *testing.T) {
	t.Parallel()
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(200, "alpha", "vm-200", id, "running"),
	}}
	r, database := newReconciler(t, pve)
	if err := database.Create(&db.VM{
		VMID: 200, Hostname: "vm-200", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.200",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Healthy != 1 {
		t.Errorf("Healthy = %d, want 1", rep.Healthy)
	}
	if rep.Opened != 0 {
		t.Errorf("Opened = %d, want 0", rep.Opened)
	}
	var divergences int64
	if err := database.DB.Model(&db.VMDivergence{}).Count(&divergences).Error; err != nil {
		t.Fatalf("count divergences: %v", err)
	}
	if divergences != 0 {
		t.Errorf("divergence rows = %d, want 0", divergences)
	}
}

// TestReconcile_OrphanedDBRow: DB row with no PVE counterpart opens an
// "orphaned" divergence and does NOT delete the row (spec: never auto-
// delete). A second healthy VM keeps the snapshot non-empty so the
// ErrEmptyClusterSnapshot guard doesn't fire.
func TestReconcile_OrphanedDBRow(t *testing.T) {
	t.Parallel()
	const healthyID = "0193ffff-eeee-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(999, "alpha", "other", healthyID, "running"),
	}}
	r, database := newReconciler(t, pve)
	if err := database.Create(&db.VM{
		VMID: 999, Hostname: "other", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: healthyID,
		IP: "10.0.0.999",
	}).Error; err != nil {
		t.Fatalf("seed healthy vm: %v", err)
	}

	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	if err := database.Create(&db.VM{
		VMID: 200, Hostname: "gone", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.200",
	}).Error; err != nil {
		t.Fatalf("seed orphan vm: %v", err)
	}

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Orphaned != 1 || rep.Opened != 1 {
		t.Errorf("Orphaned=%d Opened=%d, want 1/1", rep.Orphaned, rep.Opened)
	}
	// Spec: row must NOT be auto-deleted (admin action in #300 does this).
	var n int64
	if err := database.DB.Unscoped().Model(&db.VM{}).Where("hostname = ?", "gone").Count(&n).Error; err != nil {
		t.Fatalf("count vms: %v", err)
	}
	if n != 1 {
		t.Errorf("vm row count = %d, want 1 (must NOT auto-delete)", n)
	}

	var div db.VMDivergence
	if err := database.DB.Where("type = ?", reconciler.TypeOrphaned).First(&div).Error; err != nil {
		t.Fatalf("read orphan divergence: %v", err)
	}
	if div.NimbusID != id {
		t.Errorf("NimbusID = %q, want %q", div.NimbusID, id)
	}
}

// TestReconcile_ExternalUnmanaged: a PVE VM with no nimbus tag is
// classified as external-unmanaged. No DB row is created (spec).
func TestReconcile_ExternalUnmanaged(t *testing.T) {
	t.Parallel()
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		{VMID: 300, Node: "alpha", Name: "operator-built", Status: "running"},
	}}
	r, database := newReconciler(t, pve)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.External != 1 || rep.Opened != 1 {
		t.Errorf("External=%d Opened=%d, want 1/1", rep.External, rep.Opened)
	}
	var vms int64
	if err := database.DB.Model(&db.VM{}).Count(&vms).Error; err != nil {
		t.Fatalf("count vms: %v", err)
	}
	if vms != 0 {
		t.Errorf("vm row count = %d, want 0 (must NOT auto-create)", vms)
	}
}

// TestReconcile_ExternalTaggedOrphan: PVE VM with a nimbus_id in its
// description but no DB row classifies as external-tagged-orphan.
func TestReconcile_ExternalTaggedOrphan(t *testing.T) {
	t.Parallel()
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(400, "beta", "lost-and-found", id, "stopped"),
	}}
	r, database := newReconciler(t, pve)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.TaggedOrph != 1 || rep.Opened != 1 {
		t.Errorf("TaggedOrph=%d Opened=%d, want 1/1", rep.TaggedOrph, rep.Opened)
	}
	var div db.VMDivergence
	if err := database.DB.First(&div).Error; err != nil {
		t.Fatalf("read divergence: %v", err)
	}
	if div.Type != reconciler.TypeExternalTaggedOrphan || div.NimbusID != id {
		t.Errorf("got type=%q nimbus_id=%q, want %q/%q", div.Type, div.NimbusID, reconciler.TypeExternalTaggedOrphan, id)
	}
}

// TestReconcile_AutoSyncsNodeAndStatus: a migrated-out-of-band VM
// updates vms.node and vms.status without opening a divergence row.
func TestReconcile_AutoSyncsNodeAndStatus(t *testing.T) {
	t.Parallel()
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(200, "beta", "vm-200", id, "stopped"), // moved alpha → beta, also stopped
	}}
	r, database := newReconciler(t, pve)
	if err := database.Create(&db.VM{
		VMID: 200, Hostname: "vm-200", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.200",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Healthy != 1 || rep.AutoSynced != 1 {
		t.Errorf("Healthy=%d AutoSynced=%d, want 1/1", rep.Healthy, rep.AutoSynced)
	}
	var got db.VM
	if err := database.DB.First(&got, "vmid = ?", 200).Error; err != nil {
		t.Fatalf("read vm: %v", err)
	}
	if got.Node != "beta" || got.Status != "stopped" {
		t.Errorf("after auto-sync: node=%q status=%q, want beta/stopped", got.Node, got.Status)
	}
}

// TestReconcile_DedupesOpenDivergence: running the same cycle twice
// against the same divergence updates one row instead of inserting a
// second.
func TestReconcile_DedupesOpenDivergence(t *testing.T) {
	t.Parallel()
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		{VMID: 300, Node: "alpha", Name: "operator-built", Status: "running"},
	}}
	r, database := newReconciler(t, pve)

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}
	rep2, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	if rep2.Opened != 0 || rep2.Refreshed != 1 {
		t.Errorf("second cycle: Opened=%d Refreshed=%d, want 0/1", rep2.Opened, rep2.Refreshed)
	}
	var n int64
	if err := database.DB.Model(&db.VMDivergence{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("divergence rows = %d, want 1 (dedupe)", n)
	}
}

// TestReconcile_AutoClosesWhenCauseGone: a divergence whose underlying
// cause vanishes (e.g. an external-tagged VM got imported into the DB)
// auto-closes on the next cycle.
func TestReconcile_AutoClosesWhenCauseGone(t *testing.T) {
	t.Parallel()
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	// Cycle 1: PVE has a tagged-orphan, no DB row.
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(400, "beta", "to-be-imported", id, "running"),
	}}
	r, database := newReconciler(t, pve)
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}

	// Cycle 2: admin imported it — DB row exists now.
	if err := database.Create(&db.VM{
		VMID: 400, Hostname: "to-be-imported", Node: "beta", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.400",
	}).Error; err != nil {
		t.Fatalf("import vm: %v", err)
	}
	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	if rep.Closed < 1 {
		t.Errorf("Closed = %d, want >= 1", rep.Closed)
	}

	var open int64
	if err := database.DB.Model(&db.VMDivergence{}).Where("resolved_at IS NULL").Count(&open).Error; err != nil {
		t.Fatalf("count open: %v", err)
	}
	if open != 0 {
		t.Errorf("open divergence count = %d, want 0", open)
	}
}

// TestReconcile_OpenedDivergenceEmitsAudit: opening a new orphan
// divergence row fires one divergence.opened audit event so the
// Infrastructure → Audit page surfaces the observation.
func TestReconcile_OpenedDivergenceEmitsAudit(t *testing.T) {
	t.Parallel()
	const healthyID = "0193ffff-eeee-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(999, "alpha", "other", healthyID, "running"),
	}}
	r, database := newReconciler(t, pve)
	audit := &fakeAudit{}
	r.SetAudit(audit)

	if err := database.Create(&db.VM{
		VMID: 999, Hostname: "other", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: healthyID,
		IP: "10.0.0.999",
	}).Error; err != nil {
		t.Fatalf("seed healthy: %v", err)
	}
	if err := database.Create(&db.VM{
		VMID: 200, Hostname: "gone", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running",
		NimbusID: "0193abcd-4321-7000-89ab-cdef01234567",
		IP:       "10.0.0.200",
	}).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	audit.mu.Lock()
	defer audit.mu.Unlock()
	gotOpened := 0
	for _, evt := range audit.events {
		if evt.Action == "divergence.opened" {
			gotOpened++
		}
	}
	if gotOpened != 1 {
		t.Errorf("divergence.opened audit events = %d, want 1", gotOpened)
	}
}

// TestReconcile_RefusesEmptySnapshot: a zero-VM snapshot returns the
// guard error so we don't mark every DB row orphaned on a Proxmox API
// hiccup.
func TestReconcile_RefusesEmptySnapshot(t *testing.T) {
	t.Parallel()
	pve := &fakePVE{details: nil}
	r, _ := newReconciler(t, pve)

	_, err := r.Reconcile(context.Background())
	if err == nil || err != reconciler.ErrEmptyClusterSnapshot {
		t.Errorf("err = %v, want ErrEmptyClusterSnapshot", err)
	}
}

// TestReconcile_VMIDMismatch: same nimbus_id at a different VMID than
// the DB row holds opens a vmid-mismatch divergence rather than treating
// the row as orphaned.
func TestReconcile_VMIDMismatch(t *testing.T) {
	t.Parallel()
	const id = "0193abcd-4321-7000-89ab-cdef01234567"
	pve := &fakePVE{details: []proxmox.ClusterVMDetail{
		withMarker(250, "alpha", "vm-recycled", id, "running"),
	}}
	r, database := newReconciler(t, pve)
	if err := database.Create(&db.VM{
		VMID: 200, Hostname: "vm-was-200", Node: "alpha", Tier: "small",
		OSTemplate: "ubuntu-22.04", Status: "running", NimbusID: id,
		IP: "10.0.0.200",
	}).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.VMIDMismatch != 1 || rep.Opened != 1 {
		t.Errorf("VMIDMismatch=%d Opened=%d, want 1/1", rep.VMIDMismatch, rep.Opened)
	}
	if rep.Orphaned != 0 {
		t.Errorf("Orphaned = %d, want 0 (must classify as vmid-mismatch, not orphan)", rep.Orphaned)
	}
}
