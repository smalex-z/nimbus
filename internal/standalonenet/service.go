// Package standalonenet owns the per-VM Simple-zone networking
// primitive — every Standalone VM in Nimbus gets its own dedicated
// Proxmox SDN zone + VNet + subnet, all host-local, with PVE's
// native SNAT (`subnet.snat=1`) handling outbound NAT.
//
// Why per-VM zones (vs. a shared cluster-wide one): Simple zones are
// host-local by design — the gateway IP and the NAT MASQUERADE rule
// live on the PVE host that owns the bridge. Two Standalone VMs on
// different nodes can safely use the same `10.128.X.0/24` because
// the bridges never connect; collisions only matter when two VMs on
// the *same* node hash to the same `/24`. The DB's unique index on
// `subnet_cidr` plus a salt-and-retry loop catches that case.
//
// What this package does NOT do:
//   - Cross-VM communication. Each VM is alone on its `/24`. For
//     multi-VM private networks, callers go through `vpcmgr` (the
//     VXLAN-zone primitive).
//   - IP pool reservation. A Standalone VM's IP is hardcoded to .10
//     of its `/24` (the `/24` itself is the unit of allocation, not
//     individual addresses within it). No `internal/ippool`
//     involvement.
//   - Cloud-init. The caller (provision.Service) builds the cidata
//     ISO using the gateway/IP/prefix this package returns.
package standalonenet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
)

// SDNClient is the slice of *proxmox.Client this package uses.
// "Accept interfaces, return structs" — narrow shape so tests can
// stub without standing up the whole Proxmox surface.
type SDNClient interface {
	CreateSDNZone(ctx context.Context, z proxmox.SDNZone) error
	DeleteSDNZone(ctx context.Context, zone string) error
	CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error
	DeleteSDNVNet(ctx context.Context, vnet string) error
	CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error
	DeleteSDNSubnet(ctx context.Context, vnet, subnetID string) error
	ApplySDN(ctx context.Context) error
}

// Config is the deployment-specific knobs the Service needs.
type Config struct {
	// PoolCIDR is the supernet Standalone VM /24s carve from. Default
	// 10.128.0.0/9 (32768 /24s, plenty for any single-cluster scale).
	// Must be a /9 or larger CIDR with /24 slices fitting cleanly
	// inside it.
	PoolCIDR string
	// SubnetSize is the prefix length for each per-VM subnet. /24
	// gives 254 usable hosts per VM, which is overkill for one VM
	// but matches what users expect for a private network. Tighter
	// sizes (e.g. /28 for 14 hosts) would pack more VMs into the
	// supernet but break the mental model of "this VM has its own
	// network." Default 24.
	SubnetSize int
}

// Service is the Standalone VM network manager. Provision creates
// per-VM PVE state and persists a row; Destroy reverses both.
type Service struct {
	px  SDNClient
	db  *gorm.DB
	cfg Config
}

// New constructs a Service. cfg.PoolCIDR is required and must be a
// valid CIDR; cfg.SubnetSize defaults to 24 if zero.
func New(px SDNClient, dbConn *gorm.DB, cfg Config) (*Service, error) {
	if cfg.PoolCIDR == "" {
		return nil, errors.New("standalonenet: PoolCIDR is required")
	}
	if _, _, err := net.ParseCIDR(cfg.PoolCIDR); err != nil {
		return nil, fmt.Errorf("standalonenet: invalid PoolCIDR %q: %w", cfg.PoolCIDR, err)
	}
	if cfg.SubnetSize == 0 {
		cfg.SubnetSize = 24
	}
	if cfg.SubnetSize < 16 || cfg.SubnetSize > 30 {
		return nil, fmt.Errorf("standalonenet: SubnetSize must be /16..-/30, got /%d", cfg.SubnetSize)
	}
	return &Service{px: px, db: dbConn, cfg: cfg}, nil
}

// maxCollisionRetries is how many salt-and-retry passes we make on
// hash collision before giving up. With 28 bits of zone-name space
// the birthday bound puts 50% collision odds at ~16K entries; 8
// retries give a vanishingly small per-VM failure rate even at
// hundreds of thousands of standalone VMs.
const maxCollisionRetries = 8

// Provision creates the per-VM Simple zone + VNet + subnet on the
// given node and persists a `db.StandaloneVMNetwork` row. Returns
// the row on success; the caller (provision.Service) reads VNetName
// for SetVMNetwork, GatewayIP/VMIP/SubnetCIDR for cloud-init.
//
// vmIdentifier is a stable string — Nimbus passes the VM's UUID, but
// any unique input works. The zone name is derived from
// `sha256(vmIdentifier)` so re-running with the same identifier is
// idempotent (returns the existing row when its zone is still in
// PVE; no-op).
//
// Failure modes:
//   - All collision retries exhausted → ConflictError surfaces to the
//     user with "subnet pool exhausted" (very unlikely in practice).
//   - PVE-side errors during create propagate. We attempt rollback
//     of partial PVE state but leave the orphan if rollback fails;
//     subsequent calls return a clear error rather than silently
//     overwriting.
func (s *Service) Provision(ctx context.Context, vmID uint, vmIdentifier, node string) (*db.StandaloneVMNetwork, error) {
	if vmID == 0 {
		return nil, errors.New("standalonenet: vmID required")
	}
	if vmIdentifier == "" {
		return nil, errors.New("standalonenet: vmIdentifier required")
	}
	if node == "" {
		return nil, errors.New("standalonenet: node required")
	}

	// Idempotency: if a row already exists for this VM, return it.
	// Caller treats this as "already provisioned, nothing to do."
	var existing db.StandaloneVMNetwork
	if err := s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&existing).Error; err == nil {
		return &existing, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("standalonenet: lookup existing: %w", err)
	}

	// Hash + collision-retry loop. Each iteration salts the input,
	// computes a candidate zone name + /24, and tries the DB insert.
	// DB unique constraints on zone_name and subnet_cidr act as the
	// collision detector — the salt-and-retry handles birthday hits
	// without holding application-level locks.
	for attempt := 0; attempt < maxCollisionRetries; attempt++ {
		salt := vmIdentifier
		if attempt > 0 {
			salt = fmt.Sprintf("%s#%d", vmIdentifier, attempt)
		}
		row, err := s.tryInsertRow(ctx, vmID, salt, node)
		if err == nil {
			// DB insert succeeded; now do the PVE side. On PVE
			// failure we roll back the row and bail with a real
			// error (the operator sees what went wrong).
			if perr := s.bootstrapPVE(ctx, row); perr != nil {
				// Hard-delete on rollback. Soft-delete leaves the row
				// with deleted_at set, but the unique index on
				// zone_name/vnet_name doesn't filter on that — a
				// retry would hit a phantom collision against our
				// own tombstone.
				_ = s.db.WithContext(ctx).Unscoped().Delete(row).Error
				return nil, fmt.Errorf("standalonenet: provision pve state for vm %d: %w", vmID, perr)
			}
			return row, nil
		}
		if !isUniqueViolation(err) {
			return nil, fmt.Errorf("standalonenet: persist row: %w", err)
		}
		// Unique violation → hash collision; retry with next salt.
	}
	return nil, &internalerrors.ConflictError{
		Message: fmt.Sprintf("standalonenet: zone-name/subnet collisions exhausted after %d retries (pool may be saturated)", maxCollisionRetries),
	}
}

// Destroy tears down a Standalone VM's PVE state and deletes the DB
// row. Idempotent: a missing PVE-side resource is treated as
// success since a previous failed cleanup may have already removed
// it. Called from `provision.Service.deleteVM` when the VM has a
// `db.StandaloneVMNetwork` row.
func (s *Service) Destroy(ctx context.Context, vmID uint) error {
	var row db.StandaloneVMNetwork
	if err := s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // nothing to clean up
		}
		return fmt.Errorf("standalonenet: lookup row: %w", err)
	}

	// Reverse-order PVE teardown: subnet → vnet → zone, then ApplySDN.
	// Tolerate "already gone" 404s — the caller may be retrying after
	// a partial failure.
	subnetID := proxmox.FormatSDNSubnetID(row.ZoneName, row.SubnetCIDR)
	if err := s.px.DeleteSDNSubnet(ctx, row.VNetName, subnetID); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		return fmt.Errorf("standalonenet: delete pve subnet %s/%s: %w", row.VNetName, subnetID, err)
	}
	if err := s.px.DeleteSDNVNet(ctx, row.VNetName); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		return fmt.Errorf("standalonenet: delete pve vnet %s: %w", row.VNetName, err)
	}
	if err := s.px.DeleteSDNZone(ctx, row.ZoneName); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		return fmt.Errorf("standalonenet: delete pve zone %s: %w", row.ZoneName, err)
	}
	if err := s.px.ApplySDN(ctx); err != nil {
		// Apply failure is logged but doesn't block row deletion —
		// PVE's pending state will be applied on the next ApplySDN
		// from any source.
		log.Printf("standalonenet: apply sdn after destroy of vm %d: %v (continuing)", vmID, err)
	}

	if err := s.db.WithContext(ctx).Unscoped().Delete(&row).Error; err != nil {
		return fmt.Errorf("standalonenet: delete row: %w", err)
	}
	return nil
}

// Get returns the Standalone VM network for a VM, or nil/nil if the
// VM isn't using this primitive (e.g. it's in a VPC, or it's a
// legacy vm.SubnetID-tracked VM). Used by provision.Service.deleteVM
// to dispatch to the right cleanup path.
func (s *Service) Get(ctx context.Context, vmID uint) (*db.StandaloneVMNetwork, error) {
	var row db.StandaloneVMNetwork
	if err := s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("standalonenet: lookup: %w", err)
	}
	return &row, nil
}

// tryInsertRow computes the zone name + /24 for a salt and inserts
// the row. Returns the row on success or the DB error (which the
// caller checks for unique-violation to drive collision retry).
func (s *Service) tryInsertRow(ctx context.Context, vmID uint, salt, node string) (*db.StandaloneVMNetwork, error) {
	zoneName := proxmox.FormatStandaloneZoneName(salt)
	subnetCIDR, err := s.cidrFromSalt(salt)
	if err != nil {
		return nil, err
	}
	gatewayIP, vmIP, err := splitGatewayAndHost(subnetCIDR)
	if err != nil {
		return nil, err
	}
	row := &db.StandaloneVMNetwork{
		VMID:       vmID,
		ZoneName:   zoneName,
		VNetName:   zoneName, // same string; one VNet per Standalone zone
		SubnetCIDR: subnetCIDR,
		GatewayIP:  gatewayIP,
		VMIP:       vmIP,
		Node:       node,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, err
	}
	return row, nil
}

// bootstrapPVE creates the per-VM PVE state. Failure mode: if any
// step partially succeeds and the next fails, we leave the orphan
// in place — the caller-level rollback (DeleteRow above) plus the
// admin-facing Reset endpoint cover recovery. We don't try to walk
// back partial PVE state inline because it'd just generate another
// failure mode without simplifying the caller.
func (s *Service) bootstrapPVE(ctx context.Context, row *db.StandaloneVMNetwork) error {
	// Simple zones are host-local — only the node that runs the VM
	// needs the bridge. Pin via Nodes= so PVE doesn't auto-create
	// the bridge on every other cluster member (cleaner UI, fewer
	// orphan bridges if a node leaves the cluster).
	if err := s.px.CreateSDNZone(ctx, proxmox.SDNZone{
		Zone:  row.ZoneName,
		Type:  "simple",
		Nodes: row.Node,
	}); err != nil {
		return fmt.Errorf("create zone %s: %w", row.ZoneName, err)
	}
	if err := s.px.CreateSDNVNet(ctx, proxmox.SDNVNet{
		VNet: row.VNetName,
		Zone: row.ZoneName,
	}); err != nil {
		return fmt.Errorf("create vnet %s: %w", row.VNetName, err)
	}
	if err := s.px.CreateSDNSubnet(ctx, proxmox.SDNSubnet{
		Subnet:  row.SubnetCIDR,
		VNet:    row.VNetName,
		Gateway: row.GatewayIP,
		SNAT:    true, // PVE handles MASQUERADE on the per-host bridge
	}); err != nil {
		return fmt.Errorf("create subnet %s: %w", row.SubnetCIDR, err)
	}
	if err := s.px.ApplySDN(ctx); err != nil {
		return fmt.Errorf("apply sdn: %w", err)
	}
	// ApplySDN itself triggers cluster-wide ifreload via PVE's
	// "Reload network configuration on all nodes" task. We don't
	// chase it with an explicit per-node reload anymore — that
	// doubled the reload count for every provision. If a fresh
	// node ever needs a manual nudge, `ifreload -a` on the host
	// is the one-shot recovery.
	return nil
}

// cidrFromSalt picks a /<SubnetSize> within the supernet
// deterministically from a salt. The hash's low bits select the
// /24-offset; high bits would be wasted (the supernet is /9 so 23
// host bits, plenty of room for the full /24 index without modulo
// bias).
func (s *Service) cidrFromSalt(salt string) (string, error) {
	_, supernet, err := net.ParseCIDR(s.cfg.PoolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool cidr: %w", err)
	}
	supernetPrefix, _ := supernet.Mask.Size()
	if s.cfg.SubnetSize < supernetPrefix {
		return "", fmt.Errorf("subnet size /%d cannot exceed pool /%d",
			s.cfg.SubnetSize, supernetPrefix)
	}
	bitsAvailable := uint(s.cfg.SubnetSize - supernetPrefix)
	if bitsAvailable > 31 {
		// Shouldn't happen for /9 → /24, but defensive.
		return "", fmt.Errorf("supernet too large for index width")
	}
	totalSlices := uint32(1) << bitsAvailable

	sum := sha256.Sum256([]byte(salt))
	idx := binary.BigEndian.Uint32(sum[:4]) % totalSlices

	stride := uint32(1) << (32 - uint(s.cfg.SubnetSize))
	base := ipv4ToUint32(supernet.IP.To4())
	addr := base + idx*stride
	return fmt.Sprintf("%s/%d", uint32ToIPv4(addr), s.cfg.SubnetSize), nil
}

// splitGatewayAndHost returns (gateway=.1, host=.10) of a CIDR.
// .10 is convention; .2 through .9 are reserved for future Nimbus
// uses (DHCP servers, anycast services) without forcing a renumber.
func splitGatewayAndHost(cidr string) (gateway, host string, err error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	_ = ip
	base := ipv4ToUint32(ipnet.IP.To4())
	return uint32ToIPv4(base + 1).String(), uint32ToIPv4(base + 10).String(), nil
}

func ipv4ToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}

func uint32ToIPv4(n uint32) net.IP {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, n)
	return out
}

// isUniqueViolation reports whether a gorm error is a unique-index
// violation. SQLite returns "UNIQUE constraint failed: <table>.<col>"
// in the error string; we don't have access to the underlying
// driver-error type here without dragging in a sqlite import, so
// substring-matching is the cheapest reliable check.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint violation")
}

// Compile-time assertion that the real *proxmox.Client satisfies our
// SDN client surface. Catches signature drift at build time.
var _ SDNClient = (*proxmox.Client)(nil)
