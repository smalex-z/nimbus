package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// SyncFromProxmox scans the Proxmox cluster for existing templates whose name
// matches the bootstrap-created naming convention `<os>-template`, and writes
// any unknown ones into the node_templates table.
//
// Use cases:
//   - Existing cluster: surfaces hand-bootstrapped templates from earlier
//     versions so the new (node, os) → vmid lookup works without re-running
//     the bootstrap.
//   - Fresh deploy: no-op (no templates yet); bootstrap fills the table itself.
//   - Restart after operator manually deletes a template: the stale row is NOT
//     removed by Sync (Sync is purely additive). The next bootstrap call
//     detects the discrepancy via TemplateExists and refreshes.
//
// Returns the number of new rows inserted. Errors are surfaced — callers
// should log but not fatal-fail (Sync running at startup is best-effort).
func SyncFromProxmox(ctx context.Context, database *gorm.DB, px *proxmox.Client) (int, error) {
	clusterNodes, err := px.GetNodes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}

	// Build a set of OS keys we know about so we only import recognized ones.
	osByName := make(map[string]string, len(Catalog))
	for _, e := range Catalog {
		osByName[fmt.Sprintf("%s-template", e.OS)] = e.OS
	}

	var inserted int
	for _, n := range clusterNodes {
		if n.Status != "online" {
			continue
		}
		vms, err := px.ListVMs(ctx, n.Name)
		if err != nil {
			// One node failing doesn't block the others — keep going.
			continue
		}
		for _, vm := range vms {
			if vm.Template != 1 {
				continue
			}
			// Match exact name; ignore renamed/unknown templates.
			osKey, ok := osByName[strings.ToLower(vm.Name)]
			if !ok {
				continue
			}
			res := database.WithContext(ctx).
				Clauses(clause.OnConflict{DoNothing: true}).
				Create(&db.NodeTemplate{
					Node: n.Name,
					OS:   osKey,
					VMID: vm.VMID,
				})
			if res.Error != nil {
				return inserted, fmt.Errorf("insert (%s, %s, %d): %w",
					n.Name, osKey, vm.VMID, res.Error)
			}
			if res.RowsAffected > 0 {
				inserted++
			}
		}
	}
	return inserted, nil
}
