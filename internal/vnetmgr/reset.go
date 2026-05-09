package vnetmgr

import (
	"context"
	"errors"
	"fmt"
	"log"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/proxmox"
)

// Reset tears down Nimbus's SDN state in Proxmox: deletes every
// user_subnets row (which destroys its PVE subnet+vnet + IP-pool
// rows), then deletes the configured zone, then applies SDN.
// Returns the platform to a "Clean" state, ready for the operator
// to save fresh SDN settings (e.g. switching simple → vxlan, or
// renaming the zone).
//
// REFUSES if any VMs are still attached to Nimbus subnets — Reset
// never destroys VM data. The operator must delete attached VMs
// first via the normal lifecycle (My VMs / Admin → Delete).
//
// Idempotent and best-effort per-step: if Proxmox is missing one
// of the things we expected (e.g. operator already nuked it via
// pvesh), we skip and continue. The end state is always "no Nimbus
// subnets in DB, no Nimbus zone in PVE."
func (s *Service) Reset(ctx context.Context) (*ResetReport, error) {
	if s.dbConn == nil {
		return nil, errors.New("vnetmgr.Reset: db not wired")
	}

	// Step 1: refuse if any VM still references a Nimbus subnet.
	// This is the ONLY hard gate — Reset never destroys VM data.
	var vmCount int64
	if err := s.dbConn.WithContext(ctx).
		Model(&db.VM{}).
		Where("subnet_id IS NOT NULL").
		Count(&vmCount).Error; err != nil {
		return nil, fmt.Errorf("count vms on subnets: %w", err)
	}
	if vmCount > 0 {
		return nil, &internalerrors.ConflictError{
			Message: fmt.Sprintf(
				"cannot reset SDN: %d VM(s) still attached to Nimbus subnets — "+
					"delete or migrate them via the normal VM lifecycle first",
				vmCount),
		}
	}

	report := &ResetReport{}

	// Step 2: tear down every user_subnets row. DeleteSubnet handles
	// the full Proxmox-side teardown (subnet → vnet → ApplySDN) plus
	// IP-pool drop and the DB row delete. Best-effort: log per-row
	// failures and keep going so a stuck row doesn't block the rest.
	var rows []db.UserSubnet
	if err := s.dbConn.WithContext(ctx).Find(&rows).Error; err != nil {
		return report, fmt.Errorf("list user subnets: %w", err)
	}
	for _, row := range rows {
		if err := s.DeleteSubnet(ctx, row.ID, row.OwnerID); err != nil {
			log.Printf("vnetmgr.Reset: delete subnet id=%d (vnet=%s): %v (continuing)",
				row.ID, row.VNet, err)
			report.SubnetFailures = append(report.SubnetFailures,
				fmt.Sprintf("subnet id=%d vnet=%s: %s", row.ID, row.VNet, err.Error()))
			continue
		}
		report.SubnetsDeleted++
	}

	// Step 3: scrub orphan PVE vnets in the configured zone. The DB
	// loop above only catches subnets Nimbus tracks; PVE may still
	// hold vnets/subnets from earlier failed deletes (e.g. residue
	// from a partial subnet teardown that crashed mid-flight). For
	// each vnet in our zone, list its subnets, delete them, then
	// delete the vnet. Without this scrub, the next step (zone
	// delete) fails with "zone is used by vnet X".
	settings, err := s.settings.GetNetworkSettings()
	if err != nil {
		return report, fmt.Errorf("get settings: %w", err)
	}
	if settings.SDNZoneName != "" && s.subnetCRUD != nil {
		if vnets, lerr := s.subnetCRUD.ListSDNVNets(ctx); lerr != nil {
			log.Printf("vnetmgr.Reset: list pve vnets: %v (continuing)", lerr)
			report.SubnetFailures = append(report.SubnetFailures,
				"list vnets: "+lerr.Error())
		} else {
			for _, v := range vnets {
				if v.Zone != settings.SDNZoneName {
					continue
				}
				if cerr := s.scrubVNet(ctx, v.VNet); cerr != nil {
					log.Printf("vnetmgr.Reset: scrub orphan vnet %s: %v (continuing)",
						v.VNet, cerr)
					report.SubnetFailures = append(report.SubnetFailures,
						fmt.Sprintf("scrub vnet %s: %s", v.VNet, cerr.Error()))
					continue
				}
				report.OrphansScrubbed++
			}
		}

		// Step 4: delete the zone. Tolerate "not found" — operator
		// may have nuked it manually already.
		if delErr := s.subnetCRUD.DeleteSDNZone(ctx, settings.SDNZoneName); delErr != nil {
			if !errors.Is(delErr, proxmox.ErrNotFound) {
				log.Printf("vnetmgr.Reset: delete zone %s: %v (continuing)",
					settings.SDNZoneName, delErr)
				report.ZoneError = delErr.Error()
			}
		} else {
			report.ZoneDeleted = settings.SDNZoneName
		}
		// Apply once at the end so the deletion takes effect on
		// every node. Tolerate apply errors — the deletes are
		// recorded in PVE config either way.
		if applyErr := s.subnetCRUD.ApplySDN(ctx); applyErr != nil {
			log.Printf("vnetmgr.Reset: apply sdn: %v (continuing)", applyErr)
			report.ApplyError = applyErr.Error()
		}
	}

	return report, nil
}

// scrubVNet tears down a PVE vnet that Nimbus's DB no longer tracks.
// Lists its subnets, deletes each, then deletes the vnet itself.
// All steps tolerate "not found" so a partially-torn-down state from
// a previous failed delete doesn't block progress.
func (s *Service) scrubVNet(ctx context.Context, vnet string) error {
	if s.subnetCRUD == nil {
		return errors.New("subnetCRUD not wired")
	}
	subnets, err := s.subnetCRUD.ListSDNSubnets(ctx, vnet)
	if err != nil && !errors.Is(err, proxmox.ErrNotFound) {
		return fmt.Errorf("list subnets: %w", err)
	}
	for _, sub := range subnets {
		// Use the PVE-assigned ID directly — it's already in the
		// zone-prefixed dash form PVE wants for DELETE. Computing
		// it from CIDR would also work but adds a fragility we
		// don't need (the list response is authoritative).
		if sub.ID == "" {
			continue
		}
		if derr := s.subnetCRUD.DeleteSDNSubnet(ctx, vnet, sub.ID); derr != nil && !errors.Is(derr, proxmox.ErrNotFound) {
			return fmt.Errorf("delete subnet %s: %w", sub.ID, derr)
		}
	}
	if derr := s.subnetCRUD.DeleteSDNVNet(ctx, vnet); derr != nil && !errors.Is(derr, proxmox.ErrNotFound) {
		return fmt.Errorf("delete vnet: %w", derr)
	}
	return nil
}

// CountUserSubnets returns the total number of Nimbus-managed
// subnets across all users. Used by the admin SaveSDN handler to
// pre-check whether a zone-name/type change would orphan subnets;
// also useful as a cheap "do I need to call Reset before
// reconfiguring?" probe from the UI.
func (s *Service) CountUserSubnets(ctx context.Context) (int64, error) {
	if s.dbConn == nil {
		return 0, errors.New("vnetmgr.CountUserSubnets: db not wired")
	}
	var n int64
	if err := s.dbConn.WithContext(ctx).Model(&db.UserSubnet{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count user_subnets: %w", err)
	}
	return n, nil
}

// ResetReport summarizes what Reset did. Surfaced to the admin UI
// so operators see exactly which subnets/zones were torn down and
// which (if any) hit errors. Errors don't fail the overall Reset —
// they're surfaced for visibility, since the alternative is to
// abort halfway through and leave PVE in a worse state than we
// found it.
type ResetReport struct {
	SubnetsDeleted int `json:"subnets_deleted"`
	// OrphansScrubbed counts PVE vnets in the zone that weren't
	// tracked in Nimbus's DB but were holding the zone hostage
	// (residue from earlier failed deletes). Reset force-cleans
	// these so the zone delete can proceed.
	OrphansScrubbed int      `json:"orphans_scrubbed"`
	SubnetFailures  []string `json:"subnet_failures,omitempty"`
	ZoneDeleted     string   `json:"zone_deleted,omitempty"`
	ZoneError       string   `json:"zone_error,omitempty"`
	ApplyError      string   `json:"apply_error,omitempty"`
}
