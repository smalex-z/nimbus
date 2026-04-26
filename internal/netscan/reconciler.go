package netscan

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"nimbus/internal/ippool"
)

// Verifier adapts a Scanner to the provision.IPVerifier interface so the
// IP picked at provision time gets a single-IP probe just before clone.
// This is the belt-and-suspenders check that catches LAN devices that
// appeared between netscan cycles.
//
// Probe latency is bounded by the scanner's TCPTimeout × len(TCPPorts) —
// ~800ms worst case with the defaults, faster on hits.
type Verifier struct {
	scanner Scanner
}

// NewVerifier wraps a Scanner as a provision.IPVerifier.
func NewVerifier(s Scanner) *Verifier {
	return &Verifier{scanner: s}
}

// VerifyFree returns (false, nil, nil) when the IP responds to the probe —
// matching the IPVerifier contract for "claimed but holder vmid unknown".
// (true, nil, nil) means the probe didn't see a host; provision can proceed.
// Errors propagate untouched.
func (v *Verifier) VerifyFree(ctx context.Context, ip string) (bool, *int, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		// Defer to the next verifier in the chain (or accept) — we can't
		// say anything useful about an unparseable IP.
		return true, nil, nil
	}
	hits, err := v.scanner.Scan(ctx, []net.IP{parsed})
	if err != nil {
		return false, nil, err
	}
	if len(hits) > 0 {
		return false, nil, nil
	}
	return true, nil, nil
}

// Pool is the small slice of *ippool.Pool the netscan reconciler depends on.
// Defined here in the consumer package per the "accept interfaces" idiom —
// makes the reconciler trivially testable without dragging in GORM.
type Pool interface {
	List(ctx context.Context) ([]ippool.IPAllocation, error)
	ListExternal(ctx context.Context) ([]ippool.IPAllocation, error)
	MarkExternal(ctx context.Context, ip string) error
	TouchSeen(ctx context.Context, ip string) error
	IncrementMissedCycles(ctx context.Context, ip string) (int, error)
	Release(ctx context.Context, ip string) error
}

// Reconciler ties a Scanner to an IP Pool. Each call to Reconcile probes the
// pool's range and converges the pool's external rows to match what's
// actually responding on the LAN: new responders get marked external, prior
// responders that go silent get a missed-cycle bump and are released after
// MissThreshold consecutive misses.
type Reconciler struct {
	scanner       Scanner
	pool          Pool
	missThreshold int // releases after this many consecutive missed scans
	clock         func() time.Time
}

// ReconcilerOption tunes a Reconciler at construction.
type ReconcilerOption func(*Reconciler)

// WithMissThreshold overrides the consecutive-miss count before a previously
// external row is released. Default 3 matches the Proxmox reconciler's
// VACATE_MISS_THRESHOLD; with a 5-min scan cadence that's 15 min of silence.
func WithMissThreshold(n int) ReconcilerOption {
	return func(r *Reconciler) {
		if n > 0 {
			r.missThreshold = n
		}
	}
}

// WithClock injects a clock for tests.
func WithClock(f func() time.Time) ReconcilerOption {
	return func(r *Reconciler) {
		if f != nil {
			r.clock = f
		}
	}
}

// NewReconciler constructs a Reconciler with sensible defaults.
func NewReconciler(scanner Scanner, pool Pool, opts ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		scanner:       scanner,
		pool:          pool,
		missThreshold: 3,
		clock:         time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Report describes what changed during a Reconcile. Mirrors the IP-pool
// reconciler's Report struct for log consistency.
type Report struct {
	Marked  []string  // IPs newly marked external this cycle
	Touched int       // IPs already marked external that responded again
	Missed  []string  // IPs that didn't respond — got a missed-cycle bump
	Freed   []string  // IPs released after exceeding the miss threshold
	At      time.Time // when the scan started
}

// Reconcile runs one scan-and-converge pass. Pool rows whose source is not
// "external" are never modified — Proxmox-sourced (local/adopted) and
// reserved rows are owned by the other reconciler / live-provision flow.
func (r *Reconciler) Reconcile(ctx context.Context) (Report, error) {
	rep := Report{At: r.clock()}

	rows, err := r.pool.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list pool: %w", err)
	}

	// Build the candidate list from rows the netscan is *allowed* to claim.
	// Free rows are claimable. External rows we revisit to refresh their
	// freshness counter. Everything else (reserved, local-allocated,
	// adopted) is off-limits — the Proxmox/provision side owns those.
	candidates := make([]net.IP, 0, len(rows))
	priorExternal := map[string]bool{}
	for _, row := range rows {
		switch {
		case row.Status == ippool.StatusFree:
			if ip := net.ParseIP(row.IP); ip != nil {
				candidates = append(candidates, ip)
			}
		case row.Status == ippool.StatusAllocated && row.Source == ippool.SourceExternal:
			if ip := net.ParseIP(row.IP); ip != nil {
				candidates = append(candidates, ip)
				priorExternal[row.IP] = true
			}
		}
	}

	if len(candidates) == 0 {
		return rep, nil
	}

	hits, err := r.scanner.Scan(ctx, candidates)
	if err != nil {
		return rep, fmt.Errorf("scan: %w", err)
	}

	// Defensive: only consider hits we actually asked about. A misbehaving
	// scanner that returns extra IPs (or one operating with a stale candidate
	// list) must not be allowed to claim rows the reconciler hasn't vetted —
	// the pool's MarkExternal guard would catch most cases, but this keeps
	// the report counts honest and prevents silent fan-out.
	candidateSet := make(map[string]bool, len(candidates))
	for _, ip := range candidates {
		candidateSet[ip.String()] = true
	}

	hitSet := make(map[string]bool, len(hits))
	for _, ip := range hits {
		s := ip.String()
		if !candidateSet[s] {
			continue
		}
		hitSet[s] = true
	}

	var errs []error

	// Phase 1: hits → claim or refresh.
	for ip := range hitSet {
		if priorExternal[ip] {
			if err := r.pool.TouchSeen(ctx, ip); err != nil {
				errs = append(errs, fmt.Errorf("touch %s: %w", ip, err))
				continue
			}
			rep.Touched++
			continue
		}
		if err := r.pool.MarkExternal(ctx, ip); err != nil {
			// MarkExternal silently no-ops when Proxmox has already adopted
			// the IP — only genuine errors land here.
			errs = append(errs, fmt.Errorf("mark external %s: %w", ip, err))
			continue
		}
		rep.Marked = append(rep.Marked, ip)
	}

	// Phase 2: prior external rows that didn't respond this cycle →
	// missed-cycle bump, release after threshold. Bounded by priorExternal
	// (we only touch what we already own).
	for ip := range priorExternal {
		if hitSet[ip] {
			continue
		}
		post, err := r.pool.IncrementMissedCycles(ctx, ip)
		if err != nil {
			errs = append(errs, fmt.Errorf("increment missed_cycles %s: %w", ip, err))
			continue
		}
		if post < r.missThreshold {
			rep.Missed = append(rep.Missed, ip)
			continue
		}
		if err := r.pool.Release(ctx, ip); err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", ip, err))
			continue
		}
		rep.Freed = append(rep.Freed, ip)
		log.Printf("netscan: released %s after %d missed cycles", ip, post)
	}

	if len(errs) > 0 {
		return rep, errors.Join(errs...)
	}
	return rep, nil
}
