package bootstrap

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"nimbus/internal/db"
)

// SyncFromProxmox treats Proxmox as the source of truth for templates: it
// scans every online node for baked `<os>-template` VMs and adopts them into
// node_templates, so a fresh Nimbus install pointed at an already-
// bootstrapped cluster sees the existing templates instead of an empty DB.
//
// A row is only rewritten when it doesn't already point at one of the
// node's baked candidates for that OS — a good pointer is left alone even
// if a lower-VMID sibling exists. Rows with no live counterpart are not
// deleted here; CheckTemplatesStatus / Bootstrap own that path.
//
// Returns the number of (node, OS) pairs that have a baked template.
func (s *Service) SyncFromProxmox(ctx context.Context) (int, error) {
	nodes, err := s.resolveNodes(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("resolve nodes: %w", err)
	}

	var (
		mu    sync.Mutex
		found = map[string]map[string][]int{} // node → os → sorted baked VMIDs
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelNodes)
	for _, node := range nodes {
		node := node
		g.Go(func() error {
			byOS, err := s.scanBakedTemplates(gctx, node)
			if err != nil {
				return fmt.Errorf("scan %s: %w", node, err)
			}
			mu.Lock()
			found[node] = byOS
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}

	var rows []db.NodeTemplate
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("list node_templates: %w", err)
	}
	current := map[[2]string]int{}
	for _, r := range rows {
		current[[2]string{r.Node, r.OS}] = r.VMID
	}

	total := 0
	for node, byOS := range found {
		for os, vmids := range byOS {
			total++
			if have, ok := current[[2]string{node, os}]; ok && slices.Contains(vmids, have) {
				continue
			}
			if err := s.adoptOrInsertRow(ctx, node, os, vmids[0]); err != nil {
				return 0, fmt.Errorf("adopt (%s,%s) vmid=%d: %w", node, os, vmids[0], err)
			}
			log.Printf("templates sync: adopted (%s,%s) vmid=%d from proxmox", node, os, vmids[0])
		}
	}
	return total, nil
}

// scanBakedTemplates lists one node's VMs and returns, per catalog OS, the
// sorted VMIDs of templates named `<os>-template` that carry the bake tag.
// One ListVMs plus one TemplateExists per name-matching candidate.
func (s *Service) scanBakedTemplates(ctx context.Context, node string) (map[string][]int, error) {
	vms, err := s.px.ListVMs(ctx, node)
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, vm := range vms {
		if vm.Template != 1 || vm.VMID < s.cfg.TemplateBaseVMID {
			continue
		}
		os := matchTemplateName(strings.ToLower(vm.Name))
		if os == "" {
			continue
		}
		baked, err := s.px.TemplateExists(ctx, node, vm.VMID)
		if err != nil {
			return nil, err
		}
		if baked {
			out[os] = append(out[os], vm.VMID)
		}
	}
	for _, vmids := range out {
		sort.Ints(vmids)
	}
	return out, nil
}
