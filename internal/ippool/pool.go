// Package ippool manages a static IP allocation pool backed by SQLite.
//
// The pool is seeded at boot with every address in [start, end]. Provisioning
// reserves the first free IP atomically, then later marks it allocated once
// the VM is verifiably reachable. Failures release the reservation so the IP
// returns to the pool.
//
// Atomicity relies on (a) GORM transactions and (b) the
// MaxOpenConns=1 setting in db.New — SQLite serializes writes naturally.
package ippool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrPoolExhausted is returned by Reserve when no free IP is available.
var ErrPoolExhausted = errors.New("ip pool exhausted")

// ErrInvalidRange is returned when start/end are malformed or end < start.
var ErrInvalidRange = errors.New("invalid ip range")

// Pool wraps a GORM DB and exposes the four allocation operations the
// provisioner needs.
type Pool struct {
	db *gorm.DB
}

// New constructs a Pool backed by the supplied GORM connection. The IP table
// must already be auto-migrated by the caller — use Model() to register it.
func New(db *gorm.DB) *Pool {
	return &Pool{db: db}
}

// Model returns the type to register with GORM AutoMigrate. Keeps the model's
// owning package self-contained so db.New stays decoupled.
func Model() any { return &IPAllocation{} }

// Seed inserts every IP in [start, end] (inclusive) with status=free. Existing
// rows are left untouched, so re-running Seed after expanding the pool only
// adds the new addresses.
//
// Both start and end must be IPv4 addresses on the same /24 boundary or
// smaller — we don't validate the subnet here, just that end >= start.
func (p *Pool) Seed(ctx context.Context, start, end string) error {
	startIP := net.ParseIP(start).To4()
	endIP := net.ParseIP(end).To4()
	if startIP == nil || endIP == nil {
		return fmt.Errorf("%w: %q .. %q", ErrInvalidRange, start, end)
	}
	if compareIP(endIP, startIP) < 0 {
		return fmt.Errorf("%w: end %s precedes start %s", ErrInvalidRange, end, start)
	}

	addresses := make([]IPAllocation, 0, 256)
	cur := append(net.IP(nil), startIP...)
	for {
		addresses = append(addresses, IPAllocation{
			IP:     cur.String(),
			Status: StatusFree,
		})
		if cur.Equal(endIP) {
			break
		}
		incrementIP(cur)
		// Defensive cap: a /16 is 65k addresses; refuse to seed more.
		if len(addresses) > 65536 {
			return fmt.Errorf("%w: range exceeds 65536 addresses", ErrInvalidRange)
		}
	}

	// Use INSERT ... ON CONFLICT DO NOTHING so existing rows are preserved
	// (idempotent re-seed when expanding the range).
	return p.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&addresses).Error
}

// Reserve atomically claims the first free IP and tags it with hostname.
// Returns ErrPoolExhausted when none is available.
//
// Caller must subsequently call MarkAllocated (success path) or Release
// (failure path); a stuck "reserved" row indicates a crashed provision and
// should be cleaned up by an out-of-band janitor (not in Phase 1 scope).
func (p *Pool) Reserve(ctx context.Context, hostname string) (string, error) {
	var ip string
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row IPAllocation
		if err := tx.
			Where("status = ?", StatusFree).
			Order("ip ASC").
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPoolExhausted
			}
			return fmt.Errorf("select free ip: %w", err)
		}

		now := time.Now().UTC()
		hn := hostname
		updates := map[string]any{
			"status":      StatusReserved,
			"hostname":    &hn,
			"reserved_at": &now,
		}
		if err := tx.Model(&IPAllocation{}).
			Where("ip = ? AND status = ?", row.IP, StatusFree).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("mark %s reserved: %w", row.IP, err)
		}
		ip = row.IP
		return nil
	})
	if err != nil {
		return "", err
	}
	return ip, nil
}

// MarkAllocated promotes a reservation to allocated and binds it to a VMID.
// Idempotent if the row is already allocated to the same VMID.
func (p *Pool) MarkAllocated(ctx context.Context, ip string, vmid int) error {
	now := time.Now().UTC()
	res := p.db.WithContext(ctx).
		Model(&IPAllocation{}).
		Where("ip = ? AND status IN ?", ip, []string{StatusReserved, StatusAllocated}).
		Updates(map[string]any{
			"status":       StatusAllocated,
			"vmid":         &vmid,
			"allocated_at": &now,
		})
	if res.Error != nil {
		return fmt.Errorf("mark %s allocated: %w", ip, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("ip %s is not reserved", ip)
	}
	return nil
}

// Release returns an IP to the free pool, clearing all metadata. Safe to call
// for an already-free IP — it's a no-op in that case.
func (p *Pool) Release(ctx context.Context, ip string) error {
	res := p.db.WithContext(ctx).
		Model(&IPAllocation{}).
		Where("ip = ?", ip).
		Updates(map[string]any{
			"status":       StatusFree,
			"vmid":         nil,
			"hostname":     nil,
			"reserved_at":  nil,
			"allocated_at": nil,
		})
	if res.Error != nil {
		return fmt.Errorf("release %s: %w", ip, res.Error)
	}
	return nil
}

// List returns all IP allocations ordered by IP. Useful for the admin UI and
// debugging.
func (p *Pool) List(ctx context.Context) ([]IPAllocation, error) {
	var out []IPAllocation
	if err := p.db.WithContext(ctx).
		Order("ip ASC").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("list allocations: %w", err)
	}
	return out, nil
}

// incrementIP increments an IPv4 address in-place.
func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

func compareIP(a, b net.IP) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
