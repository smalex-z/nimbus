// Package reconciler detects and surfaces divergence between the local
// vms table and the Proxmox cluster snapshot. It is the implementation
// of issue #298 — the second leg of the EPIC #296 Reconciler.
//
// Identity primitive: nimbus_id (UUIDv7) stamped at provision time into
// the PVE VM's tag + description and the local row. The reconciler
// joins on nimbus_id, NOT on (node, vmid) — vmid is reusable by PVE so
// a destroyed-and-recreated VM at the same slot would otherwise look
// like a renamed survivor. SMBIOS UUID is the secondary anchor.
//
// Divergence categories (one row per observation in vm_divergences):
//
//   - orphaned:               Nimbus DB row, no matching Proxmox VM.
//   - external-unmanaged:     Proxmox VM with no nimbus marker tag.
//   - external-tagged-orphan: Proxmox VM with nimbus_id but no DB row.
//   - vmid-mismatch:          DB row's nimbus_id matches a PVE VM at a
//     different vmid than the DB row holds
//     (slot recycle, manual restore).
//
// What the reconciler is allowed to auto-act on (per spec):
//   - status (running/stopped) on the local row.
//   - node (when a VM was migrated out-of-band).
//
// What it MUST NOT do (per spec — admin actions in #300 handle these):
//   - Auto-create a DB row for an external VM.
//   - Auto-delete a DB row for an orphan.
//
// Coexistence with provision.ReconcileVMs: the existing reconciler
// still runs the VMID-keyed rename + missed-cycles + soft-delete path
// for now. This package is the additive "divergence detector" layer.
// Once #300 admin actions ship and operators are using the new
// surface, we can retire the old auto-soft-delete behavior.
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// Divergence categories. Mirrored to the DB via the Type column.
const (
	TypeOrphaned             = "orphaned"
	TypeExternalUnmanaged    = "external-unmanaged"
	TypeExternalTaggedOrphan = "external-tagged-orphan"
	TypeVMIDMismatch         = "vmid-mismatch"
)

// ProxmoxClient is the minimal surface the reconciler needs.
//
// Defined here at the consumer per the "accept interfaces" idiom so
// tests can stub without dragging in the full *proxmox.Client.
type ProxmoxClient interface {
	GetClusterVMDetails(ctx context.Context) ([]proxmox.ClusterVMDetail, error)
}

// Reconciler diffs the local vms table against Proxmox and records
// divergences. Safe for concurrent calls; the run lock serializes
// background ticks against targeted post-op invocations.
type Reconciler struct {
	px     ProxmoxClient
	dbConn *gorm.DB
	audit  AuditReporter // optional — see SetAudit

	runMu sync.Mutex

	// now is the clock — overridable in tests so detected_at /
	// resolved_at are predictable.
	now func() time.Time
}

// New constructs a Reconciler. Wiring is straightforward — both deps
// are required. Background loops are driven by the caller via Start.
func New(px ProxmoxClient, dbConn *gorm.DB) *Reconciler {
	return &Reconciler{px: px, dbConn: dbConn, now: time.Now}
}

// SetClock injects a fake clock for tests.
func (r *Reconciler) SetClock(f func() time.Time) {
	r.now = f
}

// Report is the structured outcome of one reconcile pass. Useful in
// tests, logs, and the future targeted-reconcile HTTP response.
type Report struct {
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Healthy      int       `json:"healthy"`
	Orphaned     int       `json:"orphaned"`
	External     int       `json:"external_unmanaged"`
	TaggedOrph   int       `json:"external_tagged_orphan"`
	VMIDMismatch int       `json:"vmid_mismatch"`
	AutoSynced   int       `json:"auto_synced"`
	Opened       int       `json:"opened"`    // new divergence rows inserted
	Closed       int       `json:"closed"`    // auto-resolved (divergence vanished)
	Refreshed    int       `json:"refreshed"` // still-open divergences whose detected_at was bumped
}

// ErrEmptyClusterSnapshot is returned when the cluster snapshot pulls
// zero VMs. Acting on that would mark every DB row as orphaned —
// almost always a Proxmox API hiccup, not a real "everything gone"
// event. Mirrors the same guard in provision.ReconcileVMs.
var ErrEmptyClusterSnapshot = errors.New("reconciler: cluster snapshot is empty — refusing to act")

// Reconcile runs one pass: pulls /cluster/resources enriched with
// tags + description, diffs against vms, writes divergence rows.
// Safe to call concurrently — runMu serializes.
func (r *Reconciler) Reconcile(ctx context.Context) (Report, error) {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	rep := Report{StartedAt: r.now().UTC()}
	defer func() { rep.FinishedAt = r.now().UTC() }()

	cluster, err := r.px.GetClusterVMDetails(ctx)
	if err != nil {
		return rep, fmt.Errorf("get cluster vm details: %w", err)
	}
	if len(cluster) == 0 {
		return rep, ErrEmptyClusterSnapshot
	}

	// Index PVE-side by nimbus_id (when present) and by (node, vmid)
	// for the legacy-row case where the DB row pre-dates #297.
	type pveEntry struct {
		Detail   proxmox.ClusterVMDetail
		NimbusID string // resolved from description or tag
		IsNimbus bool   // bare "nimbus" marker present
	}
	pveByNimbusID := make(map[string]*pveEntry, len(cluster))
	pveByVMID := make(map[int]*pveEntry, len(cluster))
	for i := range cluster {
		d := cluster[i]
		_, _, descID, _ := proxmox.ParseNimbusDescription(d.Description)
		// Tag holds a short id only — description is the only carrier
		// of the full uuid. Fall back to the short tag for the
		// is-this-ours signal when no description marker is present.
		isNimbus := proxmox.HasNimbusTag(d.Tags) || proxmox.ParseNimbusIDFromTags(d.Tags) != ""
		entry := &pveEntry{Detail: d, NimbusID: descID, IsNimbus: isNimbus}
		if descID != "" {
			pveByNimbusID[descID] = entry
		}
		pveByVMID[d.VMID] = entry
	}

	var rows []db.VM
	if err := r.dbConn.WithContext(ctx).
		Where("vmid > 0").
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return rep, fmt.Errorf("list vms: %w", err)
	}

	matchedPVE := make(map[int]bool, len(rows)) // pve vmids we accounted for via DB rows

	for _, vm := range rows {
		var pveHit *pveEntry
		switch {
		case vm.NimbusID != "" && pveByNimbusID[vm.NimbusID] != nil:
			pveHit = pveByNimbusID[vm.NimbusID]
		case pveByVMID[vm.VMID] != nil && pveByVMID[vm.VMID].NimbusID == "":
			// Legacy fallback: the DB row has no nimbus_id and PVE
			// also doesn't (the backfill will fix this on next pass).
			// Treat the (node, vmid) match as healthy so we don't
			// thrash an orphan row.
			pveHit = pveByVMID[vm.VMID]
		}

		if pveHit == nil {
			// DB row with no live PVE counterpart — orphaned.
			if err := r.recordDivergence(ctx, vm.NimbusID, vm.VMID, vm.Node, vm.Hostname, TypeOrphaned, map[string]any{
				"reason":    "no matching PVE VM by nimbus_id or vmid",
				"db_row_id": vm.ID,
				"db_status": vm.Status,
			}, &rep); err != nil {
				log.Printf("reconciler: record orphaned vmid=%d: %v", vm.VMID, err)
			}
			rep.Orphaned++
			continue
		}
		matchedPVE[pveHit.Detail.VMID] = true

		// vmid-mismatch: nimbus_id matches but vmid changed (slot
		// recycle). Worth surfacing so the operator can investigate
		// the stale row even though node + status can be auto-synced.
		if pveHit.Detail.VMID != vm.VMID {
			if err := r.recordDivergence(ctx, vm.NimbusID, pveHit.Detail.VMID, pveHit.Detail.Node, pveHit.Detail.Name, TypeVMIDMismatch, map[string]any{
				"db_vmid":      vm.VMID,
				"pve_vmid":     pveHit.Detail.VMID,
				"db_row_id":    vm.ID,
				"db_hostname":  vm.Hostname,
				"pve_hostname": pveHit.Detail.Name,
			}, &rep); err != nil {
				log.Printf("reconciler: record vmid-mismatch nimbus_id=%s: %v", vm.NimbusID, err)
			}
			rep.VMIDMismatch++
			continue
		}

		// Healthy match. Auto-sync the two safe fields the spec
		// allows: status + node. Anything else (hostname, tier, ip)
		// requires admin intervention.
		if r.autoSyncSafeMeta(ctx, vm, pveHit.Detail) {
			rep.AutoSynced++
		}
		// Clear any open divergence rows for this nimbus_id / vmid —
		// the underlying cause vanished, so close them as
		// auto-cleared. Keeps the UI's "open" list lean.
		if n := r.autoCloseResolved(ctx, vm.NimbusID, vm.VMID, vm.Node); n > 0 {
			rep.Closed += n
		}
		rep.Healthy++
	}

	// Second pass over PVE: anything we didn't touch above is either
	// external-unmanaged or external-tagged-orphan.
	for _, entry := range pveByVMID {
		if matchedPVE[entry.Detail.VMID] {
			continue
		}
		details := map[string]any{
			"vmid":    entry.Detail.VMID,
			"node":    entry.Detail.Node,
			"name":    entry.Detail.Name,
			"status":  entry.Detail.Status,
			"tags":    entry.Detail.Tags,
			"os_type": entry.Detail.OSType,
		}
		switch {
		case entry.NimbusID != "":
			if err := r.recordDivergence(ctx, entry.NimbusID, entry.Detail.VMID, entry.Detail.Node, entry.Detail.Name, TypeExternalTaggedOrphan, details, &rep); err != nil {
				log.Printf("reconciler: record external-tagged-orphan vmid=%d: %v", entry.Detail.VMID, err)
			}
			rep.TaggedOrph++
		case !entry.IsNimbus:
			if err := r.recordDivergence(ctx, "", entry.Detail.VMID, entry.Detail.Node, entry.Detail.Name, TypeExternalUnmanaged, details, &rep); err != nil {
				log.Printf("reconciler: record external-unmanaged vmid=%d: %v", entry.Detail.VMID, err)
			}
			rep.External++
		default:
			// Nimbus-marker tag present but no description id and no DB row.
			// Treat as tagged-orphan with no nimbus_id — surfaces the
			// "legacy-tagged VM with no record on our side" case.
			if err := r.recordDivergence(ctx, "", entry.Detail.VMID, entry.Detail.Node, entry.Detail.Name, TypeExternalTaggedOrphan, details, &rep); err != nil {
				log.Printf("reconciler: record external-tagged-orphan-no-id vmid=%d: %v", entry.Detail.VMID, err)
			}
			rep.TaggedOrph++
		}
	}

	log.Printf("reconciler: %d healthy, %d orphaned, %d external, %d tagged-orphan, %d vmid-mismatch, %d auto-synced, %d opened, %d closed, %d refreshed (cycle %s)",
		rep.Healthy, rep.Orphaned, rep.External, rep.TaggedOrph, rep.VMIDMismatch, rep.AutoSynced, rep.Opened, rep.Closed, rep.Refreshed, rep.FinishedAt.Sub(rep.StartedAt))
	return rep, nil
}

// recordDivergence upserts a divergence row by (type, nimbus_id, vmid,
// node) so a long-standing divergence updates one row instead of
// inserting every cycle. Report counters are bumped in place so a
// caller can tell new (Opened) vs. refreshed (Refreshed) rows apart.
func (r *Reconciler) recordDivergence(ctx context.Context, nimbusID string, vmid int, node, hostname, dtype string, details map[string]any, rep *Report) error {
	now := r.now().UTC()
	payload, _ := json.Marshal(details)

	var existing db.VMDivergence
	err := r.dbConn.WithContext(ctx).
		Where("type = ? AND nimbus_id = ? AND vmid = ? AND node = ? AND resolved_at IS NULL",
			dtype, nimbusID, vmid, node).
		First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		row := db.VMDivergence{
			Type:            dtype,
			NimbusID:        nimbusID,
			VMID:            vmid,
			Node:            node,
			Hostname:        hostname,
			DetectedAt:      now,
			FirstDetectedAt: now,
			DetailsJSON:     string(payload),
		}
		if err := r.dbConn.WithContext(ctx).Create(&row).Error; err != nil {
			return fmt.Errorf("insert divergence: %w", err)
		}
		// Structured log on first observation — operators want a
		// notification, not a re-statement, for ones already known.
		log.Printf("divergence: type=%s nimbus_id=%s vmid=%d node=%s hostname=%q action_hint=%s",
			dtype, nimbusID, vmid, node, hostname, actionHint(dtype))
		// Emit one audit event per opened divergence. Actor stays
		// empty (audit.Service stamps "system" via ctx absence).
		r.emit(ctx, AuditEvent{
			Action:      "divergence.opened",
			TargetType:  "divergence",
			TargetID:    fmt.Sprintf("%d", row.ID),
			TargetLabel: hostname,
			Details:     details,
			Success:     true,
		})
		rep.Opened++
	case err != nil:
		return fmt.Errorf("lookup existing divergence: %w", err)
	default:
		updates := map[string]any{
			"detected_at":  now,
			"hostname":     hostname,
			"details_json": string(payload),
		}
		if err := r.dbConn.WithContext(ctx).Model(&existing).Updates(updates).Error; err != nil {
			return fmt.Errorf("refresh divergence: %w", err)
		}
		rep.Refreshed++
	}
	return nil
}

// autoSyncSafeMeta updates the two fields the spec lets us auto-sync:
// status and node. Returns true when at least one column was written.
// Anything else (hostname, tier, ip) requires an admin action.
func (r *Reconciler) autoSyncSafeMeta(ctx context.Context, vm db.VM, d proxmox.ClusterVMDetail) bool {
	patch := map[string]any{}
	if d.Node != "" && d.Node != vm.Node {
		patch["node"] = d.Node
	}
	if d.Status != "" && d.Status != vm.Status {
		patch["status"] = d.Status
	}
	if len(patch) == 0 {
		return false
	}
	if err := r.dbConn.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).Updates(patch).Error; err != nil {
		log.Printf("reconciler: auto-sync vmid=%d row=%d: %v", vm.VMID, vm.ID, err)
		return false
	}
	return true
}

// autoCloseResolved marks any still-open divergence rows for this
// nimbus_id (or vmid+node fallback) as system-resolved. Used when the
// reconciler observes the underlying cause is gone (e.g. an orphan
// became real on PVE again, or an external-tagged VM was imported).
// Returns the count closed.
func (r *Reconciler) autoCloseResolved(ctx context.Context, nimbusID string, vmid int, node string) int {
	now := r.now().UTC()
	q := r.dbConn.WithContext(ctx).Model(&db.VMDivergence{}).
		Where("resolved_at IS NULL")
	switch {
	case nimbusID != "":
		q = q.Where("nimbus_id = ?", nimbusID)
	default:
		q = q.Where("vmid = ? AND node = ?", vmid, node)
	}
	res := q.Updates(map[string]any{
		"resolved_at":     &now,
		"resolved_by":     "system",
		"resolved_action": "auto-cleared",
	})
	if res.Error != nil {
		log.Printf("reconciler: auto-close divergences nimbus_id=%s vmid=%d: %v", nimbusID, vmid, res.Error)
		return 0
	}
	if res.RowsAffected > 0 {
		r.emit(ctx, AuditEvent{
			Action:      "divergence.auto_cleared",
			TargetType:  "divergence",
			TargetLabel: nimbusID,
			Details: map[string]any{
				"nimbus_id": nimbusID,
				"vmid":      vmid,
				"node":      node,
				"count":     res.RowsAffected,
			},
			Success: true,
		})
	}
	return int(res.RowsAffected)
}

// actionHint returns a one-word suggestion for what an admin should
// do with this divergence type. Surfaces in logs and the UI.
func actionHint(dtype string) string {
	switch dtype {
	case TypeOrphaned:
		return "mark-deleted-or-restore"
	case TypeExternalUnmanaged:
		return "import-or-ignore"
	case TypeExternalTaggedOrphan:
		return "adopt-or-force-delete"
	case TypeVMIDMismatch:
		return "investigate-recycle"
	}
	return "investigate"
}
