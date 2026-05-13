package bootstrap

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// SweepResult is the per-call summary the API + UI render. DryRun=true
// means nothing was destroyed; the Removed lists describe what *would* be.
type SweepResult struct {
	DryRun       bool        `json:"dry_run"`
	Nodes        []NodeSweep `json:"nodes"`
	TotalRemoved int         `json:"total_removed"`
}

// NodeSweep is the per-node breakdown.
type NodeSweep struct {
	Node    string            `json:"node"`
	Removed []RemovedTemplate `json:"removed,omitempty"`
	// Kept maps OS → VMID of the surviving template for that OS on this
	// node. Empty when no baked template exists for an OS (the rebuild
	// banner already covers that case).
	Kept   map[string]int `json:"kept,omitempty"`
	Errors []string       `json:"errors,omitempty"`
}

// RemovedTemplate describes one VM the sweeper destroyed (or would have
// destroyed in a dry-run). Reason classifies the cleanup so the UI can
// group rows.
type RemovedTemplate struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Reason string `json:"reason"` // duplicate | unbaked_with_baked_sibling | failed_bake_leftover
	Status string `json:"status"` // VM status at sweep time
}

const (
	reasonDuplicate     = "duplicate"
	reasonUnbakedSib    = "unbaked_with_baked_sibling"
	reasonFailedBake    = "failed_bake_leftover"
	templateNameSuffix  = "-template"
	destroyPollInterval = 2 * time.Second
)

// SweepTemplates walks every online node, finds template-shaped VMs in
// the bootstrap VMID range, and (optionally) destroys redundant ones.
// Three categories qualify:
//
//   - **duplicate** — multiple baked templates with the same OS name. We
//     keep one (preferring the VMID already recorded in node_templates;
//     otherwise lowest VMID) and destroy the rest.
//   - **unbaked_with_baked_sibling** — a template lacking NimbusBakedTag
//     whose OS has another baked template surviving on the node. The
//     bare-banner rebuild path handles unbaked templates with no sibling,
//     so we don't touch those here.
//   - **failed_bake_leftover** — a stopped non-template VM in the template
//     VMID range with a `<os>-template` name. Bake ceremony left it behind.
//
// Conservative invariants:
//   - VMs with VMID below TemplateBaseVMID are never touched.
//   - VMs whose names don't match a known catalog OS are never touched.
//   - Running non-template VMs are skipped (might be a bake in flight).
//   - Per-OS groups with zero baked templates are skipped entirely (we'd
//     leave the operator with nothing — let the rebuild banner handle it).
//
// dryRun=true reports the would-be removals without calling DestroyVM.
func (s *Service) SweepTemplates(ctx context.Context, dryRun bool) (*SweepResult, error) {
	nodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	res := &SweepResult{DryRun: dryRun}
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		ns := s.sweepNode(ctx, n.Name, dryRun)
		res.Nodes = append(res.Nodes, ns)
		res.TotalRemoved += len(ns.Removed)
	}
	sort.SliceStable(res.Nodes, func(i, j int) bool {
		return res.Nodes[i].Node < res.Nodes[j].Node
	})
	return res, nil
}

func (s *Service) sweepNode(ctx context.Context, node string, dryRun bool) NodeSweep {
	out := NodeSweep{Node: node, Kept: map[string]int{}}

	vms, err := s.px.ListVMs(ctx, node)
	if err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("list vms: %v", err))
		return out
	}

	// Group VMs by the catalog OS their name implies. Each bucket holds
	// three slices: baked templates, unbaked templates, stopped non-template
	// leftovers. Running non-template VMs are dropped here (potential
	// in-flight bake — we don't want to destroy work in progress).
	type bucket struct {
		baked   []proxmox.VMStatus
		unbaked []proxmox.VMStatus
		stopped []proxmox.VMStatus
	}
	buckets := map[string]*bucket{}

	for _, vm := range vms {
		if vm.VMID < s.cfg.TemplateBaseVMID {
			continue
		}
		os := matchTemplateName(vm.Name)
		if os == "" {
			continue
		}
		b := buckets[os]
		if b == nil {
			b = &bucket{}
			buckets[os] = b
		}
		switch {
		case vm.Template == 1:
			// TemplateExists is named misleadingly: it returns true only
			// when the template carries NimbusBakedTag. Use it as our
			// "baked vs unbaked" classifier.
			baked, terr := s.px.TemplateExists(ctx, node, vm.VMID)
			if terr != nil {
				// Treat as unbaked for safety — the operator can re-run
				// the sweep after the underlying issue clears.
				out.Errors = append(out.Errors, fmt.Sprintf("check baked vmid=%d: %v", vm.VMID, terr))
				baked = false
			}
			if baked {
				b.baked = append(b.baked, vm)
			} else {
				b.unbaked = append(b.unbaked, vm)
			}
		case vm.Status == "stopped":
			b.stopped = append(b.stopped, vm)
		}
	}

	// Read current DB pointers so we prefer the recorded VMID when it's
	// still a valid baked template.
	var rows []db.NodeTemplate
	if err := s.db.WithContext(ctx).Where("node = ?", node).Find(&rows).Error; err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("load db rows: %v", err))
	}
	rowByOS := map[string]int{}
	for _, r := range rows {
		rowByOS[r.OS] = r.VMID
	}

	// Decide per-OS what to keep and what to destroy.
	osKeys := make([]string, 0, len(buckets))
	for k := range buckets {
		osKeys = append(osKeys, k)
	}
	sort.Strings(osKeys)
	for _, os := range osKeys {
		b := buckets[os]
		if len(b.baked) == 0 {
			// No baked template for this OS on this node. Leave unbaked
			// and stopped VMs alone — the operator's rebuild path
			// (force=true bootstrap) needs to replace them; destroying
			// here would create an asymmetry where one OS is gone
			// entirely until the next reconcile cycle.
			continue
		}
		// Pick the keeper: DB pointer if it's still in the baked set,
		// otherwise lowest VMID for a stable, predictable choice.
		keepVMID := 0
		if dbVMID, ok := rowByOS[os]; ok {
			for _, t := range b.baked {
				if t.VMID == dbVMID {
					keepVMID = dbVMID
					break
				}
			}
		}
		if keepVMID == 0 {
			sort.SliceStable(b.baked, func(i, j int) bool { return b.baked[i].VMID < b.baked[j].VMID })
			keepVMID = b.baked[0].VMID
		}
		out.Kept[os] = keepVMID

		// Extra baked templates → duplicates.
		for _, t := range b.baked {
			if t.VMID == keepVMID {
				continue
			}
			out.Removed = append(out.Removed, RemovedTemplate{
				VMID: t.VMID, Name: t.Name, OS: os, Reason: reasonDuplicate, Status: "template",
			})
		}
		// Unbaked siblings with a baked keeper present → cleanup.
		for _, t := range b.unbaked {
			out.Removed = append(out.Removed, RemovedTemplate{
				VMID: t.VMID, Name: t.Name, OS: os, Reason: reasonUnbakedSib, Status: "template",
			})
		}
		// Stopped non-template VMs → failed-bake leftovers.
		for _, t := range b.stopped {
			out.Removed = append(out.Removed, RemovedTemplate{
				VMID: t.VMID, Name: t.Name, OS: os, Reason: reasonFailedBake, Status: t.Status,
			})
		}
	}

	sort.SliceStable(out.Removed, func(i, j int) bool {
		return out.Removed[i].VMID < out.Removed[j].VMID
	})

	if dryRun {
		return out
	}

	// Live pass — reconcile DB pointers first so a destroy mid-flight
	// can't leave the DB pointing at a vanished VMID.
	for os, vmid := range out.Kept {
		if rowByOS[os] == vmid {
			continue
		}
		row := db.NodeTemplate{Node: node, OS: os, VMID: vmid}
		if err := s.db.WithContext(ctx).
			Where("node = ? AND os = ?", node, os).
			Assign(map[string]any{"vmid": vmid}).
			FirstOrCreate(&row).Error; err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("update db row (%s,%s): %v", os, node, err))
		}
	}

	for _, r := range out.Removed {
		taskID, err := s.px.DestroyVM(ctx, node, r.VMID)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("destroy vmid=%d (%s): %v", r.VMID, r.Reason, err))
			continue
		}
		if taskID != "" {
			if err := s.px.WaitForTask(ctx, node, taskID, destroyPollInterval); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("wait destroy vmid=%d: %v", r.VMID, err))
			}
		}
		log.Printf("sweep-templates: destroyed vmid=%d (%s) on %s — reason=%s", r.VMID, r.Name, node, r.Reason)
	}

	return out
}

// matchTemplateName returns the catalog OS key implied by a VM name, or
// "" when the name doesn't match the `<os>-template` pattern for any
// known OS. Catalog lookup is the authoritative filter — we never act
// on names matching a freeform `something-template` heuristic, only on
// names we ourselves would have generated at bootstrap time.
func matchTemplateName(name string) string {
	if !strings.HasSuffix(name, templateNameSuffix) {
		return ""
	}
	os := strings.TrimSuffix(name, templateNameSuffix)
	if LookupOS(os) == nil {
		return ""
	}
	return os
}

// findAdoptableTemplate scans the node's existing VMs and returns the
// lowest-VMID baked template whose name matches `<os>-template`, or
// (0, false) if none exists. Used by bootstrapOne *before* creating a
// new template so a missing/stale node_templates row doesn't translate
// into a duplicate template on Proxmox.
func (s *Service) findAdoptableTemplate(ctx context.Context, node, os string) (int, bool) {
	vms, err := s.px.ListVMs(ctx, node)
	if err != nil {
		return 0, false
	}
	wantName := os + templateNameSuffix
	var candidates []int
	for _, vm := range vms {
		if vm.Template != 1 {
			continue
		}
		if vm.VMID < s.cfg.TemplateBaseVMID {
			continue
		}
		if strings.ToLower(vm.Name) != wantName {
			continue
		}
		baked, err := s.px.TemplateExists(ctx, node, vm.VMID)
		if err != nil || !baked {
			continue
		}
		candidates = append(candidates, vm.VMID)
	}
	if len(candidates) == 0 {
		return 0, false
	}
	sort.Ints(candidates)
	return candidates[0], true
}

// adoptOrInsertRow ensures the node_templates row for (node, os) points
// at vmid. If a row already exists with a different VMID, it's updated;
// if no row exists, it's created. Returns nil on success.
func (s *Service) adoptOrInsertRow(ctx context.Context, node, os string, vmid int) error {
	row := db.NodeTemplate{Node: node, OS: os, VMID: vmid}
	return s.db.WithContext(ctx).
		Where("node = ? AND os = ?", node, os).
		Assign(map[string]any{"vmid": vmid}).
		FirstOrCreate(&row).Error
}

// Compile-time check: gorm is imported for the assign/firstorcreate above.
var _ = gorm.ErrRecordNotFound
