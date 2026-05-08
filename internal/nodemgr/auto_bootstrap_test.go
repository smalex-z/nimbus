package nodemgr_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nimbus/internal/bootstrap"
	"nimbus/internal/db"
	"nimbus/internal/nodemgr"
	"nimbus/internal/proxmox"
)

// fakeBootstrapper records every Bootstrap call so tests can assert
// that auto-bootstrap fired for the expected nodes (and only those).
type fakeBootstrapper struct {
	mu    sync.Mutex
	calls []bootstrap.Request
	// done is closed every time a Bootstrap call returns. Tests use it
	// to await the goroutine that runReconcile fires off.
	done chan struct{}
	// hold blocks Bootstrap from returning until the test signals. Used
	// by the in-flight-guard test so the second Reconcile observes a
	// live in-flight slot rather than a returned-already one.
	hold chan struct{}
}

func newFakeBootstrapper() *fakeBootstrapper {
	return &fakeBootstrapper{done: make(chan struct{}, 8)}
}

func (f *fakeBootstrapper) Bootstrap(ctx context.Context, req bootstrap.Request) (*bootstrap.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	f.done <- struct{}{}
	return &bootstrap.Result{}, nil
}

func (f *fakeBootstrapper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newAutoBootstrapTestService builds a service with NodeTemplate
// migrated so the auto-bootstrap path can read template counts.
func newAutoBootstrapTestService(t *testing.T) (*nodemgr.Service, *db.DB, *fakePVE) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "nimbus.db")
	database, err := db.New(dbPath, &db.User{}, &db.VM{}, &db.Node{}, &db.NodeTemplate{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	fake := &fakePVE{}
	svc := nodemgr.New(database.DB, fake, nodemgr.Config{
		PerVMMigrateTimeout: 5 * time.Second,
		TaskPollInterval:    10 * time.Millisecond,
		VacateMissThreshold: 3,
	})
	return svc, database, fake
}

// TestReconcile_AutoBootstrapFiresForNodeMissingTemplates covers the
// "new node joins the cluster, has zero templates" path. Reconcile
// should kick off Bootstrap exactly once for that node.
func TestReconcile_AutoBootstrapFiresForNodeMissingTemplates(t *testing.T) {
	t.Parallel()

	svc, _, fake := newAutoBootstrapTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	bs := newFakeBootstrapper()
	svc.SetTemplateBootstrapper(bs)

	ctx := context.Background()
	if _, _, err := svc.Reconcile(ctx, time.Minute); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	select {
	case <-bs.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Bootstrap was never called")
	}
	if got := bs.callCount(); got != 1 {
		t.Errorf("Bootstrap call count = %d, want 1", got)
	}
	if len(bs.calls[0].Nodes) != 1 || bs.calls[0].Nodes[0] != "alpha" {
		t.Errorf("Bootstrap req nodes = %v, want [alpha]", bs.calls[0].Nodes)
	}
}

// TestReconcile_NoBootstrapWhenAllTemplatesPresent covers the steady
// state — no missing templates, no fan-out.
func TestReconcile_NoBootstrapWhenAllTemplatesPresent(t *testing.T) {
	t.Parallel()

	svc, database, fake := newAutoBootstrapTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	// Seed all catalog templates — Reconcile should see count == len.
	for i, e := range bootstrap.Catalog {
		if err := database.WithContext(context.Background()).Create(&db.NodeTemplate{
			Node: "alpha", OS: e.OS, VMID: 9000 + i,
		}).Error; err != nil {
			t.Fatalf("seed template %s: %v", e.OS, err)
		}
	}
	bs := newFakeBootstrapper()
	svc.SetTemplateBootstrapper(bs)

	if _, _, err := svc.Reconcile(context.Background(), time.Minute); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Give the (would-be) goroutine a beat — it shouldn't fire.
	time.Sleep(100 * time.Millisecond)
	if got := bs.callCount(); got != 0 {
		t.Errorf("Bootstrap fired %d times, want 0 (all templates present)", got)
	}
}

// TestRemove_DestroysTemplatesBeforeDeleteNode covers the cleanup hook:
// every db.NodeTemplate row for the removed node maps to a DestroyVM
// call, and the rows are cleared.
func TestRemove_DestroysTemplatesBeforeDeleteNode(t *testing.T) {
	t.Parallel()

	svc, database, fake := newAutoBootstrapTestService(t)
	fake.nodes = []proxmox.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	ctx := context.Background()
	if _, err := svc.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Seed all 4 templates onto alpha.
	for i, e := range bootstrap.Catalog {
		if err := database.WithContext(ctx).Create(&db.NodeTemplate{
			Node: "alpha", OS: e.OS, VMID: 9000 + i,
		}).Error; err != nil {
			t.Fatalf("seed template %s: %v", e.OS, err)
		}
	}
	// Force the node into "drained" so Remove's gate passes.
	if err := database.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", "alpha").
		Update("lock_state", "drained").Error; err != nil {
		t.Fatalf("force drained: %v", err)
	}

	if err := svc.Remove(ctx, "alpha"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if got := len(fake.destroys); got != len(bootstrap.Catalog) {
		t.Errorf("DestroyVM call count = %d, want %d", got, len(bootstrap.Catalog))
	}
	for _, c := range fake.destroys {
		if c.Node != "alpha" {
			t.Errorf("DestroyVM node = %q, want alpha", c.Node)
		}
	}
	var remaining int64
	_ = database.WithContext(ctx).Model(&db.NodeTemplate{}).
		Where("node = ?", "alpha").Count(&remaining).Error
	if remaining != 0 {
		t.Errorf("node_templates rows for alpha = %d, want 0", remaining)
	}
}
