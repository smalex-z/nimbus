// Reconciliation between the local IP pool and the Proxmox cluster's actual
// state. The Proxmox cluster (specifically each VM's cloud-init `ipconfig0`)
// is treated as the source of truth; the local SQLite cache is converged to
// match. This is what allows multiple Nimbus instances to share a Proxmox
// cluster without handing out the same IP to two different VMs.
//
// Two entry points:
//   - Reconcile(ctx)     — full diff + apply, run on startup, periodically, and on demand.
//   - VerifyFree(ctx, ip) — fast check used immediately after Reserve to catch
//     the live race between two Nimbus instances picking
//     the same lowest-free IP at the same moment.
//
// Both rely on a short-lived snapshot of the cluster's claimed IPs, served
// from a TTL cache so concurrent provisions don't hammer the Proxmox API.
package ippool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"nimbus/internal/proxmox"
)

// Default reconciler tunables. All overridable via Option.
const (
	defaultStaleAfter    = 10 * time.Minute
	defaultCacheTTL      = 5 * time.Second
	defaultMissThreshold = 3
)

// ClusterIPLister is the small interface the reconciler depends on. Defined
// here in the consumer package per the "accept interfaces" idiom — keeps the
// reconciler trivially testable without dragging in the full Proxmox client.
type ClusterIPLister interface {
	ListClusterIPs(ctx context.Context) ([]proxmox.ClusterIP, error)
}

// Reconciler diffs Proxmox cluster state against the local IP pool and applies
// targeted fixes. Safe for concurrent use.
type Reconciler struct {
	pool          *Pool
	px            ClusterIPLister
	staleAfter    time.Duration
	cacheTTL      time.Duration
	missThreshold int
	clock         func() time.Time

	mu       sync.Mutex
	cached   []proxmox.ClusterIP
	cachedAt time.Time
}

// Option configures a Reconciler at construction time.
type Option func(*Reconciler)

// WithStaleAfter overrides the reservation TTL. Reservations older than this
// are released by Reconcile on the assumption their provision crashed.
func WithStaleAfter(d time.Duration) Option {
	return func(r *Reconciler) {
		if d > 0 {
			r.staleAfter = d
		}
	}
}

// WithCacheTTL overrides how long a ListClusterIPs snapshot may be reused
// across VerifyFree calls. Lower = tighter race window, more API load.
func WithCacheTTL(d time.Duration) Option {
	return func(r *Reconciler) {
		if d > 0 {
			r.cacheTTL = d
		}
	}
}

// WithMissThreshold overrides how many consecutive reconcile cycles a row
// can be allocated-locally-but-missing-from-Proxmox before it is auto-vacated.
// Higher = more tolerant of brief migration disappearance.
func WithMissThreshold(n int) Option {
	return func(r *Reconciler) {
		if n > 0 {
			r.missThreshold = n
		}
	}
}

// WithClock injects a clock function. Test-only — production should leave the
// default (time.Now).
func WithClock(f func() time.Time) Option {
	return func(r *Reconciler) {
		if f != nil {
			r.clock = f
		}
	}
}

// NewReconciler constructs a Reconciler with the supplied options applied on
// top of sensible defaults.
func NewReconciler(pool *Pool, px ClusterIPLister, opts ...Option) *Reconciler {
	r := &Reconciler{
		pool:          pool,
		px:            px,
		staleAfter:    defaultStaleAfter,
		cacheTTL:      defaultCacheTTL,
		missThreshold: defaultMissThreshold,
		clock:         time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AdoptedRow describes one row that the reconciler upserted from a Proxmox
// observation, along with the prior local status (free or reserved).
type AdoptedRow struct {
	IP          string `json:"ip"`
	VMID        int    `json:"vmid"`
	Hostname    string `json:"hostname"`
	PriorStatus string `json:"prior_status"`
}

// ConflictRow describes a true cluster-state conflict: Proxmox shows two
// different VMs claiming the same IP, OR the local DB and Proxmox disagree on
// which VMID owns an IP. The reconciler does not auto-resolve these.
type ConflictRow struct {
	IP          string `json:"ip"`
	LocalVMID   int    `json:"local_vmid"`
	ProxmoxVMID int    `json:"proxmox_vmid"`
	Node        string `json:"node"`
}

// Report is the structured result of a Reconcile run, also the body of the
// /api/ips/reconcile endpoint.
type Report struct {
	Adopted    []AdoptedRow  `json:"adopted"`
	Conflicts  []ConflictRow `json:"conflicts"`
	Freed      []string      `json:"freed"`
	Vacated    []string      `json:"vacated"`
	NoOps      int           `json:"no_ops"`
	SnapshotAt time.Time     `json:"snapshot_at"`
}

// Refresh forces a fresh ListClusterIPs and updates the cache. Returns the
// number of IPs observed.
func (r *Reconciler) Refresh(ctx context.Context) (int, error) {
	snap, err := r.fetchAndCache(ctx)
	if err != nil {
		return 0, err
	}
	return len(snap), nil
}

// VerifyFree reports whether the supplied IP is unclaimed by any VM in the
// cluster. The cluster snapshot is reused for up to cacheTTL across calls so
// concurrent provisions don't multiply Proxmox API load.
//
// Return semantics:
//   - (true, nil, nil)            no VM claims the IP (safe to use)
//   - (false, &vmid, nil)         a VM with this VMID claims it (race or stale state)
//   - (false, nil, err)           lookup failed — treat as unsafe (release+retry)
func (r *Reconciler) VerifyFree(ctx context.Context, ip string) (bool, *int, error) {
	snap, err := r.snapshot(ctx, false)
	if err != nil {
		return false, nil, err
	}
	for i := range snap {
		if snap[i].IP == ip {
			vmid := snap[i].VMID
			return false, &vmid, nil
		}
	}
	return true, nil, nil
}

// Reconcile walks the union of (Proxmox-claimed IPs) ∪ (local pool rows) and
// applies the decision table from the design doc. Returns a Report describing
// what changed plus a multi-error if any per-row update failed (the rest of
// the run completes regardless).
func (r *Reconciler) Reconcile(ctx context.Context) (Report, error) {
	rep := Report{SnapshotAt: r.clock()}

	snap, err := r.snapshot(ctx, true)
	if err != nil {
		// snapshot returns an error only when ListClusterIPs returned no
		// usable data — there is nothing to reconcile against.
		return rep, fmt.Errorf("snapshot cluster: %w", err)
	}

	pxByIP := make(map[string]proxmox.ClusterIP, len(snap))
	for _, c := range snap {
		pxByIP[c.IP] = c
	}

	rows, err := r.pool.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list local rows: %w", err)
	}

	var errs []error
	for _, row := range rows {
		px, hasPx := pxByIP[row.IP]
		switch {
		case hasPx && row.Status == StatusAllocated:
			r.handleAllocatedSeen(ctx, row, px, &rep, &errs)
		case hasPx:
			r.handleAdopt(ctx, row, px, &rep, &errs)
		case row.Status == StatusReserved:
			r.handleReservedMissing(ctx, row, &rep, &errs)
		case row.Status == StatusAllocated:
			r.handleAllocatedMissing(ctx, row, &rep, &errs)
		default:
			rep.NoOps++
		}
	}

	// Stale reservations may also be caught by handleReservedMissing above,
	// but we run the explicit pass too in case there are reservations newer
	// than this reconcile but still older than staleAfter (rare — defense in
	// depth, costs one extra query).
	cutoff := r.clock().Add(-r.staleAfter)
	freed, err := r.pool.ReleaseStaleReservations(ctx, cutoff)
	if err != nil {
		errs = append(errs, fmt.Errorf("release stale reservations: %w", err))
	}
	for _, ip := range freed {
		// Avoid double-reporting if the same IP was freed via handleReservedMissing.
		if !contains(rep.Freed, ip) {
			rep.Freed = append(rep.Freed, ip)
		}
	}

	if len(errs) > 0 {
		return rep, errors.Join(errs...)
	}
	return rep, nil
}

// handleAllocatedSeen applies the (Proxmox=yes, DB=allocated) row.
// Same VMID → no-op + touch. Different VMID → conflict.
func (r *Reconciler) handleAllocatedSeen(ctx context.Context, row IPAllocation, px proxmox.ClusterIP, rep *Report, errs *[]error) {
	if row.VMID != nil && *row.VMID == px.VMID {
		if err := r.pool.TouchSeen(ctx, row.IP); err != nil {
			*errs = append(*errs, fmt.Errorf("touch %s: %w", row.IP, err))
			return
		}
		rep.NoOps++
		return
	}
	local := 0
	if row.VMID != nil {
		local = *row.VMID
	}
	rep.Conflicts = append(rep.Conflicts, ConflictRow{
		IP:          row.IP,
		LocalVMID:   local,
		ProxmoxVMID: px.VMID,
		Node:        px.Node,
	})
	log.Printf("ip-reconcile CONFLICT: %s held by local-vmid=%d, proxmox-vmid=%d on %s — leaving DB unchanged",
		row.IP, local, px.VMID, px.Node)
}

// handleAdopt applies the (Proxmox=yes, DB=free|reserved) row by upserting it
// to allocated bound to Proxmox's vmid+hostname.
func (r *Reconciler) handleAdopt(ctx context.Context, row IPAllocation, px proxmox.ClusterIP, rep *Report, errs *[]error) {
	if err := r.pool.AdoptAllocation(ctx, row.IP, px.VMID, px.Hostname); err != nil {
		*errs = append(*errs, fmt.Errorf("adopt %s: %w", row.IP, err))
		return
	}
	rep.Adopted = append(rep.Adopted, AdoptedRow{
		IP:          row.IP,
		VMID:        px.VMID,
		Hostname:    px.Hostname,
		PriorStatus: row.Status,
	})
}

// handleReservedMissing applies the (Proxmox=no, DB=reserved) row. Recent
// reservations are left alone (in-flight on this instance); stale ones are
// freed.
func (r *Reconciler) handleReservedMissing(ctx context.Context, row IPAllocation, rep *Report, errs *[]error) {
	if row.ReservedAt == nil {
		return
	}
	age := r.clock().Sub(*row.ReservedAt)
	if age <= r.staleAfter {
		return
	}
	if err := r.pool.Release(ctx, row.IP); err != nil {
		*errs = append(*errs, fmt.Errorf("release stale %s: %w", row.IP, err))
		return
	}
	rep.Freed = append(rep.Freed, row.IP)
}

// handleAllocatedMissing applies the (Proxmox=no, DB=allocated) row. Increments
// the missed-cycle counter; vacates the row only after missThreshold misses to
// tolerate a VM briefly disappearing during a Proxmox migration.
func (r *Reconciler) handleAllocatedMissing(ctx context.Context, row IPAllocation, rep *Report, errs *[]error) {
	post, err := r.pool.IncrementMissedCycles(ctx, row.IP)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("increment missed_cycles %s: %w", row.IP, err))
		return
	}
	if post < r.missThreshold {
		return
	}
	if err := r.pool.Release(ctx, row.IP); err != nil {
		*errs = append(*errs, fmt.Errorf("vacate %s: %w", row.IP, err))
		return
	}
	rep.Vacated = append(rep.Vacated, row.IP)
	log.Printf("ip-reconcile vacated %s after %d consecutive missed cycles", row.IP, post)
}

// snapshot returns the current cluster IP snapshot, refetching from Proxmox
// when the cache is older than cacheTTL or when force is true.
//
// On lookup error the cache is NOT updated and the prior cached snapshot is
// not returned — callers must treat the error as "unsafe to make a decision".
func (r *Reconciler) snapshot(ctx context.Context, force bool) ([]proxmox.ClusterIP, error) {
	r.mu.Lock()
	if !force && r.cached != nil && r.clock().Sub(r.cachedAt) < r.cacheTTL {
		snap := r.cached
		r.mu.Unlock()
		return snap, nil
	}
	r.mu.Unlock()

	return r.fetchAndCache(ctx)
}

func (r *Reconciler) fetchAndCache(ctx context.Context) ([]proxmox.ClusterIP, error) {
	snap, err := r.px.ListClusterIPs(ctx)
	if err != nil {
		// Partial result? Don't cache it — VerifyFree against partial data
		// could miss an IP held by an offline node and produce a duplicate.
		return nil, err
	}
	r.mu.Lock()
	r.cached = snap
	r.cachedAt = r.clock()
	r.mu.Unlock()
	return snap, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
