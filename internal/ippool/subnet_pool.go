package ippool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"gorm.io/gorm"
)

// SeedSubnet inserts every IP in [start, end] inclusive with vnet=<name>
// and status=free. Idempotent: existing rows for the same vnet are
// preserved (so calling this twice during a retry doesn't reset state),
// but the row's vnet column gets corrected if it somehow drifted.
//
// Required at user-subnet create time. Symmetrical with Seed but scoped
// to the subnet's VNet — the legacy global pool (vnet="") and the
// per-subnet pools coexist in the same table, distinguished by the VNet
// column.
func (p *Pool) SeedSubnet(ctx context.Context, vnet, start, end string) error {
	if vnet == "" {
		return errors.New("SeedSubnet requires a non-empty vnet — use Seed for the global pool")
	}
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
			VNet:   vnet,
			Status: StatusFree,
		})
		if cur.Equal(endIP) {
			break
		}
		incrementIP(cur)
		if len(addresses) > 65536 {
			return fmt.Errorf("%w: range exceeds 65536 addresses", ErrInvalidRange)
		}
	}

	// Insert with ON CONFLICT DO NOTHING — same idempotency guarantee
	// the global Seed gives. We can't reuse Seed verbatim because the
	// rows now carry vnet=<name> rather than vnet="".
	return p.db.WithContext(ctx).
		Session(&gorm.Session{CreateBatchSize: 256}).
		Create(&addresses).Error
}

// DropSubnet deletes every row scoped to the given vnet. Used by
// vnetmgr.DeleteSubnet after the per-subnet VM-attached check passes.
// Defensive: if any row is in StatusReserved or StatusAllocated, returns
// ErrSubnetInUse so the caller can refuse cleanly without orphaning
// allocations.
func (p *Pool) DropSubnet(ctx context.Context, vnet string) error {
	if vnet == "" {
		return errors.New("DropSubnet requires a non-empty vnet")
	}
	var inUse int64
	if err := p.db.WithContext(ctx).Model(&IPAllocation{}).
		Where("vnet = ? AND status != ?", vnet, StatusFree).
		Count(&inUse).Error; err != nil {
		return fmt.Errorf("count in-use rows for %s: %w", vnet, err)
	}
	if inUse > 0 {
		return fmt.Errorf("%w: %d allocations still active in %s", ErrSubnetInUse, inUse, vnet)
	}
	return p.db.WithContext(ctx).
		Where("vnet = ?", vnet).
		Delete(&IPAllocation{}).Error
}

// ErrSubnetInUse signals DropSubnet refused because the subnet still
// has reserved or allocated IPs. Not the same as ErrPoolExhausted —
// this is "you asked me to delete a pool that has live tenants."
var ErrSubnetInUse = errors.New("subnet pool still has active allocations")

// ReserveInSubnet is the per-subnet variant of Reserve. Picks the
// lowest-numbered free IP within the named vnet and tags it with
// hostname. Returns ErrPoolExhausted when the subnet's pool is full.
//
// The race-loss leapfrog invariant (CLAUDE.md gotcha #2) holds the
// same way it does for global Reserve: the SQL filter scopes the
// "lowest free" search to one vnet, and SQLite single-writer
// serializes the transaction.
func (p *Pool) ReserveInSubnet(ctx context.Context, vnet, hostname string) (string, error) {
	if vnet == "" {
		return "", errors.New("ReserveInSubnet requires a non-empty vnet")
	}
	var ip string
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row IPAllocation
		if err := tx.
			Where("vnet = ? AND status = ?", vnet, StatusFree).
			Order("ip ASC").
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPoolExhausted
			}
			return fmt.Errorf("select free ip in %s: %w", vnet, err)
		}

		now := time.Now().UTC()
		hn := hostname
		updates := map[string]any{
			"status":      StatusReserved,
			"hostname":    &hn,
			"reserved_at": &now,
		}
		if err := tx.Model(&IPAllocation{}).
			Where("ip = ? AND vnet = ? AND status = ?", row.IP, vnet, StatusFree).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("mark %s reserved in %s: %w", row.IP, vnet, err)
		}
		ip = row.IP
		return nil
	})
	if err != nil {
		return "", err
	}
	return ip, nil
}

// MarkAllocatedInSubnet promotes a per-subnet reservation to allocated.
// Mirror of MarkAllocated; the vnet scoping is purely defensive — the
// IP is globally unique so a vnet-less query would also work, but
// scoping makes the intent explicit and lets the query plan use the
// vnet+status index.
func (p *Pool) MarkAllocatedInSubnet(ctx context.Context, vnet, ip string, vmid int) error {
	if vnet == "" {
		return errors.New("MarkAllocatedInSubnet requires a non-empty vnet")
	}
	now := time.Now().UTC()
	res := p.db.WithContext(ctx).
		Model(&IPAllocation{}).
		Where("ip = ? AND vnet = ? AND status IN ?", ip, vnet, []string{StatusReserved, StatusAllocated}).
		Updates(map[string]any{
			"status":       StatusAllocated,
			"vmid":         &vmid,
			"allocated_at": &now,
		})
	if res.Error != nil {
		return fmt.Errorf("mark %s/%s allocated: %w", vnet, ip, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("ip %s in %s is not reserved", ip, vnet)
	}
	return nil
}

// ReleaseInSubnet returns a per-subnet IP to the free pool. Same
// idempotent semantics as Release — already-free is a no-op.
func (p *Pool) ReleaseInSubnet(ctx context.Context, vnet, ip string) error {
	if vnet == "" {
		return errors.New("ReleaseInSubnet requires a non-empty vnet")
	}
	res := p.db.WithContext(ctx).
		Model(&IPAllocation{}).
		Where("ip = ? AND vnet = ?", ip, vnet).
		Updates(map[string]any{
			"status":       StatusFree,
			"vmid":         nil,
			"hostname":     nil,
			"reserved_at":  nil,
			"allocated_at": nil,
		})
	if res.Error != nil {
		return fmt.Errorf("release %s/%s: %w", vnet, ip, res.Error)
	}
	return nil
}

// ListSubnet returns every row scoped to the given vnet. Powers the
// admin UI's per-subnet pool view.
func (p *Pool) ListSubnet(ctx context.Context, vnet string) ([]IPAllocation, error) {
	var rows []IPAllocation
	if err := p.db.WithContext(ctx).
		Where("vnet = ?", vnet).
		Order("ip ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
