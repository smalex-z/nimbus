package netscan_test

import (
	"context"
	"net"
	"sort"
	"testing"

	"nimbus/internal/ippool"
	"nimbus/internal/netscan"
)

// fakePool implements netscan.Pool with in-memory state. Mirrors the slice
// of *ippool.Pool the reconciler actually depends on — we don't need a
// SQLite DB for these tests because the reconciler logic is pure
// state-transition.
type fakePool struct {
	rows map[string]*ippool.IPAllocation
}

func newFakePool(ips ...string) *fakePool {
	rows := map[string]*ippool.IPAllocation{}
	for _, ip := range ips {
		rows[ip] = &ippool.IPAllocation{IP: ip, Status: ippool.StatusFree}
	}
	return &fakePool{rows: rows}
}

func (p *fakePool) List(_ context.Context) ([]ippool.IPAllocation, error) {
	out := make([]ippool.IPAllocation, 0, len(p.rows))
	for _, r := range p.rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, nil
}

func (p *fakePool) ListExternal(_ context.Context) ([]ippool.IPAllocation, error) {
	out := make([]ippool.IPAllocation, 0)
	for _, r := range p.rows {
		if r.Status == ippool.StatusAllocated && r.Source == ippool.SourceExternal {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (p *fakePool) MarkExternal(_ context.Context, ip string) error {
	r := p.rows[ip]
	if r == nil {
		return ippool.ErrNotFound
	}
	// Mirror the real pool's guard: never overwrite reserved or non-external
	// allocated rows. Silently no-op (matches production behavior — the
	// netscan caller doesn't get an error when Proxmox owns the IP).
	if r.Status != ippool.StatusFree &&
		(r.Status != ippool.StatusAllocated || r.Source != ippool.SourceExternal) {
		return nil
	}
	r.Status = ippool.StatusAllocated
	r.Source = ippool.SourceExternal
	r.MissedCycles = 0
	return nil
}

func (p *fakePool) TouchSeen(_ context.Context, ip string) error {
	if r := p.rows[ip]; r != nil {
		r.MissedCycles = 0
	}
	return nil
}

func (p *fakePool) IncrementMissedCycles(_ context.Context, ip string) (int, error) {
	r := p.rows[ip]
	if r == nil {
		return 0, ippool.ErrNotFound
	}
	r.MissedCycles++
	return r.MissedCycles, nil
}

func (p *fakePool) Release(_ context.Context, ip string) error {
	if r := p.rows[ip]; r != nil {
		r.Status = ippool.StatusFree
		r.Source = ""
		r.MissedCycles = 0
	}
	return nil
}

// fixedScanner returns a fixed set of "in-use" IPs regardless of candidates.
// Lets tests drive the reconciler through specific scan outcomes.
type fixedScanner struct{ inUse []string }

func (s fixedScanner) Scan(_ context.Context, _ []net.IP) ([]net.IP, error) {
	out := make([]net.IP, 0, len(s.inUse))
	for _, ip := range s.inUse {
		if parsed := net.ParseIP(ip); parsed != nil {
			out = append(out, parsed)
		}
	}
	return out, nil
}

func TestReconciler_MarksFreshHits(t *testing.T) {
	t.Parallel()
	pool := newFakePool("10.0.0.1", "10.0.0.2", "10.0.0.3")
	r := netscan.NewReconciler(fixedScanner{inUse: []string{"10.0.0.1", "10.0.0.3"}}, pool)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Marked) != 2 {
		t.Errorf("Marked = %d %v, want 2", len(rep.Marked), rep.Marked)
	}
	if pool.rows["10.0.0.1"].Status != ippool.StatusAllocated || pool.rows["10.0.0.1"].Source != ippool.SourceExternal {
		t.Errorf("10.0.0.1 = %+v, want allocated+external", pool.rows["10.0.0.1"])
	}
	if pool.rows["10.0.0.2"].Status != ippool.StatusFree {
		t.Errorf("10.0.0.2 = %+v, want free (no hit)", pool.rows["10.0.0.2"])
	}
}

func TestReconciler_TouchesPriorExternalsThatStillRespond(t *testing.T) {
	t.Parallel()
	pool := newFakePool("10.0.0.1")
	pool.rows["10.0.0.1"].Status = ippool.StatusAllocated
	pool.rows["10.0.0.1"].Source = ippool.SourceExternal
	pool.rows["10.0.0.1"].MissedCycles = 1 // simulate one prior miss

	r := netscan.NewReconciler(fixedScanner{inUse: []string{"10.0.0.1"}}, pool)
	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Touched != 1 {
		t.Errorf("Touched = %d, want 1", rep.Touched)
	}
	if pool.rows["10.0.0.1"].MissedCycles != 0 {
		t.Errorf("MissedCycles = %d, want reset to 0", pool.rows["10.0.0.1"].MissedCycles)
	}
}

func TestReconciler_BumpsMissedCycles_ReleasesAfterThreshold(t *testing.T) {
	t.Parallel()
	pool := newFakePool("10.0.0.1")
	pool.rows["10.0.0.1"].Status = ippool.StatusAllocated
	pool.rows["10.0.0.1"].Source = ippool.SourceExternal

	// Threshold 2 so two missed cycles release. Scanner returns nothing.
	r := netscan.NewReconciler(fixedScanner{}, pool, netscan.WithMissThreshold(2))

	// Cycle 1: bump to 1 (below threshold). Not released yet.
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if pool.rows["10.0.0.1"].Status != ippool.StatusAllocated {
		t.Errorf("after cycle 1: status = %s, want allocated", pool.rows["10.0.0.1"].Status)
	}
	if pool.rows["10.0.0.1"].MissedCycles != 1 {
		t.Errorf("after cycle 1: MissedCycles = %d, want 1", pool.rows["10.0.0.1"].MissedCycles)
	}

	// Cycle 2: bump to 2, hits threshold, released.
	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if pool.rows["10.0.0.1"].Status != ippool.StatusFree {
		t.Errorf("after cycle 2: status = %s, want free", pool.rows["10.0.0.1"].Status)
	}
	if len(rep.Freed) != 1 || rep.Freed[0] != "10.0.0.1" {
		t.Errorf("Freed = %v, want [10.0.0.1]", rep.Freed)
	}
}

// Reserved and locally-allocated rows are owned by the Proxmox/provision
// side. The netscan reconciler must never claim or release them, even if
// they happen to respond to a probe.
func TestReconciler_DoesNotClaimReservedOrLocallyAllocatedRows(t *testing.T) {
	t.Parallel()
	pool := newFakePool("10.0.0.1", "10.0.0.2", "10.0.0.3")
	pool.rows["10.0.0.1"].Status = ippool.StatusReserved
	pool.rows["10.0.0.2"].Status = ippool.StatusAllocated
	pool.rows["10.0.0.2"].Source = ippool.SourceLocal
	// 10.0.0.3 stays free → eligible candidate.

	// Scanner reports ALL three as in-use. Only 10.0.0.3 should be marked.
	r := netscan.NewReconciler(fixedScanner{inUse: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}}, pool)

	rep, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Marked) != 1 || rep.Marked[0] != "10.0.0.3" {
		t.Errorf("Marked = %v, want [10.0.0.3]", rep.Marked)
	}
	if pool.rows["10.0.0.1"].Status != ippool.StatusReserved {
		t.Errorf("reserved row clobbered: %+v", pool.rows["10.0.0.1"])
	}
	if pool.rows["10.0.0.2"].Source != ippool.SourceLocal {
		t.Errorf("local row clobbered: %+v", pool.rows["10.0.0.2"])
	}
}
