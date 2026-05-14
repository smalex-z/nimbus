package bootstrap_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// TestSweep_DryRunReportsDuplicatesWithoutDestroy verifies that a dry-run
// classifies redundant templates but never calls DestroyVM. Mirrors the
// state of a leaked node (multiple baked templates for the same OS, an
// unbaked sibling, and a stopped failed-bake VM).
func TestSweep_DryRunReportsDuplicatesWithoutDestroy(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "neander", Status: "online"}}, nil
	}
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		return []proxmox.VMStatus{
			// Baked siblings for ubuntu-22.04 (4 of them — the wild case).
			{VMID: 9006, Name: "ubuntu-22.04-template", Template: 1},
			{VMID: 9017, Name: "ubuntu-22.04-template", Template: 1},
			{VMID: 9034, Name: "ubuntu-22.04-template", Template: 1},
			// Unbaked sibling — has same name but no NimbusBakedTag.
			{VMID: 9042, Name: "ubuntu-22.04-template", Template: 1},
			// Stopped failed-bake VM (not yet converted).
			{VMID: 9099, Name: "ubuntu-22.04-template", Template: 0, Status: "stopped"},
			// User VM in a different VMID range — must NOT be touched.
			{VMID: 101, Name: "my-vm", Template: 0, Status: "running"},
			// Unrelated template with non-catalog name — must NOT be touched.
			{VMID: 9500, Name: "custom-image", Template: 1},
		}, nil
	}
	bakedVMIDs := map[int]bool{9006: true, 9017: true, 9034: true}
	px.templateExists = func(_ context.Context, _ string, vmid int) (bool, error) {
		return bakedVMIDs[vmid], nil
	}

	var destroyCalls []int
	var mu sync.Mutex
	px.destroyVM = func(_ context.Context, _ string, vmid int) (string, error) {
		mu.Lock()
		destroyCalls = append(destroyCalls, vmid)
		mu.Unlock()
		return "UPID:destroy", nil
	}

	svc, _ := newSvc(t, px)

	res, err := svc.SweepTemplates(context.Background(), true)
	if err != nil {
		t.Fatalf("SweepTemplates: %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	if len(destroyCalls) != 0 {
		t.Errorf("DestroyVM called %d times in dry-run, want 0 (vmids: %v)", len(destroyCalls), destroyCalls)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Node != "neander" {
		t.Fatalf("nodes = %+v, want one entry for 'neander'", res.Nodes)
	}

	removed := res.Nodes[0].Removed
	if got := len(removed); got != 4 {
		t.Fatalf("removed count = %d, want 4 (2 duplicate baked, 1 unbaked, 1 failed bake)", got)
	}
	gotVMIDs := make([]int, 0, len(removed))
	gotReasons := map[int]string{}
	for _, r := range removed {
		gotVMIDs = append(gotVMIDs, r.VMID)
		gotReasons[r.VMID] = r.Reason
	}
	sort.Ints(gotVMIDs)
	wantVMIDs := []int{9017, 9034, 9042, 9099}
	if !equalIntSlices(gotVMIDs, wantVMIDs) {
		t.Errorf("removed vmids = %v, want %v", gotVMIDs, wantVMIDs)
	}

	// Lowest baked vmid (9006) should be the keeper.
	if got := res.Nodes[0].Kept["ubuntu-22.04"]; got != 9006 {
		t.Errorf("kept[ubuntu-22.04] = %d, want 9006", got)
	}

	// Reasons should match expectations.
	wantReasons := map[int]string{
		9017: "duplicate",
		9034: "duplicate",
		9042: "unbaked_with_baked_sibling",
		9099: "failed_bake_leftover",
	}
	for vmid, wantReason := range wantReasons {
		if got := gotReasons[vmid]; got != wantReason {
			t.Errorf("reason for vmid=%d = %q, want %q", vmid, got, wantReason)
		}
	}
}

// TestSweep_DestroysOnLiveRunAndUpdatesDBRow runs a non-dry sweep and
// asserts both DestroyVM and the node_templates DB pointer reflect the
// kept template.
func TestSweep_DestroysOnLiveRunAndUpdatesDBRow(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		return []proxmox.VMStatus{
			{VMID: 9005, Name: "ubuntu-24.04-template", Template: 1},
			{VMID: 9009, Name: "ubuntu-24.04-template", Template: 1},
		}, nil
	}
	px.templateExists = func(_ context.Context, _ string, vmid int) (bool, error) {
		// Both baked.
		return vmid == 9005 || vmid == 9009, nil
	}

	var destroyed []int
	var mu sync.Mutex
	px.destroyVM = func(_ context.Context, _ string, vmid int) (string, error) {
		mu.Lock()
		destroyed = append(destroyed, vmid)
		mu.Unlock()
		return "UPID:destroy", nil
	}

	svc, database := newSvc(t, px)

	// Pre-seed a stale DB row pointing at a VMID that no longer exists.
	// The sweep should fall back to lowest-VMID baked (9005) and rewrite
	// the row.
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9999}).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	res, err := svc.SweepTemplates(context.Background(), false)
	if err != nil {
		t.Fatalf("SweepTemplates: %v", err)
	}
	if res.DryRun {
		t.Errorf("DryRun = true on live run")
	}
	if got := len(destroyed); got != 1 || destroyed[0] != 9009 {
		t.Errorf("destroyed = %v, want [9009]", destroyed)
	}

	// DB row should now point at 9005 (the kept baked template).
	var row db.NodeTemplate
	if err := database.DB.Where("node = ? AND os = ?", "alpha", "ubuntu-24.04").First(&row).Error; err != nil {
		t.Fatalf("read db row: %v", err)
	}
	if row.VMID != 9005 {
		t.Errorf("db row vmid = %d, want 9005", row.VMID)
	}
}

// TestSweep_LeavesOSWithoutBakedTemplateAlone covers the conservative
// invariant: an OS that has only an unbaked template (or only a stopped
// failed-bake VM) is left untouched so the rebuild-banner path can step
// in. Without this guard the sweeper would leave the node with zero
// templates for that OS until the next reconcile.
func TestSweep_LeavesOSWithoutBakedTemplateAlone(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		return []proxmox.VMStatus{
			// Only an unbaked template — no baked sibling.
			{VMID: 9007, Name: "debian-12-template", Template: 1},
			// Only a stopped failed-bake — no baked template either.
			{VMID: 9008, Name: "debian-13-template", Template: 0, Status: "stopped"},
		}, nil
	}
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) {
		return false, nil
	}

	var destroyed []int
	px.destroyVM = func(_ context.Context, _ string, vmid int) (string, error) {
		destroyed = append(destroyed, vmid)
		return "UPID:destroy", nil
	}

	svc, _ := newSvc(t, px)

	res, err := svc.SweepTemplates(context.Background(), false)
	if err != nil {
		t.Fatalf("SweepTemplates: %v", err)
	}
	if len(destroyed) != 0 {
		t.Errorf("destroyed = %v, want []", destroyed)
	}
	if got := len(res.Nodes[0].Removed); got != 0 {
		t.Errorf("removed = %d, want 0", got)
	}
}

// TestSweep_DestroysTemplateForCatalogOrphanOS covers a fresh failure
// mode: an OS used to be in the catalog (e.g. debian-11) and was later
// retired in favor of a newer release. The old templates are still on
// disk on every PVE node and their node_templates rows still exist —
// `nimbus bootstrap --force` won't touch them because the catalog
// iteration doesn't include the retired OS. Sweep must catch them via
// the stale_os reason: destroy the PVE template AND drop the node_
// templates row so the bootstrap-status banner stops counting them.
func TestSweep_DestroysTemplateForCatalogOrphanOS(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		return []proxmox.VMStatus{
			// In-catalog: should not be touched.
			{VMID: 9000, Name: "ubuntu-24.04-template", Template: 1},
			// Catalog orphan: destroy.
			{VMID: 9099, Name: "debian-11-template", Template: 1},
		}, nil
	}
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) {
		return true, nil
	}
	var destroyed []int
	px.destroyVM = func(_ context.Context, _ string, vmid int) (string, error) {
		destroyed = append(destroyed, vmid)
		return "UPID:destroy", nil
	}

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9000}).Error; err != nil {
		t.Fatalf("seed ubuntu row: %v", err)
	}
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "debian-11", VMID: 9099}).Error; err != nil {
		t.Fatalf("seed stale-os row: %v", err)
	}

	res, err := svc.SweepTemplates(context.Background(), false)
	if err != nil {
		t.Fatalf("SweepTemplates: %v", err)
	}
	if !equalIntSlices(destroyed, []int{9099}) {
		t.Errorf("destroyed = %v, want [9099]", destroyed)
	}
	if len(res.Nodes) != 1 || len(res.Nodes[0].Removed) != 1 || res.Nodes[0].Removed[0].Reason != "stale_os" {
		t.Errorf("Removed = %+v, want one stale_os entry", res.Nodes[0].Removed)
	}
	// The catalog-orphan row should be gone; the in-catalog row stays.
	var rows []db.NodeTemplate
	if err := database.DB.Order("os").Find(&rows).Error; err != nil {
		t.Fatalf("re-read rows: %v", err)
	}
	if len(rows) != 1 || rows[0].OS != "ubuntu-24.04" {
		t.Errorf("rows after sweep = %+v, want only the ubuntu-24.04 row", rows)
	}
}

// TestSweep_DropsOrphanRowsForAbsentNodes asserts that node_templates
// rows for nodes that have left the cluster (PVE-side removal not
// routed through Nimbus's RemoveNode) get dropped at the top of the
// sweep so the bootstrap-status banner doesn't keep counting them.
func TestSweep_DropsOrphanRowsForAbsentNodes(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		// alpha is the only live node; bravo is the removed one.
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) { return nil, nil }

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9000}).Error; err != nil {
		t.Fatalf("seed alpha row: %v", err)
	}
	if err := database.DB.Create(&db.NodeTemplate{Node: "bravo", OS: "ubuntu-24.04", VMID: 9100}).Error; err != nil {
		t.Fatalf("seed bravo row: %v", err)
	}

	if _, err := svc.SweepTemplates(context.Background(), false); err != nil {
		t.Fatalf("SweepTemplates: %v", err)
	}
	var rows []db.NodeTemplate
	if err := database.DB.Order("node").Find(&rows).Error; err != nil {
		t.Fatalf("re-read rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Node != "alpha" {
		t.Errorf("rows after sweep = %+v, want only the alpha row", rows)
	}
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
