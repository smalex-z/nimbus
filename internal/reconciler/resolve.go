package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// ResolveActor identifies who performed the resolution. The audit log
// stores this verbatim; pass user email for human actions and "system"
// for automatic resolution paths.
type ResolveActor struct {
	Email   string
	IsAdmin bool
}

// ResolveResult is the per-action outcome. Returned to the HTTP layer
// so the response can surface the freshly-minted nimbus_id (on import)
// or the imported VM row id (on adopt).
type ResolveResult struct {
	DivergenceID uint   `json:"divergence_id"`
	Action       string `json:"action"`
	NimbusID     string `json:"nimbus_id,omitempty"`
	VMRowID      uint   `json:"vm_row_id,omitempty"`
	Message      string `json:"message,omitempty"`
}

// ResolveDeps wires the side-effect surfaces resolve actions need.
// Defined here at the consumer per the "accept interfaces" idiom; the
// HTTP layer passes *proxmox.Client which satisfies the methods.
type ResolveDeps interface {
	GetVMConfig(ctx context.Context, node string, vmid int) (map[string]any, error)
	SetVMTags(ctx context.Context, node string, vmid int, tags []string) error
	SetVMDescription(ctx context.Context, node string, vmid int, description string) error
	StopVM(ctx context.Context, node string, vmid int) (string, error)
	DestroyVM(ctx context.Context, node string, vmid int) (string, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
}

// ErrDivergenceNotFound is returned when the id doesn't exist or has
// already been resolved (admin actions are idempotent at the type
// level — re-resolving a resolved row is a no-op error).
var ErrDivergenceNotFound = errors.New("reconciler: divergence not found or already resolved")

// ErrWrongDivergenceType is returned when the requested action doesn't
// apply to the row's type (e.g. Import on an orphan).
type ErrWrongDivergenceType struct {
	Have, Want string
}

func (e *ErrWrongDivergenceType) Error() string {
	return fmt.Sprintf("reconciler: this action requires divergence type %q, got %q", e.Want, e.Have)
}

// ImportExternal converts an external-unmanaged Proxmox VM into a
// Nimbus-tracked row. Mints a fresh nimbus_id, stamps PVE tag +
// description, inserts a vms row, marks the divergence resolved.
//
// Tier/OS/IP are left empty on the imported row — Nimbus doesn't have
// the information to fill them, and inventing values would lie about
// what the VM actually is. The operator can edit the row afterwards
// to set them correctly.
func (r *Reconciler) ImportExternal(ctx context.Context, divID uint, deps ResolveDeps, actor ResolveActor) (ResolveResult, error) {
	row, err := r.loadOpenDivergence(ctx, divID)
	if err != nil {
		return ResolveResult{}, err
	}
	if row.Type != TypeExternalUnmanaged {
		return ResolveResult{}, &ErrWrongDivergenceType{Have: row.Type, Want: TypeExternalUnmanaged}
	}
	if row.VMID == 0 || row.Node == "" {
		return ResolveResult{}, fmt.Errorf("import: divergence %d is missing vmid/node", divID)
	}

	cfg, err := deps.GetVMConfig(ctx, row.Node, row.VMID)
	if err != nil {
		return ResolveResult{}, fmt.Errorf("read vm config: %w", err)
	}
	existingTags := proxmox.SplitTags(stringField(cfg, "tags"))
	existingDesc := stringField(cfg, "description")
	hostname := row.Hostname
	if hostname == "" {
		hostname = stringField(cfg, "name")
	}

	newID := uuid.Must(uuid.NewV7()).String()
	tags := proxmox.MergeNimbusTags(existingTags, newID)
	desc := proxmox.MergeNimbusDescription(existingDesc, "imported", "", newID)

	if err := deps.SetVMTags(ctx, row.Node, row.VMID, tags); err != nil {
		return ResolveResult{}, fmt.Errorf("set tags: %w", err)
	}
	if err := deps.SetVMDescription(ctx, row.Node, row.VMID, desc); err != nil {
		return ResolveResult{}, fmt.Errorf("set description: %w", err)
	}

	// Insert minimal row. tier="imported" + os_template="" are
	// intentionally placeholder values — the operator edits them
	// after import. Status comes from the PVE snapshot, falling back
	// to "unknown" so we never claim a state we don't have evidence
	// for.
	status := stringField(cfg, "status")
	if status == "" {
		status = "unknown"
	}
	smbios := proxmox.ParseSMBIOSUUID(stringField(cfg, "smbios1"))
	vm := db.VM{
		VMID:       row.VMID,
		Hostname:   hostname,
		IP:         "",
		Node:       row.Node,
		Tier:       "imported",
		OSTemplate: "",
		Username:   "",
		Status:     status,
		NimbusID:   newID,
		SMBIOSID:   smbios,
	}
	if err := r.dbConn.WithContext(ctx).Create(&vm).Error; err != nil {
		return ResolveResult{}, fmt.Errorf("insert vms row: %w", err)
	}

	if err := r.markResolved(ctx, &row, actor, "imported"); err != nil {
		return ResolveResult{}, err
	}
	log.Printf("divergence resolve: import divergence=%d vmid=%d node=%s by=%s -> vm_row=%d nimbus_id=%s",
		divID, row.VMID, row.Node, actor.Email, vm.ID, newID)
	return ResolveResult{
		DivergenceID: divID,
		Action:       "import",
		NimbusID:     newID,
		VMRowID:      vm.ID,
		Message:      fmt.Sprintf("imported %s (vmid %d) as %s", hostname, row.VMID, newID),
	}, nil
}

// Adopt re-links an external-tagged-orphan with an existing local DB
// row (matched by nimbus_id). The expected case is: a row was
// soft-deleted or had its vmid wiped, the reconciler observed the
// orphan, and the admin clicks Adopt to restore the binding.
//
// When no matching DB row exists, returns an error — the operator
// should Force-delete the PVE VM (or import as a new VM, after the
// reconciler closes this divergence on the next pass).
func (r *Reconciler) Adopt(ctx context.Context, divID uint, _ ResolveDeps, actor ResolveActor) (ResolveResult, error) {
	row, err := r.loadOpenDivergence(ctx, divID)
	if err != nil {
		return ResolveResult{}, err
	}
	if row.Type != TypeExternalTaggedOrphan {
		return ResolveResult{}, &ErrWrongDivergenceType{Have: row.Type, Want: TypeExternalTaggedOrphan}
	}
	if row.NimbusID == "" {
		return ResolveResult{}, fmt.Errorf("adopt: divergence %d has no nimbus_id to match against", divID)
	}

	var vm db.VM
	err = r.dbConn.WithContext(ctx).Unscoped().Where("nimbus_id = ?", row.NimbusID).First(&vm).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ResolveResult{}, fmt.Errorf("adopt: no DB row with nimbus_id %s — use force-delete or wait for an import option", row.NimbusID)
	}
	if err != nil {
		return ResolveResult{}, fmt.Errorf("lookup vms by nimbus_id: %w", err)
	}

	patch := map[string]any{
		"vmid": row.VMID,
		"node": row.Node,
	}
	if vm.DeletedAt.Valid {
		// Restore the soft-deleted row by clearing deleted_at via Unscoped.
		patch["deleted_at"] = nil
	}
	if err := r.dbConn.WithContext(ctx).Unscoped().Model(&db.VM{}).
		Where("id = ?", vm.ID).Updates(patch).Error; err != nil {
		return ResolveResult{}, fmt.Errorf("update adopted row: %w", err)
	}

	if err := r.markResolved(ctx, &row, actor, "adopted"); err != nil {
		return ResolveResult{}, err
	}
	log.Printf("divergence resolve: adopt divergence=%d nimbus_id=%s vmid=%d by=%s -> vm_row=%d",
		divID, row.NimbusID, row.VMID, actor.Email, vm.ID)
	return ResolveResult{
		DivergenceID: divID,
		Action:       "adopt",
		NimbusID:     row.NimbusID,
		VMRowID:      vm.ID,
		Message:      fmt.Sprintf("re-linked vm row %d to vmid %d on %s", vm.ID, row.VMID, row.Node),
	}, nil
}

// MarkDeleted is the orphaned-row resolution: the local DB row is
// dropped (hard delete — there's no PVE VM to reconcile against, and
// keeping the row would just re-open the divergence on every cycle).
func (r *Reconciler) MarkDeleted(ctx context.Context, divID uint, _ ResolveDeps, actor ResolveActor) (ResolveResult, error) {
	row, err := r.loadOpenDivergence(ctx, divID)
	if err != nil {
		return ResolveResult{}, err
	}
	if row.Type != TypeOrphaned {
		return ResolveResult{}, &ErrWrongDivergenceType{Have: row.Type, Want: TypeOrphaned}
	}

	q := r.dbConn.WithContext(ctx).Unscoped().Where("vmid = ?", row.VMID)
	if row.NimbusID != "" {
		q = q.Where("nimbus_id = ?", row.NimbusID)
	}
	if err := q.Delete(&db.VM{}).Error; err != nil {
		return ResolveResult{}, fmt.Errorf("delete orphan vm row: %w", err)
	}

	if err := r.markResolved(ctx, &row, actor, "marked-deleted"); err != nil {
		return ResolveResult{}, err
	}
	log.Printf("divergence resolve: mark-deleted divergence=%d vmid=%d nimbus_id=%s by=%s",
		divID, row.VMID, row.NimbusID, actor.Email)
	return ResolveResult{
		DivergenceID: divID,
		Action:       "mark-deleted",
		NimbusID:     row.NimbusID,
		Message:      fmt.Sprintf("removed local vm row for vmid %d", row.VMID),
	}, nil
}

// ForceDelete destroys the Proxmox VM (if it still exists) AND drops
// the local DB row. Higher-blast-radius — requires admin role + the
// `force=true` query param at the HTTP layer.
//
// Applies to orphaned (DB has row, PVE doesn't — unlikely to actually
// destroy anything but safe to call) and external-tagged-orphan (PVE
// has VM with our id, DB doesn't track it — operator wants it gone).
func (r *Reconciler) ForceDelete(ctx context.Context, divID uint, deps ResolveDeps, actor ResolveActor) (ResolveResult, error) {
	row, err := r.loadOpenDivergence(ctx, divID)
	if err != nil {
		return ResolveResult{}, err
	}
	if row.Type != TypeOrphaned && row.Type != TypeExternalTaggedOrphan {
		return ResolveResult{}, fmt.Errorf("force-delete: not applicable to divergence type %q", row.Type)
	}
	if row.VMID == 0 || row.Node == "" {
		return ResolveResult{}, fmt.Errorf("force-delete: divergence %d is missing vmid/node", divID)
	}

	// Best-effort stop, then destroy. "Already gone" errors are
	// success — the goal is "this VM no longer exists on PVE".
	if upid, sErr := deps.StopVM(ctx, row.Node, row.VMID); sErr == nil {
		_ = deps.WaitForTask(ctx, row.Node, upid, time.Second)
	}
	upid, err := deps.DestroyVM(ctx, row.Node, row.VMID)
	if err != nil && !isAlreadyGone(err) {
		return ResolveResult{}, fmt.Errorf("destroy vm: %w", err)
	}
	if err == nil {
		if wErr := deps.WaitForTask(ctx, row.Node, upid, time.Second); wErr != nil {
			return ResolveResult{}, fmt.Errorf("wait destroy task: %w", wErr)
		}
	}

	// Drop any local DB row pointing at the same identity (best-effort
	// for the external-tagged-orphan case where the row might already
	// be missing).
	q := r.dbConn.WithContext(ctx).Unscoped()
	switch {
	case row.NimbusID != "":
		q = q.Where("nimbus_id = ?", row.NimbusID)
	default:
		q = q.Where("vmid = ? AND node = ?", row.VMID, row.Node)
	}
	if err := q.Delete(&db.VM{}).Error; err != nil {
		log.Printf("force-delete: drop local vm row vmid=%d: %v", row.VMID, err)
	}

	if err := r.markResolved(ctx, &row, actor, "force-deleted"); err != nil {
		return ResolveResult{}, err
	}
	log.Printf("divergence resolve: force-delete divergence=%d vmid=%d node=%s by=%s",
		divID, row.VMID, row.Node, actor.Email)
	return ResolveResult{
		DivergenceID: divID,
		Action:       "force-delete",
		NimbusID:     row.NimbusID,
		Message:      fmt.Sprintf("destroyed vmid %d on %s and dropped local row", row.VMID, row.Node),
	}, nil
}

// loadOpenDivergence fetches a divergence by id, returning
// ErrDivergenceNotFound when missing or already resolved.
func (r *Reconciler) loadOpenDivergence(ctx context.Context, id uint) (db.VMDivergence, error) {
	var row db.VMDivergence
	err := r.dbConn.WithContext(ctx).
		Where("id = ? AND resolved_at IS NULL", id).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrDivergenceNotFound
	}
	if err != nil {
		return row, fmt.Errorf("lookup divergence: %w", err)
	}
	return row, nil
}

// markResolved flips the divergence row's resolved fields. Callers
// invoke this from inside their action method so the audit trail
// (action, actor) lands atomically with the side-effect.
func (r *Reconciler) markResolved(ctx context.Context, row *db.VMDivergence, actor ResolveActor, action string) error {
	now := r.now().UTC()
	updates := map[string]any{
		"resolved_at":     &now,
		"resolved_by":     actor.Email,
		"resolved_action": action,
	}
	if actor.Email == "" {
		updates["resolved_by"] = "system"
	}
	if err := r.dbConn.WithContext(ctx).Model(row).Updates(updates).Error; err != nil {
		return fmt.Errorf("mark resolved: %w", err)
	}
	return nil
}

// stringField is a tiny convenience for reading map[string]any-shaped
// PVE config values — non-string entries become "".
func stringField(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// isAlreadyGone mirrors provision.isAlreadyGone — we copy it instead of
// importing because pulling provision into reconciler would create an
// import cycle (provision imports reconciler via PostOpReconciler).
func isAlreadyGone(err error) bool {
	if errors.Is(err, proxmox.ErrNotFound) {
		return true
	}
	var httpErr *proxmox.HTTPError
	if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
		return true
	}
	return false
}

// detailsForLog re-encodes a divergence's DetailsJSON for log lines —
// used by the action helpers to include the raw payload in the audit
// trail. Tolerates malformed payloads so a hand-edited row doesn't
// poison the action path. Currently unused but kept for #301 hook.
func detailsForLog(raw string) string {
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, _ := json.Marshal(v)
	return string(out)
}

var _ = detailsForLog
