// Package vnetmgr owns the per-user Proxmox SDN VNet lifecycle.
//
// P1 scope (this file at first cut): a startup-time Bootstrap that
// reconciles the configured Nimbus SDN zone with Proxmox — creates the
// zone if missing, applies pending changes, and surfaces structured
// status. No user-VNet allocation yet; that lands in P2 alongside
// EnsureUserVNet and the per-vnet IP pool wiring.
//
// Why a separate package vs. living in provision.Service:
//   - Lifecycle: zone bootstrap runs once at startup; user-VNet
//     allocation is per-provision. provision.Service is already
//     stateful and growing; a focused package keeps the SDN concern
//     testable in isolation.
//   - Reuse: nodemgr's existing pattern of "small consumer-defined
//     interface for the Proxmox client" applies here too.
package vnetmgr

import (
	"context"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// SDNClient is the small subset of *proxmox.Client this package uses
// for zone-level operations (Bootstrap + Status). Subnet-CRUD calls
// live on the wider subnetCRUDClient interface declared in subnets.go;
// *proxmox.Client satisfies both.
type SDNClient interface {
	GetSDNZone(ctx context.Context, zone string) (*proxmox.SDNZone, error)
	CreateSDNZone(ctx context.Context, z proxmox.SDNZone) error
	ListSDNVNets(ctx context.Context) ([]proxmox.SDNVNet, error)
	ApplySDN(ctx context.Context) error
	// Subnet CRUD methods live on the same proxmox.Client; including
	// them here lets New(...) accept one client argument and
	// satisfy both the zone-level and subnet-level surfaces.
	CreateSDNVNet(ctx context.Context, v proxmox.SDNVNet) error
	DeleteSDNVNet(ctx context.Context, vnet string) error
	CreateSDNSubnet(ctx context.Context, s proxmox.SDNSubnet) error
	DeleteSDNSubnet(ctx context.Context, vnet, subnet string) error
}

// SettingsReader is the slice of internal/service.AuthService this
// package needs — just NetworkSettings reads. Same accept-interfaces
// shape as nodemgr's deps.
type SettingsReader interface {
	GetNetworkSettings() (*db.NetworkSettings, error)
}

// Service orchestrates SDN bootstrap + per-user subnet lifecycle.
// All methods are idempotent and safe to call from startup hooks.
type Service struct {
	px         SDNClient
	settings   SettingsReader
	dbConn     *gorm.DB         // optional; required for subnet CRUD
	pool       IPPoolWriter     // optional; required for subnet CRUD
	vmRefs     VMRefCounter     // optional; if nil, delete skips the VM-attached check
	subnetCRUD subnetCRUDClient // alias of px above; populated by New
}

// New constructs a Service for zone-level operations only (Bootstrap +
// Status). Subnet CRUD deps wire on via the With* builders so the
// startup-time bootstrap path doesn't have to thread them through.
func New(px SDNClient, settings SettingsReader) *Service {
	return &Service{
		px:         px,
		settings:   settings,
		subnetCRUD: px, // px satisfies both interfaces
	}
}

// WithDB wires the GORM connection used for user_subnets reads/writes.
// Required for subnet CRUD; not used by Bootstrap/Status.
func (s *Service) WithDB(db *gorm.DB) *Service {
	s.dbConn = db
	return s
}

// WithPool wires the IP-pool seed/drop hooks. Required for subnet
// CRUD; *ippool.Pool satisfies IPPoolWriter.
func (s *Service) WithPool(p IPPoolWriter) *Service {
	s.pool = p
	return s
}

// WithVMRefCounter wires the VM-attachment check used by DeleteSubnet
// to refuse while VMs still reference the subnet. Optional — if nil,
// deletes proceed without the check (test fakes typically pass nil).
func (s *Service) WithVMRefCounter(v VMRefCounter) *Service {
	s.vmRefs = v
	return s
}

// dbWriter returns the GORM connection for subnet CRUD. Surfaces a
// clear error in tests that forget to wire it rather than nil-panic.
func (s *Service) dbWriter() *gorm.DB {
	if s.dbConn == nil {
		// Unreachable in production (main.go wires it); helpful in
		// tests that exercise subnet methods without a DB by panicking
		// here rather than further down the call stack.
		panic("vnetmgr: DB not wired — call WithDB(db) on Service")
	}
	return s.dbConn
}

// StatusView is the shape returned by GET /api/settings/sdn so the
// admin UI can render the toggle + diagnostic panel without a second
// round trip. ZoneStatus is one of:
//
//	disabled     — SDNEnabled = false in NetworkSettings
//	missing-pkg  — Proxmox returned 501/404 (libpve-network-perl absent)
//	pending      — zone exists in Proxmox but not yet applied
//	active       — zone exists and is applied
//	unconfigured — SDNEnabled = true but SDNZoneName is empty
//	error        — ProxmoxError carries the underlying message
type StatusView struct {
	Enabled      bool   `json:"enabled"`
	ZoneName     string `json:"zone_name"`
	ZoneType     string `json:"zone_type"`
	Supernet     string `json:"supernet"`
	SubnetSize   int    `json:"subnet_size"`
	DNSServer    string `json:"dns_server,omitempty"`
	ZoneStatus   string `json:"zone_status"`
	VNetCount    int    `json:"vnet_count"`
	ProxmoxError string `json:"proxmox_error,omitempty"`
}

// Status assembles the StatusView for the admin UI. Read-only — does
// not create the zone. The bootstrap path is what creates; this is
// strictly diagnostic so the admin can see "the zone I asked for is
// running" before anyone provisions a VM.
func (s *Service) Status(ctx context.Context) (*StatusView, error) {
	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	view := &StatusView{
		Enabled:    settings.SDNEnabled,
		ZoneName:   settings.SDNZoneName,
		ZoneType:   settings.SDNZoneType,
		Supernet:   settings.SDNSubnetSupernet,
		SubnetSize: settings.SDNSubnetSize,
		DNSServer:  settings.SDNDNSServer,
	}
	if !settings.SDNEnabled {
		view.ZoneStatus = "disabled"
		return view, nil
	}
	if settings.SDNZoneName == "" {
		view.ZoneStatus = "unconfigured"
		return view, nil
	}

	zone, err := s.px.GetSDNZone(ctx, settings.SDNZoneName)
	if err != nil {
		// Distinguish "Proxmox doesn't know the SDN endpoint at all"
		// (package missing) from other failures so the UI can render
		// the operator-actionable hint.
		if isSDNPackageMissing(err) {
			view.ZoneStatus = "missing-pkg"
			view.ProxmoxError = err.Error()
			return view, nil
		}
		if errors.Is(err, proxmox.ErrNotFound) {
			view.ZoneStatus = "pending"
			return view, nil
		}
		view.ZoneStatus = "error"
		view.ProxmoxError = err.Error()
		return view, nil
	}
	if zone == nil {
		view.ZoneStatus = "pending"
		return view, nil
	}
	view.ZoneStatus = "active"

	// VNet count is "zone-scoped" but Proxmox's list endpoint returns
	// every VNet across all zones — filter here.
	vnets, err := s.px.ListSDNVNets(ctx)
	if err == nil {
		for _, v := range vnets {
			if v.Zone == settings.SDNZoneName {
				view.VNetCount++
			}
		}
	}
	return view, nil
}

// Bootstrap reconciles the configured SDN zone with Proxmox. Idempotent:
//
//   - SDNEnabled=false       → returns immediately, no Proxmox calls.
//   - Zone missing in Proxmox → creates it (matching the configured
//     type), then ApplySDN.
//   - Zone exists, type match → no-op (no ApplySDN — pending changes
//     from earlier failed runs are picked up by future create calls).
//   - Zone exists, type mismatch → returns ErrZoneTypeMismatch so the
//     operator sees the conflict rather than silently overwriting a
//     hand-managed zone.
//
// Called from main.go after db.New (and from the SDN settings handler
// when an admin saves with enabled=true). Surfaces SDN-package-missing
// as a structured warning rather than a fatal error — the rest of
// Nimbus continues to run on vmbr0.
func (s *Service) Bootstrap(ctx context.Context) error {
	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if !settings.SDNEnabled {
		return nil
	}
	if settings.SDNZoneName == "" {
		return errors.New("SDN enabled but zone name is empty — set Settings → Network → SDN zone")
	}

	zone, err := s.px.GetSDNZone(ctx, settings.SDNZoneName)
	if err != nil {
		if isSDNPackageMissing(err) {
			log.Printf("vnetmgr: SDN package missing on Proxmox — install libpve-network-perl on every node and retry")
			return ErrSDNPackageMissing
		}
		if !errors.Is(err, proxmox.ErrNotFound) {
			return fmt.Errorf("get sdn zone %q: %w", settings.SDNZoneName, err)
		}
		// ErrNotFound → fall through to create.
		zone = nil
	}

	if zone != nil {
		if zone.Type != settings.SDNZoneType {
			return &ZoneTypeMismatchError{
				ZoneName:    settings.SDNZoneName,
				WantType:    settings.SDNZoneType,
				ProxmoxType: zone.Type,
			}
		}
		// Already exists with the right type — nothing to do.
		log.Printf("vnetmgr: SDN zone %q already exists (type=%s)", zone.Zone, zone.Type)
		return nil
	}

	z := proxmox.SDNZone{
		Zone: settings.SDNZoneName,
		Type: settings.SDNZoneType,
	}
	if err := s.px.CreateSDNZone(ctx, z); err != nil {
		return fmt.Errorf("create sdn zone %q: %w", settings.SDNZoneName, err)
	}
	if err := s.px.ApplySDN(ctx); err != nil {
		return fmt.Errorf("apply sdn (after creating zone %q): %w", settings.SDNZoneName, err)
	}
	log.Printf("vnetmgr: created SDN zone %q (type=%s) and applied", z.Zone, z.Type)
	return nil
}

// ErrSDNPackageMissing is returned by Bootstrap when Proxmox returns
// 501/404 on an SDN endpoint — almost always means libpve-network-perl
// isn't installed cluster-wide. The handler maps this to a 503 with an
// actionable hint.
var ErrSDNPackageMissing = errors.New("proxmox SDN package missing — install libpve-network-perl on every node")

// ZoneTypeMismatchError signals the operator manually created a zone
// with our chosen name but a different type. Refusing to act is the
// safe thing — overwriting their config silently would be worse.
type ZoneTypeMismatchError struct {
	ZoneName    string
	WantType    string
	ProxmoxType string
}

func (e *ZoneTypeMismatchError) Error() string {
	return fmt.Sprintf("sdn zone %q already exists with type %q; nimbus configured to use type %q — rename or delete the existing zone in Proxmox first",
		e.ZoneName, e.ProxmoxType, e.WantType)
}

// isSDNPackageMissing detects the "Proxmox returned 501/404 on an SDN
// endpoint" signature. Older Proxmox installs without the SDN package
// return 404 ("path /cluster/sdn not found"); newer ones return 501
// ("not implemented"). Either way it's the same operator action.
func isSDNPackageMissing(err error) bool {
	if errors.Is(err, proxmox.ErrNotFound) {
		// ErrNotFound is ambiguous — could be "zone not found" or
		// "endpoint missing entirely". Caller (Bootstrap, Status)
		// handles ErrNotFound separately as "zone needs creating",
		// so this helper conservatively returns false. The 501 path
		// below catches the real package-missing case.
		return false
	}
	var httpErr *proxmox.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 501
	}
	return false
}
