// Package vpcmgr owns the Networking-v1 VPC primitive — a VXLAN
// zone shared by N VMs across cluster nodes plus a dedicated gateway
// LXC for NAT egress.
//
// Architecture (Neutron L3-agent equivalent):
//
//   - Zone: VXLAN on a per-VPC VNI, peers = every online cluster node.
//   - VNet: one per VPC, attached to the zone.
//   - Subnet: per-VPC /16 carved from the VPC pool (default
//     10.0.0.0/9), gateway = .1 of the /16, SNAT=0 (PVE doesn't
//     honor snat=1 on VXLAN, so a gateway LXC handles MASQUERADE).
//   - Gateway LXC: provisioned by package `gateway` and wired into
//     CreateVPC at the end. Lives on a designated network node.
//   - Memberships: VPCMembership rows assign per-VM IPs from the
//     VPC's /16 pool, allocated linearly starting at .10.
//
// Why per-VPC zones (vs. one cluster-wide VXLAN zone with VLAN
// segmentation): isolation is explicit at the PVE-zone level — no
// risk of a misconfigured VLAN tag leaking traffic between tenants.
// Costs: O(N) zone count for N VPCs, but PVE handles that fine up
// to ~hundreds. Above that we'd consolidate.
package vpcmgr

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

// SDNClient is the slice of *proxmox.Client this package uses for
// the VXLAN zone + VNet + subnet lifecycle.
type SDNClient interface {
	CreateSDNZone(ctx context.Context, z proxmox.SDNZone) error
	DeleteSDNZone(ctx context.Context, zone string) error
	CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error
	DeleteSDNVNet(ctx context.Context, vnet string) error
	CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error
	DeleteSDNSubnet(ctx context.Context, vnet, subnetID string) error
	ApplySDN(ctx context.Context) error
}

// PeerResolver returns the comma-joined list of online cluster node
// IPs for the VXLAN `peers=` field. Implemented by a small adapter
// around proxmox.ResolveOnlinePeerIPs in production.
type PeerResolver interface {
	ResolvePeers(ctx context.Context) (string, error)
}

// GatewayProvisioner abstracts the gateway-LXC lifecycle. Wired
// after construction via SetGateway so vpcmgr and gateway packages
// don't form an import cycle.
type GatewayProvisioner interface {
	Provision(ctx context.Context, vpc *db.VPC) (gatewayVMID int, gatewayNode string, err error)
	Destroy(ctx context.Context, vpc *db.VPC) error
}

// Config holds deployment-specific knobs.
type Config struct {
	// PoolCIDR is the supernet VPC /16s carve from. Default
	// 10.0.0.0/9 (32K /16s — far more than any single cluster ever
	// needs). Must be /16-or-larger.
	PoolCIDR string
	// VPCSize is the prefix length of each per-VPC subnet. Default
	// /16 (65K hosts) — overkill but matches OpenStack/AWS defaults.
	VPCSize int
}

// Service is the VPC manager. It owns the VPC + VPCMembership tables
// and orchestrates PVE state transitions.
type Service struct {
	px      SDNClient
	peers   PeerResolver
	db      *gorm.DB
	gw      GatewayProvisioner
	cfg     Config
	enabled bool
}

// New constructs a Service. PoolCIDR is required; SubnetSize defaults
// to /16. The gateway provisioner is installed later via SetGateway.
func New(px SDNClient, peers PeerResolver, dbConn *gorm.DB, cfg Config) (*Service, error) {
	if cfg.PoolCIDR == "" {
		return nil, errors.New("vpcmgr: PoolCIDR is required")
	}
	if _, _, err := net.ParseCIDR(cfg.PoolCIDR); err != nil {
		return nil, fmt.Errorf("vpcmgr: invalid PoolCIDR %q: %w", cfg.PoolCIDR, err)
	}
	if cfg.VPCSize == 0 {
		cfg.VPCSize = 16
	}
	if cfg.VPCSize < 12 || cfg.VPCSize > 24 {
		return nil, fmt.Errorf("vpcmgr: VPCSize must be /12..-/24, got /%d", cfg.VPCSize)
	}
	return &Service{px: px, peers: peers, db: dbConn, cfg: cfg, enabled: true}, nil
}

// SetGateway installs the GatewayProvisioner. CreateVPC fails fast
// when this is nil — gateway provisioning is not optional in v1.
func (s *Service) SetGateway(g GatewayProvisioner) {
	s.gw = g
}

// SetEnabled toggles whether new VPCs can be created. Live-reloadable
// from the admin Settings page; default is enabled. Existing VPCs
// keep working when disabled — only Create + AddMember refuse.
func (s *Service) SetEnabled(v bool) { s.enabled = v }

// Enabled reports the live toggle state.
func (s *Service) Enabled() bool { return s.enabled }

// maxCollisionRetries is how many salt-and-retry passes we make on
// VPC zone-name collision. Same birthday-bound math as standalonenet.
const maxCollisionRetries = 8

// memberStartOffset is the first usable host index within a VPC /16.
// .1 is the gateway, .2..-.9 reserved for future Nimbus uses.
const memberStartOffset = 10

// CreateVPC provisions a new VPC end-to-end: allocate /16, create
// PVE zone+vnet+subnet, then provision the gateway LXC. Atomicity
// is best-effort — partial PVE state is rolled back on failure. The
// row is hard-deleted on rollback so retries don't trip the unique
// indexes against tombstoned rows.
func (s *Service) CreateVPC(ctx context.Context, ownerID uint, name string) (*db.VPC, error) {
	if !s.enabled {
		return nil, &internalerrors.ValidationError{
			Field:   "network_mode",
			Message: "VPCs are disabled by the administrator",
		}
	}
	if s.gw == nil {
		return nil, errors.New("vpcmgr: gateway provisioner not wired")
	}
	if ownerID == 0 {
		return nil, &internalerrors.ValidationError{Field: "owner_id", Message: "owner_id required"}
	}
	name = strings.TrimSpace(name)
	if !validVPCName(name) {
		return nil, &internalerrors.ValidationError{
			Field:   "name",
			Message: "VPC name must be 1–32 chars, lowercase alphanumeric or hyphens",
		}
	}

	peers, err := s.peers.ResolvePeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster peers: %w", err)
	}
	if peers == "" {
		return nil, errors.New("vpcmgr: no online cluster nodes — cannot create VXLAN zone")
	}

	// Hash + collision-retry. The salt is the (owner, name, attempt)
	// tuple — re-running CreateVPC with the same name+owner returns
	// the existing row (idempotent) but a second creator picking the
	// same name in a different account gets a unique zone.
	for attempt := 0; attempt < maxCollisionRetries; attempt++ {
		salt := fmt.Sprintf("%d/%s", ownerID, name)
		if attempt > 0 {
			salt = fmt.Sprintf("%s#%d", salt, attempt)
		}
		row, err := s.tryInsertRow(ctx, ownerID, name, salt)
		if err == nil {
			// DB write OK — provision PVE state.
			if perr := s.bootstrapPVE(ctx, row, peers); perr != nil {
				_ = s.db.WithContext(ctx).Unscoped().Delete(row).Error
				return nil, fmt.Errorf("provision pve state for vpc %s: %w", name, perr)
			}
			// Gateway LXC.
			gwVMID, gwNode, gerr := s.gw.Provision(ctx, row)
			if gerr != nil {
				// Rollback PVE state + DB row.
				s.tearDownPVE(context.Background(), row)
				_ = s.db.WithContext(ctx).Unscoped().Delete(row).Error
				return nil, fmt.Errorf("provision gateway lxc: %w", gerr)
			}
			row.GatewayLXCID = &gwVMID
			row.GatewayNode = gwNode
			row.Status = "active"
			if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
				log.Printf("vpcmgr: persist gateway info for vpc %d: %v (vpc is up but DB is stale)", row.ID, err)
			}
			return row, nil
		}
		if !isUniqueViolation(err) {
			return nil, fmt.Errorf("persist vpc row: %w", err)
		}
		// Owner+name uniqueness conflict means the user already has
		// a VPC by this name — return ConflictError, don't retry.
		if strings.Contains(err.Error(), "idx_vpc_owner_name") ||
			strings.Contains(err.Error(), "owner_id") || strings.Contains(err.Error(), "name") {
			return nil, &internalerrors.ConflictError{
				Message: fmt.Sprintf("a VPC named %q already exists for this user", name),
			}
		}
		// Otherwise: zone-name or CIDR collision — retry with salt.
	}
	return nil, &internalerrors.ConflictError{
		Message: fmt.Sprintf("vpcmgr: zone-name collisions exhausted after %d retries", maxCollisionRetries),
	}
}

// ReapResult captures the row-level outcomes of ReapStuckProvisioning
// so the caller can log them separately.
type ReapResult struct {
	VPCsMarkedError    int64
	OrphanMemberships  int64
}

// ReapStuckProvisioning runs the startup-side cleanup for VPCs:
//
//  1. Drops VPCMembership rows whose vm_id has no live db.VM row.
//     The VM-provision flow inserts the membership BEFORE the db.VM
//     row, so a crash between those steps leaves an orphan
//     membership pointing at a vmid that was never tracked. Scoped to
//     VPCs in provisioning/error/degraded so we don't touch healthy
//     state. (Status='active' rows can have orphan memberships too in
//     pathological cases — TODO if it bites.)
//  2. Flips any vpcs.status = 'provisioning' rows to 'error'.
//     CreateVPC is synchronous and the status='active' write is its
//     last step; anything still in 'provisioning' at startup means
//     the previous process died mid-create. The PVE-side state may
//     be partial — the operator can delete from the UI which is
//     tolerant of missing PVE objects.
//
// Step 1 runs before step 2 so a freshly-reaped VPC also gets its
// memberships cleaned in the same pass.
func (s *Service) ReapStuckProvisioning(ctx context.Context) (ReapResult, error) {
	var out ReapResult
	if s == nil || s.db == nil {
		return out, nil
	}

	// Drop orphan memberships first. Subqueries reference the same
	// tables that GORM gives soft-delete clauses on by default; we
	// scope explicitly to live rows so a tombstoned db.VM doesn't
	// rescue an actually-orphan membership.
	orphanRes := s.db.WithContext(ctx).Unscoped().
		Where(`vpc_id IN (?) AND vm_id NOT IN (?)`,
			s.db.Model(&db.VPC{}).
				Where("status IN ?", []string{"provisioning", "error", "degraded"}).
				Where("deleted_at IS NULL").
				Select("id"),
			s.db.Model(&db.VM{}).
				Where("deleted_at IS NULL").
				Select("vmid"),
		).
		Delete(&db.VPCMembership{})
	if orphanRes.Error != nil {
		return out, fmt.Errorf("reap orphan memberships: %w", orphanRes.Error)
	}
	out.OrphanMemberships = orphanRes.RowsAffected

	// Flip stuck provisioning rows to error.
	statusRes := s.db.WithContext(ctx).Model(&db.VPC{}).
		Where("status = ?", "provisioning").
		Update("status", "error")
	if statusRes.Error != nil {
		return out, fmt.Errorf("mark stuck vpcs as error: %w", statusRes.Error)
	}
	out.VPCsMarkedError = statusRes.RowsAffected

	return out, nil
}

// DeleteVPC tears down a VPC. Refuses if any VMs are still members.
// Reverse-order cleanup: gateway LXC → subnet → vnet → zone → row.
func (s *Service) DeleteVPC(ctx context.Context, vpcID, ownerID uint, isAdmin bool) error {
	var row db.VPC
	q := s.db.WithContext(ctx).Where("id = ?", vpcID)
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &internalerrors.NotFoundError{Resource: "vpc", ID: fmt.Sprintf("%d", vpcID)}
		}
		return fmt.Errorf("lookup vpc: %w", err)
	}

	var memberCount int64
	if err := s.db.WithContext(ctx).Model(&db.VPCMembership{}).
		Where("vpc_id = ?", vpcID).Count(&memberCount).Error; err != nil {
		return fmt.Errorf("count members: %w", err)
	}
	if memberCount > 0 {
		return &internalerrors.ConflictError{
			Message: fmt.Sprintf("VPC has %d member VM(s); delete or move them first", memberCount),
		}
	}

	if s.gw != nil {
		if err := s.gw.Destroy(ctx, &row); err != nil {
			log.Printf("vpcmgr: destroy gateway for vpc %d: %v (continuing)", row.ID, err)
		}
	}
	s.tearDownPVE(ctx, &row)
	if err := s.db.WithContext(ctx).Unscoped().Delete(&row).Error; err != nil {
		return fmt.Errorf("delete vpc row: %w", err)
	}
	return nil
}

// ListVPCs returns every VPC the caller can see. Members see their
// own; admins see all.
func (s *Service) ListVPCs(ctx context.Context, ownerID uint, isAdmin bool) ([]db.VPC, error) {
	var rows []db.VPC
	q := s.db.WithContext(ctx).Order("created_at DESC")
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list vpcs: %w", err)
	}
	return rows, nil
}

// GetVPC fetches a single VPC by ID, scoped by ownership unless
// isAdmin is true.
func (s *Service) GetVPC(ctx context.Context, vpcID, ownerID uint, isAdmin bool) (*db.VPC, error) {
	var row db.VPC
	q := s.db.WithContext(ctx).Where("id = ?", vpcID)
	if !isAdmin {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vpc", ID: fmt.Sprintf("%d", vpcID)}
		}
		return nil, fmt.Errorf("get vpc: %w", err)
	}
	return &row, nil
}

// AllocateMemberIP picks the next free host IP within the VPC's CIDR
// and inserts a VPCMembership row for the VM. Returns the allocated
// IP. Linear-scan from .10 — VPCs hold 65K addresses by default, so
// even a worst-case full pool is sub-millisecond on SQLite.
//
// Callers pass the new VM's PVE vmid (from /cluster/nextid). PVE
// recycles vmids of deleted VMs, so a previously-released vmid can
// come back attached to a fresh provision. The membership table has a
// global UNIQUE on vm_id (a VM is in at most one VPC), so any orphan
// row pointing at the same vmid would block every insert. We drop
// orphans up-front: vmid has just been handed out by nextid, so by
// definition no live VM owns it, and any matching row is dead state
// the previous deletion failed to clean up.
func (s *Service) AllocateMemberIP(ctx context.Context, vpcID, vmID uint) (string, error) {
	var vpc db.VPC
	if err := s.db.WithContext(ctx).First(&vpc, vpcID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", &internalerrors.NotFoundError{Resource: "vpc", ID: fmt.Sprintf("%d", vpcID)}
		}
		return "", fmt.Errorf("lookup vpc: %w", err)
	}
	_, ipnet, err := net.ParseCIDR(vpc.CIDR)
	if err != nil {
		return "", fmt.Errorf("parse vpc cidr %q: %w", vpc.CIDR, err)
	}
	prefix, _ := ipnet.Mask.Size()
	totalHosts := uint32(1) << uint(32-prefix)
	base := ipv4ToUint32(ipnet.IP.To4())

	res := s.db.WithContext(ctx).Unscoped().
		Where("vm_id = ?", vmID).Delete(&db.VPCMembership{})
	if res.Error != nil {
		return "", fmt.Errorf("clear orphan membership for vmid %d: %w", vmID, res.Error)
	}
	if res.RowsAffected > 0 {
		log.Printf("vpcmgr: cleared %d orphan membership row(s) for reused vmid %d before allocation", res.RowsAffected, vmID)
	}

	// Snapshot existing memberships into a set so we can find the
	// first gap. Linear in member count; fine for 1K-member VPCs.
	var existing []string
	if err := s.db.WithContext(ctx).Model(&db.VPCMembership{}).
		Where("vpc_id = ?", vpcID).Pluck("vm_ip", &existing).Error; err != nil {
		return "", fmt.Errorf("list memberships: %w", err)
	}
	taken := make(map[string]struct{}, len(existing))
	for _, ip := range existing {
		taken[ip] = struct{}{}
	}

	for offset := uint32(memberStartOffset); offset < totalHosts-1; offset++ {
		candidate := uint32ToIPv4(base + offset).String()
		if _, ok := taken[candidate]; ok {
			continue
		}
		mem := &db.VPCMembership{VPCID: vpcID, VMID: vmID, VMIP: candidate}
		if err := s.db.WithContext(ctx).Create(mem).Error; err != nil {
			if isUniqueViolation(err) {
				// Lost a race on this IP; retry from the next
				// offset. Orphan rows for vmID were dropped
				// above, so a unique violation here is on
				// (vpc_id, vm_ip) — i.e. someone else just
				// took this IP.
				taken[candidate] = struct{}{}
				continue
			}
			return "", fmt.Errorf("insert membership: %w", err)
		}
		return candidate, nil
	}
	return "", &internalerrors.ConflictError{
		Message: fmt.Sprintf("vpc %d (CIDR %s) has no free host addresses", vpcID, vpc.CIDR),
	}
}

// ReleaseMember removes a VM's membership from its VPC. Idempotent:
// missing row is treated as success.
func (s *Service) ReleaseMember(ctx context.Context, vmID uint) error {
	if err := s.db.WithContext(ctx).Unscoped().
		Where("vm_id = ?", vmID).Delete(&db.VPCMembership{}).Error; err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	return nil
}

// GetMembership returns the VM's VPC membership, or nil if the VM is
// not in a VPC. Used by provision delete to dispatch cleanup.
func (s *Service) GetMembership(ctx context.Context, vmID uint) (*db.VPCMembership, error) {
	var mem db.VPCMembership
	if err := s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&mem).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup membership: %w", err)
	}
	return &mem, nil
}

// CountMembers reports how many VMs are members of a VPC. Used by
// the API list view (member-count column) and by the delete-VPC
// pre-check.
func (s *Service) CountMembers(ctx context.Context, vpcID uint) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&db.VPCMembership{}).
		Where("vpc_id = ?", vpcID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count members: %w", err)
	}
	return int(n), nil
}

// tryInsertRow computes the zone name + /16 for a salt and inserts
// the VPC row. Returns the row on success or the DB error.
func (s *Service) tryInsertRow(ctx context.Context, ownerID uint, name, salt string) (*db.VPC, error) {
	zoneName := proxmox.FormatVPCZoneName(salt)
	cidr, err := s.cidrFromSalt(salt)
	if err != nil {
		return nil, err
	}
	row := &db.VPC{
		OwnerID:  ownerID,
		Name:     name,
		CIDR:     cidr,
		ZoneName: zoneName,
		VNetName: zoneName,
		Status:   "provisioning",
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, err
	}
	return row, nil
}

// bootstrapPVE creates the per-VPC PVE state. Failure leaves orphan
// state — the caller-level rollback (Unscoped delete + tearDownPVE)
// covers it.
func (s *Service) bootstrapPVE(ctx context.Context, row *db.VPC, peers string) error {
	// VNI: low 24 bits of the zone-name hash. VXLAN VNIs are 24-bit.
	sum := sha256.Sum256([]byte(row.ZoneName))
	vni := int(binary.BigEndian.Uint32(sum[:4]) & 0xFFFFFF)
	if vni < 100 {
		vni += 100 // VNIs below 100 are reserved by some switches
	}

	if err := s.px.CreateSDNZone(ctx, proxmox.SDNZone{
		Zone:  row.ZoneName,
		Type:  "vxlan",
		Peers: peers,
	}); err != nil {
		return fmt.Errorf("create zone %s: %w", row.ZoneName, err)
	}
	if err := s.px.CreateSDNVNet(ctx, proxmox.SDNVNet{
		VNet: row.VNetName,
		Zone: row.ZoneName,
		Tag:  vni,
	}); err != nil {
		return fmt.Errorf("create vnet %s: %w", row.VNetName, err)
	}
	gateway, _, err := splitGatewayAndHost(row.CIDR)
	if err != nil {
		return fmt.Errorf("derive gateway: %w", err)
	}
	if err := s.px.CreateSDNSubnet(ctx, proxmox.SDNSubnet{
		Subnet:  row.CIDR,
		VNet:    row.VNetName,
		Gateway: gateway,
		// SNAT=false on VXLAN — PVE silently drops the rule. The
		// per-VPC gateway LXC handles MASQUERADE.
		SNAT: false,
	}); err != nil {
		return fmt.Errorf("create subnet %s: %w", row.CIDR, err)
	}
	if err := s.px.ApplySDN(ctx); err != nil {
		return fmt.Errorf("apply sdn: %w", err)
	}
	// ApplySDN ("Reload network configuration on all nodes" task in
	// PVE's UI) already fans `ifreload -a` out to every cluster
	// member. We used to follow up with a per-peer ReloadNodeNetwork
	// call after a single fresh-node initial-bootstrap quirk, but
	// that doubled the reload count on every VPC create — keep the
	// single ApplySDN reload and let operators run `ifreload -a` by
	// hand if a freshly-joined node is missing the VXLAN bridge.
	return nil
}

// tearDownPVE reverses bootstrapPVE. Tolerates "already gone" 404s.
// Best-effort — failures are logged but don't propagate, mirroring
// standalonenet.Destroy's policy.
func (s *Service) tearDownPVE(ctx context.Context, row *db.VPC) {
	subnetID := proxmox.FormatSDNSubnetID(row.ZoneName, row.CIDR)
	if err := s.px.DeleteSDNSubnet(ctx, row.VNetName, subnetID); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		log.Printf("vpcmgr: delete pve subnet %s/%s: %v", row.VNetName, subnetID, err)
	}
	if err := s.px.DeleteSDNVNet(ctx, row.VNetName); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		log.Printf("vpcmgr: delete pve vnet %s: %v", row.VNetName, err)
	}
	if err := s.px.DeleteSDNZone(ctx, row.ZoneName); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		log.Printf("vpcmgr: delete pve zone %s: %v", row.ZoneName, err)
	}
	if err := s.px.ApplySDN(ctx); err != nil {
		log.Printf("vpcmgr: apply sdn after teardown of vpc %d: %v", row.ID, err)
	}
}

// cidrFromSalt picks a /<VPCSize> within the supernet deterministically.
func (s *Service) cidrFromSalt(salt string) (string, error) {
	_, supernet, err := net.ParseCIDR(s.cfg.PoolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool cidr: %w", err)
	}
	supernetPrefix, _ := supernet.Mask.Size()
	if s.cfg.VPCSize < supernetPrefix {
		return "", fmt.Errorf("vpc size /%d cannot exceed pool /%d",
			s.cfg.VPCSize, supernetPrefix)
	}
	bitsAvailable := uint(s.cfg.VPCSize - supernetPrefix)
	if bitsAvailable > 31 {
		return "", fmt.Errorf("supernet too large for index width")
	}
	totalSlices := uint32(1) << bitsAvailable
	sum := sha256.Sum256([]byte("vpc:" + salt))
	idx := binary.BigEndian.Uint32(sum[:4]) % totalSlices
	stride := uint32(1) << (32 - uint(s.cfg.VPCSize))
	base := ipv4ToUint32(supernet.IP.To4())
	return fmt.Sprintf("%s/%d", uint32ToIPv4(base+idx*stride), s.cfg.VPCSize), nil
}

// splitGatewayAndHost returns (gateway=.1, host=.10) of a CIDR.
// .10 is the convention; .2..-.9 reserved for future Nimbus uses.
func splitGatewayAndHost(cidr string) (gateway, host string, err error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	base := ipv4ToUint32(ipnet.IP.To4())
	return uint32ToIPv4(base + 1).String(), uint32ToIPv4(base + memberStartOffset).String(), nil
}

func ipv4ToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}

func uint32ToIPv4(n uint32) net.IP {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, n)
	return out
}

func validVPCName(name string) bool {
	if len(name) < 1 || len(name) > 32 {
		return false
	}
	for i, c := range name {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i > 0 {
			ok = ok || c == '-'
		}
		if !ok {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint violation")
}

// PeerResolverFunc adapts a function to the PeerResolver interface.
type PeerResolverFunc func(ctx context.Context) (string, error)

// ResolvePeers calls the wrapped function.
func (f PeerResolverFunc) ResolvePeers(ctx context.Context) (string, error) { return f(ctx) }

// Compile-time assertion the proxmox.Client satisfies SDNClient.
var _ SDNClient = (*proxmox.Client)(nil)
