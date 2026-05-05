package nodemgr

import (
	"context"
	"fmt"

	"nimbus/internal/nodescore"
	"nimbus/internal/proxmox"
)

// ScoreBreakdown is the SPA-facing score for one (node, tier) pair —
// optionally with a host-aggregate filter applied. Components mirrors
// nodescore.Result.Components so the dashboard can render the per-term
// tooltip ("0.45·mem(0.85) + 0.30·cpu(0.92) + 0.25·disk(0.60) = 0.78").
//
// When Score == 0 the node was rejected; Reasons carries the
// nodescore.Reason strings so the SPA can label why ("offline",
// "insufficient_mem", "cordoned", "missing_tag", …).
type ScoreBreakdown struct {
	Score      float64            `json:"score"`
	Components map[string]float64 `json:"components,omitempty"`
	// Spec is the auto-detected node specialization (cpu/memory/balanced).
	// Informational — surfaced as a chip on the dashboard so operators
	// know how to tag the node. Does NOT drive scoring.
	Spec    string   `json:"spec"`
	Reasons []string `json:"reasons,omitempty"`
}

// NodeWithScores is the decorated view served by GET /api/nodes when
// ?include_scores=true. Embeds the regular View so existing consumers
// (Admin dashboard, etc.) that don't ask for scores keep working.
//
// Score is the (node, preview-tier) result with no host-aggregate
// filter applied — the operator's "where would a medium VM fit?"
// view. The matrix on the Nodes page consumes this; per-VM placement
// during provision computes its own scores with the operator-typed
// RequiredTags applied.
type NodeWithScores struct {
	View
	Score       *ScoreBreakdown `json:"score,omitempty"`
	PreviewTier string          `json:"preview_tier,omitempty"`
}

// ScoreClusterAtTier scores every node against the requested tier with
// no host-aggregate filter. One cluster snapshot reused across all
// nodes — cheap, single-digit ms even on big clusters. Used by the
// scoring matrix on the Nodes page.
//
// previewTier defaults to "medium" when empty (the most common
// provision size; arbitrary but sensible). Unknown tier returns an
// error so the dashboard surfaces the typo rather than silently
// scoring against the wrong size.
func (s *Service) ScoreClusterAtTier(ctx context.Context, previewTier string) (map[string]ScoreBreakdown, string, error) {
	if previewTier == "" {
		previewTier = "medium"
	}
	tier, ok := nodescore.Tiers[previewTier]
	if !ok {
		return nil, "", fmt.Errorf("unknown tier %q", previewTier)
	}

	nodes, vms, storeRows, err := s.clusterSnapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	dbNodes, err := s.loadAllRows(ctx)
	if err != nil {
		return nil, "", err
	}
	storageByNode := s.buildStorageByNode(storeRows, nodes)

	committedMem := make(map[string]uint64, len(nodes))
	vmCount := make(map[string]int, len(nodes))
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		committedMem[vm.Node] += vm.MaxMem
		vmCount[vm.Node]++
	}

	scoringNodes := make([]nodescore.Node, 0, len(nodes))
	templatesPresent := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		row := dbNodes[n.Name]
		scoringNodes = append(scoringNodes, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
			LockState: lockOrNone(row.LockState),
			Tags:      splitTags(row.Tags),
		})
		// Templates are not actually relevant for the dashboard's
		// scoring preview (the operator might be asking "how would a
		// medium VM fit?" without committing to an OS yet). Stamp
		// every node as template-present so the soft score reflects
		// pure capacity, not template-bootstrap state.
		templatesPresent[n.Name] = true
	}

	runtime := make(map[string]nodescore.NodeRuntime, len(nodes))
	for _, n := range nodes {
		runtime[n.Name] = nodescore.NodeRuntime{
			VMCount:           vmCount[n.Name],
			CommittedMemBytes: committedMem[n.Name],
		}
	}

	env := nodescore.Env{
		TemplatesPresent: templatesPresent,
		StorageByNode:    storageByNode,
	}
	decisions := nodescore.Evaluate(scoringNodes, runtime, tier, env)

	out := make(map[string]ScoreBreakdown, len(decisions))
	for _, d := range decisions {
		out[d.Node.Name] = breakdownFrom(d.Result)
	}
	return out, previewTier, nil
}

// ScoreNode computes the score for a single (node, tier) pair, optionally
// applying a host-aggregate filter (requiredTags). Used by the per-cell
// drill-down endpoint when an operator wants the full breakdown including
// rejection reasons under a hypothetical tag constraint.
func (s *Service) ScoreNode(ctx context.Context, nodeName, tierName string, requiredTags []string) (*ScoreBreakdown, error) {
	tier, ok := nodescore.Tiers[tierName]
	if !ok {
		return nil, fmt.Errorf("unknown tier %q", tierName)
	}

	nodes, vms, storeRows, err := s.clusterSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	dbNodes, err := s.loadAllRows(ctx)
	if err != nil {
		return nil, err
	}
	var target *proxmox.Node
	for i := range nodes {
		if nodes[i].Name == nodeName {
			target = &nodes[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeName)
	}

	storageByNode := s.buildStorageByNode(storeRows, nodes)
	var rt nodescore.NodeRuntime
	for _, vm := range vms {
		if vm.Template != 0 || vm.Node != nodeName {
			continue
		}
		rt.VMCount++
		rt.CommittedMemBytes += vm.MaxMem
	}

	row := dbNodes[nodeName]
	res := nodescore.Score(
		nodescore.Node{
			Name: target.Name, Status: target.Status, CPU: target.CPU,
			MaxCPU: target.MaxCPU, Mem: target.Mem, MaxMem: target.MaxMem,
			LockState: lockOrNone(row.LockState),
			Tags:      splitTags(row.Tags),
		},
		tier,
		nodescore.Env{
			TemplatesPresent: map[string]bool{nodeName: true},
			StorageByNode:    storageByNode,
			RequiredTags:     requiredTags,
		},
		rt,
	)
	out := breakdownFrom(res)
	return &out, nil
}

// breakdownFrom converts a nodescore.Result into the SPA-facing shape.
// Centralizes the reason-stringification + spec extraction so the
// cluster-wide and single-cell endpoints stay consistent.
func breakdownFrom(r nodescore.Result) ScoreBreakdown {
	out := ScoreBreakdown{
		Score:      r.Score,
		Components: r.Components,
		Spec:       string(r.Spec),
	}
	if r.Score == 0 && len(r.Reasons) > 0 {
		out.Reasons = make([]string, len(r.Reasons))
		for i, reason := range r.Reasons {
			out.Reasons[i] = string(reason)
		}
	}
	return out
}

// buildStorageByNode rolls cluster-storage rows into the per-node disk
// telemetry the scorer expects. Same logic ComputePlan uses; lifted
// here so the scoring endpoints can share it. Empty when no VMDiskStorage
// is configured on the cluster.
func (s *Service) buildStorageByNode(storeRows []proxmox.ClusterStorage, nodes []proxmox.Node) map[string]nodescore.StorageInfo {
	if s.cfg.VMDiskStorage == "" {
		return nil
	}
	out := make(map[string]nodescore.StorageInfo, len(nodes))
	var sharedInfo *nodescore.StorageInfo
	for _, st := range storeRows {
		if st.Storage != s.cfg.VMDiskStorage {
			continue
		}
		info := nodescore.StorageInfo{TotalBytes: st.Total, UsedBytes: st.Used}
		if st.Shared == 1 {
			if sharedInfo == nil {
				sharedInfo = &info
			}
			continue
		}
		out[st.Node] = info
	}
	if sharedInfo != nil {
		for _, n := range nodes {
			out[n.Name] = *sharedInfo
		}
	}
	return out
}

// splitVMTags decodes the CSV string we store on db.VM.RequiredTags into
// a slice for nodescore.Env. Whitespace-trim + drop-empties; mirrors
// splitTags used for db.Node.Tags (kept separate for legibility at
// call sites).
func splitVMTags(csv string) []string { return splitTags(csv) }
