package ippool_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/proxmox"
)

// fakeLister is a minimal ClusterIPLister where each call returns whatever the
// list function field produces. Atomic counter exposes how many times it was
// invoked so cache-reuse tests can assert the call count.
type fakeLister struct {
	list  func(context.Context) ([]proxmox.ClusterIP, error)
	calls atomic.Int32
}

func (f *fakeLister) ListClusterIPs(ctx context.Context) ([]proxmox.ClusterIP, error) {
	f.calls.Add(1)
	return f.list(ctx)
}

func newSeedPool(t *testing.T, start, end string) *ippool.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, ippool.Model())
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	p := ippool.New(database.DB)
	if err := p.Seed(context.Background(), start, end); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return p
}

func staticLister(out []proxmox.ClusterIP, err error) *fakeLister {
	return &fakeLister{list: func(context.Context) ([]proxmox.ClusterIP, error) {
		return out, err
	}}
}

func TestReconciler_VerifyFree(t *testing.T) {
	t.Parallel()

	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	lister := staticLister([]proxmox.ClusterIP{
		{IP: "10.0.0.3", VMID: 200, Node: "alpha"},
	}, nil)
	r := ippool.NewReconciler(p, lister, ippool.WithCacheTTL(50*time.Millisecond))

	t.Run("returns true for unclaimed IP", func(t *testing.T) {
		free, holder, err := r.VerifyFree(context.Background(), "10.0.0.1")
		if err != nil || !free || holder != nil {
			t.Errorf("VerifyFree(unclaimed) = (%v, %v, %v), want (true, nil, nil)", free, holder, err)
		}
	})

	t.Run("returns false with vmid for claimed IP", func(t *testing.T) {
		free, holder, err := r.VerifyFree(context.Background(), "10.0.0.3")
		if err != nil || free {
			t.Errorf("VerifyFree(claimed) free=%v err=%v", free, err)
		}
		if holder == nil || *holder != 200 {
			t.Errorf("holder = %v, want 200", holder)
		}
	})

	t.Run("error is unsafe", func(t *testing.T) {
		errLister := staticLister(nil, errors.New("network down"))
		errR := ippool.NewReconciler(p, errLister)
		free, holder, err := errR.VerifyFree(context.Background(), "10.0.0.1")
		if free {
			t.Errorf("free = true on lookup error, must be false (unsafe)")
		}
		if err == nil {
			t.Errorf("expected error to be surfaced")
		}
		if holder != nil {
			t.Errorf("holder = %v, want nil on error", holder)
		}
	})
}

// TestReconciler_VerifyFree_CacheReuse checks that two calls within cacheTTL
// trigger exactly one ListClusterIPs call.
func TestReconciler_VerifyFree_CacheReuse(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	lister := staticLister([]proxmox.ClusterIP{}, nil)
	r := ippool.NewReconciler(p, lister, ippool.WithCacheTTL(1*time.Second))

	for i := 0; i < 5; i++ {
		if _, _, err := r.VerifyFree(context.Background(), "10.0.0.1"); err != nil {
			t.Fatalf("VerifyFree #%d: %v", i, err)
		}
	}
	if got := lister.calls.Load(); got != 1 {
		t.Errorf("ListClusterIPs called %d times within cacheTTL, want 1", got)
	}
}

// TestReconciler_VerifyFree_RefreshAfterTTL checks that an expired cache is
// refetched on the next VerifyFree.
func TestReconciler_VerifyFree_RefreshAfterTTL(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	lister := staticLister([]proxmox.ClusterIP{}, nil)
	// Use injectable clock so we don't actually have to sleep.
	now := time.Unix(1_000_000_000, 0)
	clock := func() time.Time { return now }
	r := ippool.NewReconciler(p, lister,
		ippool.WithCacheTTL(5*time.Second),
		ippool.WithClock(clock),
	)

	_, _, _ = r.VerifyFree(context.Background(), "10.0.0.1")
	now = now.Add(10 * time.Second) // jump past cacheTTL
	_, _, _ = r.VerifyFree(context.Background(), "10.0.0.1")

	if got := lister.calls.Load(); got != 2 {
		t.Errorf("ListClusterIPs called %d times across TTL boundary, want 2", got)
	}
}

func TestReconcile_AdoptsForeignAllocation(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	lister := staticLister([]proxmox.ClusterIP{
		{IP: "10.0.0.2", VMID: 200, Node: "alpha", Hostname: "foreign-vm", Source: "ipconfig0"},
	}, nil)
	r := ippool.NewReconciler(p, lister)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Adopted) != 1 || rep.Adopted[0].IP != "10.0.0.2" || rep.Adopted[0].VMID != 200 {
		t.Errorf("Adopted = %+v, want exactly one row for 10.0.0.2/200", rep.Adopted)
	}

	row, _ := p.GetByIP(context.Background(), "10.0.0.2")
	if row.Status != ippool.StatusAllocated {
		t.Errorf("status = %s after adopt, want allocated", row.Status)
	}
	if row.VMID == nil || *row.VMID != 200 {
		t.Errorf("vmid = %v, want 200", row.VMID)
	}
	if row.Source != ippool.SourceAdopted {
		t.Errorf("source = %s, want adopted", row.Source)
	}
}

// TestReconcile_NoOpsForMatch ensures the Proxmox-yes / DB-allocated-same-vmid
// branch updates last_seen_at and resets missed_cycles, but does not appear in
// the report's Adopted/Conflicts/Freed lists.
func TestReconcile_NoOpsForMatch(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	// Allocate 10.0.0.1 to vmid=200 locally.
	_, _ = p.Reserve(ctx, "host")
	_ = p.MarkAllocated(ctx, "10.0.0.1", 200)

	lister := staticLister([]proxmox.ClusterIP{
		{IP: "10.0.0.1", VMID: 200, Node: "alpha", Hostname: "host"},
	}, nil)
	r := ippool.NewReconciler(p, lister)

	rep, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Adopted) != 0 || len(rep.Conflicts) != 0 || len(rep.Freed) != 0 || len(rep.Vacated) != 0 {
		t.Errorf("expected only NoOps, got %+v", rep)
	}
	row, _ := p.GetByIP(ctx, "10.0.0.1")
	if row.LastSeenAt == nil {
		t.Errorf("expected last_seen_at to be touched")
	}
}

// TestReconcile_ConflictDoesNotAutoResolve covers the scenario where the local
// DB and Proxmox disagree on which VMID owns an IP. The reconciler should log
// + report the conflict, NOT mutate the row.
func TestReconcile_ConflictDoesNotAutoResolve(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	_, _ = p.Reserve(ctx, "host")
	_ = p.MarkAllocated(ctx, "10.0.0.1", 200)

	lister := staticLister([]proxmox.ClusterIP{
		{IP: "10.0.0.1", VMID: 999, Node: "bravo", Hostname: "race-winner"},
	}, nil)
	r := ippool.NewReconciler(p, lister)

	rep, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want 1 entry", rep.Conflicts)
	}
	if rep.Conflicts[0].LocalVMID != 200 || rep.Conflicts[0].ProxmoxVMID != 999 {
		t.Errorf("conflict = %+v", rep.Conflicts[0])
	}

	// Local row must be untouched.
	row, _ := p.GetByIP(ctx, "10.0.0.1")
	if row.VMID == nil || *row.VMID != 200 {
		t.Errorf("local vmid changed to %v on conflict, must stay 200", row.VMID)
	}
}

// TestReconcile_StaleReservationReleased covers the (Proxmox=no, DB=reserved
// older than staleAfter) branch. Uses real time because pool.Reserve always
// stamps reserved_at via time.Now(); injecting a fake clock for the reconciler
// while Reserve uses real time would mix clock domains.
func TestReconcile_StaleReservationReleased(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	_, _ = p.Reserve(ctx, "crashed-host")
	lister := staticLister(nil, nil)

	t.Run("fresh reservation not freed", func(t *testing.T) {
		r := ippool.NewReconciler(p, lister, ippool.WithStaleAfter(1*time.Hour))
		rep, err := r.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(rep.Freed) != 0 {
			t.Errorf("fresh reservation freed: %+v", rep.Freed)
		}
	})

	t.Run("stale reservation freed", func(t *testing.T) {
		// staleAfter=1ns means any reservation older than a nanosecond is
		// stale; the Reserve above happened well over a nanosecond ago.
		r := ippool.NewReconciler(p, lister, ippool.WithStaleAfter(1*time.Nanosecond))
		rep, err := r.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(rep.Freed) != 1 {
			t.Errorf("Freed = %+v, want 1 stale entry", rep.Freed)
		}
		row, _ := p.GetByIP(ctx, "10.0.0.1")
		if row.Status != ippool.StatusFree {
			t.Errorf("status = %s, want free", row.Status)
		}
	})
}

// TestReconcile_VacateAfterNMisses asserts the missed_cycles counter behavior:
// allocated rows that vanish from Proxmox are vacated only after the
// configured number of consecutive missing reconciles.
func TestReconcile_VacateAfterNMisses(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	_, _ = p.Reserve(ctx, "host")
	_ = p.MarkAllocated(ctx, "10.0.0.1", 200)

	emptyLister := staticLister(nil, nil)
	r := ippool.NewReconciler(p, emptyLister, ippool.WithMissThreshold(3))

	t.Run("first two misses do not vacate", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			rep, err := r.Reconcile(ctx)
			if err != nil {
				t.Fatalf("Reconcile #%d: %v", i, err)
			}
			if len(rep.Vacated) != 0 {
				t.Errorf("Reconcile #%d vacated %v, must wait until threshold", i, rep.Vacated)
			}
			row, _ := p.GetByIP(ctx, "10.0.0.1")
			if row.Status != ippool.StatusAllocated {
				t.Errorf("Reconcile #%d status = %s, want still allocated", i, row.Status)
			}
			if row.MissedCycles != i {
				t.Errorf("Reconcile #%d missed_cycles = %d, want %d", i, row.MissedCycles, i)
			}
		}
	})

	t.Run("third miss vacates", func(t *testing.T) {
		rep, err := r.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(rep.Vacated) != 1 || rep.Vacated[0] != "10.0.0.1" {
			t.Errorf("Vacated = %v, want [10.0.0.1]", rep.Vacated)
		}
		row, _ := p.GetByIP(ctx, "10.0.0.1")
		if row.Status != ippool.StatusFree {
			t.Errorf("after vacate status = %s, want free", row.Status)
		}
	})
}

// TestReconcile_VMReappearsResetsCounter covers the case where a VM disappears
// briefly (e.g. mid-migration) and reappears: the missed_cycles counter must
// reset to 0 so the row is not vacated when the third "real" miss happens
// later.
func TestReconcile_VMReappearsResetsCounter(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	_, _ = p.Reserve(ctx, "host")
	_ = p.MarkAllocated(ctx, "10.0.0.1", 200)

	// Lister whose return value we can swap atomically between calls.
	var snapshot atomic.Pointer[[]proxmox.ClusterIP]
	empty := []proxmox.ClusterIP{}
	snapshot.Store(&empty)
	lister := &fakeLister{list: func(context.Context) ([]proxmox.ClusterIP, error) {
		s := snapshot.Load()
		return *s, nil
	}}
	r := ippool.NewReconciler(p, lister, ippool.WithMissThreshold(3))

	// First two misses.
	_, _ = r.Reconcile(ctx)
	_, _ = r.Reconcile(ctx)
	row, _ := p.GetByIP(ctx, "10.0.0.1")
	if row.MissedCycles != 2 {
		t.Fatalf("after 2 misses missed_cycles = %d, want 2", row.MissedCycles)
	}

	// VM reappears.
	seen := []proxmox.ClusterIP{{IP: "10.0.0.1", VMID: 200, Node: "alpha", Hostname: "host"}}
	snapshot.Store(&seen)
	if _, err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ = p.GetByIP(ctx, "10.0.0.1")
	if row.MissedCycles != 0 {
		t.Errorf("after VM reappears missed_cycles = %d, want reset to 0", row.MissedCycles)
	}

	// Two more misses must not vacate (counter restarted at 0).
	snapshot.Store(&empty)
	_, _ = r.Reconcile(ctx)
	rep, _ := r.Reconcile(ctx)
	if len(rep.Vacated) != 0 {
		t.Errorf("Vacated %v after only 2 misses post-reset", rep.Vacated)
	}
}

func TestReconcile_OutOfRangeProxmoxIPsAreIgnored(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	// 192.168.x.y is well outside the 10.0.0.x pool.
	lister := staticLister([]proxmox.ClusterIP{
		{IP: "192.168.99.99", VMID: 999, Node: "alpha", Hostname: "out-of-range"},
	}, nil)
	r := ippool.NewReconciler(p, lister)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Adopted) != 0 || len(rep.Conflicts) != 0 {
		t.Errorf("out-of-range Proxmox IPs must not affect Adopted/Conflicts: %+v", rep)
	}
}

// TestReconcile_UnreachableNodeSuppressesMissBump asserts the reachability
// guard: when the WithUnreachableVMIDs probe reports that a row’s vmid lives
// on a node we currently can’t reach, missing-from-snapshot is treated as
// "still alive, just out of sight" — missed_cycles stays put and the row
// never gets vacated even after threshold cycles.
func TestReconcile_UnreachableNodeSuppressesMissBump(t *testing.T) {
	t.Parallel()
	p := newSeedPool(t, "10.0.0.1", "10.0.0.5")
	ctx := context.Background()

	_, _ = p.Reserve(ctx, "host")
	_ = p.MarkAllocated(ctx, "10.0.0.1", 200)

	emptyLister := staticLister(nil, nil)
	probe := func(context.Context) map[int]bool { return map[int]bool{200: true} }
	r := ippool.NewReconciler(p, emptyLister,
		ippool.WithMissThreshold(2),
		ippool.WithUnreachableVMIDs(probe),
	)

	for i := 1; i <= 3; i++ {
		rep, err := r.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile #%d: %v", i, err)
		}
		if len(rep.Vacated) != 0 {
			t.Errorf("cycle %d vacated %v, must hold while host is unreachable", i, rep.Vacated)
		}
		row, _ := p.GetByIP(ctx, "10.0.0.1")
		if row.MissedCycles != 0 {
			t.Errorf("cycle %d missed_cycles = %d, want 0 (gate should suppress)", i, row.MissedCycles)
		}
		if row.Status != ippool.StatusAllocated {
			t.Errorf("cycle %d status = %s, want allocated", i, row.Status)
		}
	}
}
