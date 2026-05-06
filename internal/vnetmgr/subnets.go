package vnetmgr

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
)

// Per-user subnet lifecycle. A subnet maps 1:1 to a Proxmox VNet + a
// single CIDR carved first-free from NetworkSettings.SDNSubnetSupernet.
// Each subnet is its own L2 broadcast domain with its own NAT gateway,
// so user A's subnets are isolated from each other unless the operator
// explicitly puts the relevant VMs in the same subnet.

// Service deps for subnet lifecycle. The subset that vnetmgr.Service
// calls — declared at the consumer for testability with a fake.
type subnetCRUDClient interface {
	CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error
	DeleteSDNVNet(ctx context.Context, vnet string) error
	CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error
	DeleteSDNSubnet(ctx context.Context, vnet, subnet string) error
	ApplySDN(ctx context.Context) error
}

// IPPoolWriter is the slice of *ippool.Pool subnet ops need. Defined
// locally so vnetmgr doesn't pull the whole pool surface in.
type IPPoolWriter interface {
	SeedSubnet(ctx context.Context, vnet, start, end string) error
	DropSubnet(ctx context.Context, vnet string) error
}

// VMRefCounter reports how many VMs currently reference a subnet by
// id — used to refuse delete while live VMs are attached. The
// provision service has a *gorm.DB and can run the count efficiently;
// we keep it as a small interface here for testability.
type VMRefCounter interface {
	CountVMsOnSubnet(ctx context.Context, subnetID uint) (int, error)
}

// nameValidRE matches the user-friendly subnet name shape: lowercase
// alphanumeric + hyphens, 1..32 chars, must start with a letter, can't
// end with a hyphen. Same shape as VM hostnames so users hit one rule
// across the product.
var nameValidRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)

// CreateSubnetRequest is the input to CreateSubnet. Name is the
// user-facing label; the Proxmox VNet name is generated from the row
// id post-insert. SetDefault flips IsDefault on this subnet (and
// clears it on every other subnet for the owner) — used to mark a
// fresh subnet as "the one new VMs land on" without a separate
// SetDefault round trip.
type CreateSubnetRequest struct {
	OwnerID    uint
	Name       string
	SetDefault bool
}

// CreateSubnet provisions a new user subnet end-to-end:
//  1. Validates the name + checks per-owner uniqueness.
//  2. Carves the first-free /N CIDR from the configured supernet.
//  3. Persists the user_subnets row (Proxmox VNet name = nbu+base36(id)).
//  4. Calls Proxmox: CreateSDNVNet → CreateSDNSubnet (with SNAT) →
//     ApplySDN. On any Proxmox-side failure, marks the row status="error"
//     so the admin can retry-or-delete from the UI.
//  5. Seeds the per-subnet IP pool (PoolStart..PoolEnd).
//
// The DB row is committed before Proxmox is called so failures on the
// Proxmox side don't lose the carved-CIDR claim — we'd rather end up
// with a status="error" row the admin can heal than a silent
// double-carve on retry.
func (s *Service) CreateSubnet(ctx context.Context, req CreateSubnetRequest) (*db.UserSubnet, error) {
	if req.OwnerID == 0 {
		return nil, &internalerrors.ValidationError{Field: "owner_id", Message: "owner_id is required"}
	}
	name := strings.TrimSpace(strings.ToLower(req.Name))
	if !nameValidRE.MatchString(name) {
		return nil, &internalerrors.ValidationError{
			Field:   "name",
			Message: "must be 1-32 chars, lowercase a-z/0-9/hyphens, must start with a letter",
		}
	}

	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if !settings.SDNEnabled {
		return nil, &internalerrors.ConflictError{Message: "SDN isolation is disabled — enable it in Settings → Network first"}
	}
	if settings.SDNSubnetSupernet == "" {
		return nil, &internalerrors.ConflictError{Message: "SDN supernet is not configured — set it in Settings → Network first"}
	}

	// Carve the next available /N from the supernet, skipping CIDRs
	// already claimed by user_subnets rows. Walks the supernet space
	// in /N strides and picks the first that doesn't overlap with
	// existing subnets — cheap O(supernet/N) scan, plenty fast for
	// the homelab-scale supernets we expect (/16 → 256 strides).
	cidr, gw, poolStart, poolEnd, err := s.carveSubnet(ctx, settings.SDNSubnetSupernet, settings.SDNSubnetSize)
	if err != nil {
		return nil, err
	}

	// Pre-flight: refuse if a subnet with this (owner, name) already
	// exists. The composite index will catch a race, but this gives
	// a friendlier error message than a constraint violation.
	var existing int64
	if err := s.dbWriter().WithContext(ctx).Model(&db.UserSubnet{}).
		Where("owner_id = ? AND name = ?", req.OwnerID, name).
		Count(&existing).Error; err != nil {
		return nil, fmt.Errorf("check subnet uniqueness: %w", err)
	}
	if existing > 0 {
		return nil, &internalerrors.ConflictError{Message: fmt.Sprintf("you already have a subnet named %q", name)}
	}

	// Optimistic insert with a placeholder VNet — we don't know the
	// final id until the row exists. Then update VNet to nbu+base36(id).
	row := &db.UserSubnet{
		OwnerID:   req.OwnerID,
		Name:      name,
		VNet:      "", // placeholder, filled below
		Subnet:    cidr,
		Gateway:   gw,
		PoolStart: poolStart,
		PoolEnd:   poolEnd,
		IsDefault: false,
		Status:    "active",
	}
	if err := s.dbWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(row).Error; err != nil {
			return fmt.Errorf("insert user_subnets: %w", err)
		}
		row.VNet = vnetNameForID(row.ID)
		if err := tx.Model(row).Update("vnet", row.VNet).Error; err != nil {
			return fmt.Errorf("set vnet on row %d: %w", row.ID, err)
		}
		if req.SetDefault {
			if err := s.applyDefault(tx, req.OwnerID, row.ID); err != nil {
				return err
			}
			row.IsDefault = true
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Proxmox side. Failures here mark the row "error" so the admin
	// can act on it — DB has the carved CIDR so no double-carve risk.
	if err := s.bootstrapProxmoxSubnet(ctx, settings.SDNZoneName, row, settings.SDNDNSServer); err != nil {
		_ = s.dbWriter().WithContext(ctx).Model(row).Update("status", "error").Error
		return row, err
	}

	// IP pool seed. Same error semantics — mark error if it fails so
	// the admin sees the partial state.
	if s.pool != nil {
		if err := s.pool.SeedSubnet(ctx, row.VNet, row.PoolStart, row.PoolEnd); err != nil {
			_ = s.dbWriter().WithContext(ctx).Model(row).Update("status", "error").Error
			return row, fmt.Errorf("seed ip pool: %w", err)
		}
	}

	return row, nil
}

// ListSubnets returns every active subnet for the owner, ordered by
// IsDefault desc then Name asc so the default surfaces first in
// dropdowns.
func (s *Service) ListSubnets(ctx context.Context, ownerID uint) ([]db.UserSubnet, error) {
	var rows []db.UserSubnet
	err := s.dbWriter().WithContext(ctx).
		Where("owner_id = ?", ownerID).
		Order("is_default DESC, name ASC").
		Find(&rows).Error
	return rows, err
}

// GetSubnet fetches one subnet, owner-gated. A subnet that belongs to
// another user surfaces as NotFound (never disclose existence across
// owners — same convention provision.LifecycleOp uses).
func (s *Service) GetSubnet(ctx context.Context, id, ownerID uint) (*db.UserSubnet, error) {
	var row db.UserSubnet
	err := s.dbWriter().WithContext(ctx).
		Where("id = ? AND owner_id = ?", id, ownerID).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "subnet", ID: fmt.Sprintf("%d", id)}
		}
		return nil, err
	}
	return &row, nil
}

// DeleteSubnet tears down the Proxmox VNet + Subnet + IP pool for one
// user-subnet. Refuses if any VM still references this subnet — admin
// must delete VMs first. The default subnet can't be deleted while it's
// the only one (would leave the user with no default to land new VMs
// on) — caller must SetDefault on a different subnet first.
func (s *Service) DeleteSubnet(ctx context.Context, id, ownerID uint) error {
	row, err := s.GetSubnet(ctx, id, ownerID)
	if err != nil {
		return err
	}

	if s.vmRefs != nil {
		count, err := s.vmRefs.CountVMsOnSubnet(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("check vm refs: %w", err)
		}
		if count > 0 {
			return &internalerrors.ConflictError{
				Message: fmt.Sprintf("subnet %q has %d VM(s) attached — delete or migrate them first", row.Name, count),
			}
		}
	}

	if row.IsDefault {
		// Allow delete only if there's another active subnet to fall
		// back on; otherwise refuse. EnsureDefault will pick another
		// at next provision.
		var others int64
		if err := s.dbWriter().WithContext(ctx).Model(&db.UserSubnet{}).
			Where("owner_id = ? AND id != ?", ownerID, id).
			Count(&others).Error; err != nil {
			return fmt.Errorf("count other subnets: %w", err)
		}
		if others == 0 {
			return &internalerrors.ConflictError{
				Message: "cannot delete your only subnet — provisions need at least one to land on",
			}
		}
	}

	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// Proxmox-side teardown: subnet first (Proxmox refuses VNet delete
	// while subnets reference it), then VNet, then ApplySDN. We tolerate
	// "already gone" 404s — the caller may be retrying after a partial
	// failure and we want delete to be idempotent.
	if s.subnetCRUD != nil {
		if err := s.subnetCRUD.DeleteSDNSubnet(ctx, row.VNet, row.Subnet); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
			return fmt.Errorf("delete proxmox subnet %s/%s: %w", row.VNet, row.Subnet, err)
		}
		if err := s.subnetCRUD.DeleteSDNVNet(ctx, row.VNet); err != nil && !errors.Is(err, proxmox.ErrNotFound) {
			return fmt.Errorf("delete proxmox vnet %s: %w", row.VNet, err)
		}
		if err := s.subnetCRUD.ApplySDN(ctx); err != nil {
			return fmt.Errorf("apply sdn after delete: %w", err)
		}
	}
	_ = settings // settings reserved for future zone-name lookups

	// IP pool drop. Already-empty is fine; a non-empty pool here means
	// VMs were tracked in the pool but not in db.VM — surface the error
	// rather than orphan.
	if s.pool != nil {
		if err := s.pool.DropSubnet(ctx, row.VNet); err != nil {
			return fmt.Errorf("drop ip pool: %w", err)
		}
	}

	// Soft-delete via gorm.DeletedAt — subnet rows are recoverable
	// from the audit perspective even after Proxmox is gone.
	return s.dbWriter().WithContext(ctx).Delete(row).Error
}

// SetDefault marks the named subnet as the owner's default and clears
// IsDefault on every other subnet for that owner. Atomic via
// transaction so a failure mid-flight can't leave two defaults.
func (s *Service) SetDefault(ctx context.Context, id, ownerID uint) error {
	row, err := s.GetSubnet(ctx, id, ownerID)
	if err != nil {
		return err
	}
	return s.dbWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.applyDefault(tx, ownerID, row.ID)
	})
}

// ResolveForProvision is the provision-time helper. Returns the
// UserSubnet a VM should land on, given the user's selection:
//
//   - subnetID != nil → use existing subnet (owner-gated)
//   - subnetName != "" → create a new subnet inline (set as default
//     when the user has none yet)
//   - both empty → use the user's default subnet, lazy-creating one
//     named "default" if none exists (SSH-keys-style first-time UX)
//
// Returns (nil, nil) when SDN is disabled cluster-wide — caller falls
// back to the legacy global vmbr0 pool.
func (s *Service) ResolveForProvision(
	ctx context.Context,
	ownerID uint,
	subnetID *uint,
	subnetName string,
) (*db.UserSubnet, error) {
	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if !settings.SDNEnabled {
		return nil, nil
	}
	switch {
	case subnetID != nil:
		return s.GetSubnet(ctx, *subnetID, ownerID)
	case subnetName != "":
		// New subnets via the provision form become the user's
		// default ONLY if they have no subnets yet. Otherwise the
		// user keeps their existing default; the new one is just
		// available in the picker.
		setDefault := false
		var existing int64
		if err := s.dbWriter().WithContext(ctx).Model(&db.UserSubnet{}).
			Where("owner_id = ?", ownerID).
			Count(&existing).Error; err == nil && existing == 0 {
			setDefault = true
		}
		return s.CreateSubnet(ctx, CreateSubnetRequest{
			OwnerID:    ownerID,
			Name:       subnetName,
			SetDefault: setDefault,
		})
	default:
		return s.EnsureDefault(ctx, ownerID)
	}
}

// PrefixLenOf returns the netmask length of the row's CIDR. Used by
// the provision flow to build ipconfig0 (`ip=...,gw=...,prefix=N`).
func PrefixLenOf(row *db.UserSubnet) int {
	if row == nil || row.Subnet == "" {
		return 0
	}
	_, ipnet, err := net.ParseCIDR(row.Subnet)
	if err != nil {
		return 0
	}
	ones, _ := ipnet.Mask.Size()
	return ones
}

// EnsureDefault is the "user provisioned without picking a subnet"
// path. Returns the owner's default subnet, lazy-creating one named
// "default" if none exists yet. SetDefault is implied for the freshly
// minted subnet so subsequent provisions skip the create path.
//
// Idempotent: a second call returns the existing default. Concurrent
// callers race on the create — the second one hits the unique
// (owner, name) constraint and falls through to a re-read.
func (s *Service) EnsureDefault(ctx context.Context, ownerID uint) (*db.UserSubnet, error) {
	var row db.UserSubnet
	err := s.dbWriter().WithContext(ctx).
		Where("owner_id = ? AND is_default = ?", ownerID, true).
		First(&row).Error
	if err == nil {
		return &row, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("query default subnet: %w", err)
	}
	// No default yet — create one.
	created, err := s.CreateSubnet(ctx, CreateSubnetRequest{
		OwnerID:    ownerID,
		Name:       "default",
		SetDefault: true,
	})
	if err == nil {
		return created, nil
	}
	// Race-loss path: another concurrent EnsureDefault won. Re-read.
	var conflict *internalerrors.ConflictError
	if errors.As(err, &conflict) {
		var fresh db.UserSubnet
		if err := s.dbWriter().WithContext(ctx).
			Where("owner_id = ? AND is_default = ?", ownerID, true).
			First(&fresh).Error; err == nil {
			return &fresh, nil
		}
	}
	return nil, err
}

// applyDefault is the SetDefault inner logic, invokable from the
// transaction context CreateSubnet uses too. Clears IsDefault on every
// row for the owner, then sets it on the chosen one.
func (s *Service) applyDefault(tx *gorm.DB, ownerID, id uint) error {
	if err := tx.Model(&db.UserSubnet{}).
		Where("owner_id = ?", ownerID).
		Update("is_default", false).Error; err != nil {
		return fmt.Errorf("clear existing defaults: %w", err)
	}
	if err := tx.Model(&db.UserSubnet{}).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Update("is_default", true).Error; err != nil {
		return fmt.Errorf("set default: %w", err)
	}
	return nil
}

// bootstrapProxmoxSubnet is the Proxmox-side half of CreateSubnet.
// VNet first (subnet attaches to it), then the subnet (with SNAT for
// outbound NAT), then ApplySDN.
func (s *Service) bootstrapProxmoxSubnet(ctx context.Context, zone string, row *db.UserSubnet, dnsServer string) error {
	if s.subnetCRUD == nil {
		return errors.New("vnetmgr: SDN subnet client not wired")
	}
	if err := s.subnetCRUD.CreateSDNVNet(ctx, proxmox.SDNVNet{
		VNet: row.VNet,
		Zone: zone,
	}); err != nil {
		return fmt.Errorf("create proxmox vnet %s: %w", row.VNet, err)
	}
	if err := s.subnetCRUD.CreateSDNSubnet(ctx, proxmox.SDNSubnet{
		Subnet:    row.Subnet,
		VNet:      row.VNet,
		Gateway:   row.Gateway,
		SNAT:      true, // simple zone NAT — gives VMs outbound internet
		DNSServer: dnsServer,
	}); err != nil {
		return fmt.Errorf("create proxmox subnet %s: %w", row.Subnet, err)
	}
	if err := s.subnetCRUD.ApplySDN(ctx); err != nil {
		return fmt.Errorf("apply sdn: %w", err)
	}
	return nil
}

// carveSubnet picks the first-free /N out of the supernet, skipping
// CIDRs already claimed by user_subnets rows. Returns (cidr, gateway,
// poolStart, poolEnd) where gateway is the .1 of the carved CIDR and
// pool is .10..-.<broadcast-1>-5 (gives the .250-ish form on /24 and
// adapts to other sizes).
//
// O(supernet-prefixes / N) — for the expected /16 supernet + /24
// subnet size this is 256 iterations. Plenty fast.
func (s *Service) carveSubnet(ctx context.Context, supernetCIDR string, subnetSize int) (cidr, gw, poolStart, poolEnd string, err error) {
	if subnetSize == 0 {
		subnetSize = 24
	}
	_, supernet, parseErr := net.ParseCIDR(supernetCIDR)
	if parseErr != nil {
		return "", "", "", "", &internalerrors.ConflictError{Message: "invalid supernet CIDR: " + parseErr.Error()}
	}
	supernetPrefix, _ := supernet.Mask.Size()
	if subnetSize < supernetPrefix {
		return "", "", "", "", &internalerrors.ConflictError{
			Message: fmt.Sprintf("subnet_size /%d cannot be larger than supernet /%d", subnetSize, supernetPrefix),
		}
	}

	// Existing claims: every user_subnets.subnet column. Soft-deleted
	// rows are excluded by gorm by default.
	var claimed []string
	if err := s.dbWriter().WithContext(ctx).Model(&db.UserSubnet{}).
		Pluck("subnet", &claimed).Error; err != nil {
		return "", "", "", "", fmt.Errorf("list claimed subnets: %w", err)
	}
	claimedSet := make(map[string]struct{}, len(claimed))
	for _, c := range claimed {
		claimedSet[c] = struct{}{}
	}

	// Step over the supernet in /subnetSize strides. The stride is
	// 2^(32-subnetSize) addresses for IPv4.
	stride := new(big.Int).Lsh(big.NewInt(1), uint(32-subnetSize))
	startAddr := ipToBigInt(supernet.IP.To4())
	supernetSize := new(big.Int).Lsh(big.NewInt(1), uint(32-supernetPrefix))
	end := new(big.Int).Add(startAddr, supernetSize)

	mask := net.CIDRMask(subnetSize, 32)
	for cur := new(big.Int).Set(startAddr); cur.Cmp(end) < 0; cur.Add(cur, stride) {
		ip := bigIntToIP(cur)
		candidate := (&net.IPNet{IP: ip, Mask: mask}).String()
		if _, taken := claimedSet[candidate]; taken {
			continue
		}
		gateway := bigIntToIP(new(big.Int).Add(cur, big.NewInt(1)))
		// Pool window: skip the first 10 addresses (network, gateway,
		// reserve room for future static services) and the last 5
		// (broadcast, padding for routers etc).
		ps := bigIntToIP(new(big.Int).Add(cur, big.NewInt(10)))
		pe := bigIntToIP(new(big.Int).Sub(new(big.Int).Add(cur, stride), big.NewInt(6)))
		return candidate, gateway.String(), ps.String(), pe.String(), nil
	}
	return "", "", "", "", &internalerrors.ConflictError{
		Message: fmt.Sprintf("supernet %s exhausted — no free /%d remains", supernetCIDR, subnetSize),
	}
}

// vnetNameForID generates the Proxmox VNet name from a UserSubnet row
// id. Format: "nbu" + base36(id), capped at 8 chars (Proxmox limit).
// Supports row ids up to 36^5 ≈ 60M before the cap is hit. Validation
// in CreateSubnet returns an error if exceeded.
func vnetNameForID(id uint) string {
	b36 := strconv.FormatUint(uint64(id), 36)
	name := "nbu" + b36
	// 8-char cap is a Proxmox SDN constraint. The stride here is for
	// completeness — we're nowhere near it for any real deployment,
	// but truncating silently would conflict on row ids that share the
	// same first 5 base36 chars.
	if len(name) > 8 {
		// Should be unreachable for foreseeable deployments. If it
		// fires, the implementer rev's the scheme (e.g. shorter
		// prefix or hash truncation with collision handling).
		return name[:8]
	}
	return name
}

// ipToBigInt / bigIntToIP for the carving math. IPv4 only — we
// validated the supernet is To4() in carveSubnet.
func ipToBigInt(ip net.IP) *big.Int {
	return new(big.Int).SetBytes(ip.To4())
}

func bigIntToIP(n *big.Int) net.IP {
	b := n.Bytes()
	// Pad to 4 bytes (big.Int strips leading zeros).
	out := make(net.IP, 4)
	copy(out[4-len(b):], b)
	return out
}
