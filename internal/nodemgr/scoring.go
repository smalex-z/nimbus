package nodemgr

import (
	"context"
	"fmt"

	"nimbus/internal/nodescore"
	"nimbus/internal/proxmox"
)

// ScoreBreakdown is the SPA-facing score for one (node, tier, workload)
// combination. Components mirrors nodescore.Result.Components so the
// dashboard can render the per-term tooltip ("0.30·mem(0.85) + …").
//
// When Score == 0 the node was rejected; Reasons carries the
// nodescore.Reason strings so the SPA can label why ("offline",
// "insufficient_mem", "cordoned", …).
type ScoreBreakdown struct {
	Score      float64            `json:"score"`
	Components map[string]float64 `json:"components,omitempty"`
	Spec       string             `json:"spec"`
	SpecMatch  bool               `json:"spec_match"`
	Reasons    []string           `json:"reasons,omitempty"`
}

// NodeScores is one node's score against every workload type. Keys are
// the canonical workload strings (web/database/compute/balanced) so the
// JSON payload reads naturally — the dashboard's matrix iterates over a
// fixed column order without hard-coding workload names.
type NodeScores map[string]ScoreBreakdown

// NodeWithScores is the decorated view served by GET /api/nodes when
// ?include_scores=true. Embeds the regular View so existing consumers
// (Admin dashboard, etc.) that don't ask for scores keep working.
type NodeWithScores struct {
	View
	Scores      NodeScores `json:"scores,omitempty"`
	PreviewTier string     `json:"preview_tier,omitempty"`
}

// ScoreAllWorkloads computes scores for every (node, workload) under
// the requested preview tier. One cluster snapshot is fetched and
// reused across all combinations — cheap, single-digit ms even on
// big clusters.
//
// previewTier defaults to "medium" when empty (the most common
// provision size; arbitrary but sensible). Unknown tier returns an
// error so the dashboard surfaces the typo rather than silently
// scoring against the wrong size.
func (s *Service) ScoreAllWorkloads(ctx context.Context, previewTier string) (map[string]NodeScores, string, error) {
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

	// Pre-aggregate per-node committed mem + VM count once.
	committedMem := make(map[string]uint64, len(nodes))
	vmCount := make(map[string]int, len(nodes))
	for _, vm := range vms {
		if vm.Template != 0 {
			continue
		}
		committedMem[vm.Node] += vm.MaxMem
		vmCount[vm.Node]++
	}

	// Build the candidate set once. The same scoringNodes slice is
	// reused per workload — only env.Workload changes.
	scoringNodes := make([]nodescore.Node, 0, len(nodes))
	templatesPresent := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		row := dbNodes[n.Name]
		scoringNodes = append(scoringNodes, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
			LockState: lockOrNone(row.LockState),
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

	out := make(map[string]NodeScores, len(nodes))
	for _, n := range scoringNodes {
		out[n.Name] = make(NodeScores, len(nodescore.AllWorkloads))
	}

	for _, w := range nodescore.AllWorkloads {
		env := nodescore.Env{
			Excluded:         nil, // dashboard preview ignores cfg.ExcludedNodes by design
			TemplatesPresent: templatesPresent,
			StorageByNode:    storageByNode,
			MemBufferMiB:     0, // use defaults
			CPULoadFactor:    0,
			Workload:         w,
		}
		decisions := nodescore.Evaluate(scoringNodes, runtime, tier, env)
		for _, d := range decisions {
			out[d.Node.Name][string(w)] = breakdownFrom(d.Result)
		}
	}

	return out, previewTier, nil
}

// ScoreOne computes the score for a single (node, tier, workload). Used
// by the per-cell drill-down endpoint when an operator hovers a
// dashboard cell and wants the full breakdown.
//
// Returns a populated ScoreBreakdown including Reasons when the score
// is 0 (rejected) — the SPA renders the rejection chips alongside the
// numeric breakdown.
func (s *Service) ScoreOne(ctx context.Context, nodeName, tierName string, workload nodescore.WorkloadType) (*ScoreBreakdown, error) {
	tier, ok := nodescore.Tiers[tierName]
	if !ok {
		return nil, fmt.Errorf("unknown tier %q", tierName)
	}
	if workload == "" {
		workload = nodescore.WorkloadBalanced
	}
	if _, ok := nodescore.Profiles[workload]; !ok {
		return nil, fmt.Errorf("unknown workload %q", workload)
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
		},
		tier,
		nodescore.Env{
			TemplatesPresent: map[string]bool{nodeName: true},
			StorageByNode:    storageByNode,
			Workload:         workload,
		},
		rt,
	)
	out := breakdownFrom(res)
	return &out, nil
}

// breakdownFrom converts a nodescore.Result into the SPA-facing shape.
// Centralizes the reason-stringification + spec-match extraction so the
// matrix and single-cell endpoints stay consistent.
func breakdownFrom(r nodescore.Result) ScoreBreakdown {
	out := ScoreBreakdown{
		Score:      r.Score,
		Components: r.Components,
		Spec:       string(r.Spec),
	}
	if r.Components != nil {
		out.SpecMatch = r.Components["spec_match"] > 0
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

// ProfileView is the workload Profile flattened for JSON serving via
// /api/scoring/profiles. Identical to nodescore.Profile minus the
// internal types.
type ProfileView struct {
	MemWeight      float64 `json:"mem_weight"`
	CPUWeight      float64 `json:"cpu_weight"`
	DiskWeight     float64 `json:"disk_weight"`
	SpecBonus      float64 `json:"spec_bonus"`
	SpecPreference string  `json:"spec_preference"`
}

// AllProfiles returns the workload→Profile map for the dashboard's
// formula explanations. Static data; trivial wrapper over the package
// constant so future weight tuning is a backend-only change.
func AllProfiles() map[string]ProfileView {
	out := make(map[string]ProfileView, len(nodescore.Profiles))
	for w, p := range nodescore.Profiles {
		out[string(w)] = ProfileView{
			MemWeight:      p.MemWeight,
			CPUWeight:      p.CPUWeight,
			DiskWeight:     p.DiskWeight,
			SpecBonus:      p.SpecBonus,
			SpecPreference: string(p.SpecPreference),
		}
	}
	return out
}
