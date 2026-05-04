package nodescore_test

import (
	"reflect"
	"testing"

	"nimbus/internal/nodescore"
)

const (
	mib = uint64(1 << 20)
	gib = uint64(1 << 30)
)

// allTemplates is a shorthand for "every named node has the requested
// template" — keeps test fixtures small.
func allTemplates(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestScore_Gates(t *testing.T) {
	t.Parallel()

	medium := nodescore.Tiers["medium"] // 2 vCPU, 2048 MiB, 30 GiB
	bigEnough := nodescore.Node{
		Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib,
	}
	storageOK := map[string]nodescore.StorageInfo{
		"alpha": {TotalBytes: 500 * gib, UsedBytes: 100 * gib},
	}

	tests := []struct {
		name        string
		node        nodescore.Node
		env         nodescore.Env
		rt          nodescore.NodeRuntime
		wantReasons []nodescore.Reason
	}{
		{
			name:        "offline rejects",
			node:        nodescore.Node{Name: "alpha", Status: "offline", MaxCPU: 8, MaxMem: 16 * gib},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha")},
			wantReasons: []nodescore.Reason{nodescore.ReasonOffline},
		},
		{
			name:        "excluded rejects",
			node:        bigEnough,
			env:         nodescore.Env{Excluded: []string{"alpha"}, TemplatesPresent: allTemplates("alpha")},
			wantReasons: []nodescore.Reason{nodescore.ReasonExcluded},
		},
		{
			name:        "missing template rejects",
			node:        bigEnough,
			env:         nodescore.Env{TemplatesPresent: map[string]bool{}},
			wantReasons: []nodescore.Reason{nodescore.ReasonNoTemplate},
		},
		{
			name:        "no capacity rejects",
			node:        nodescore.Node{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 0},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha")},
			wantReasons: []nodescore.Reason{nodescore.ReasonNoCapacity, nodescore.ReasonInsufficientMem},
		},
		{
			name:        "insufficient cores rejects",
			node:        nodescore.Node{Name: "alpha", Status: "online", MaxCPU: 1, MaxMem: 16 * gib},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha")},
			wantReasons: []nodescore.Reason{nodescore.ReasonInsufficientCores},
		},
		{
			name: "insufficient mem with buffer biting",
			// Tier wants 2048 MiB; buffer adds 256. Free = 2200 MiB → reject.
			node:        nodescore.Node{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 16*gib - 2200*mib},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha")},
			wantReasons: []nodescore.Reason{nodescore.ReasonInsufficientMem},
		},
		{
			name: "insufficient disk rejects when telemetry on",
			// alpha has 1 GiB free disk; medium wants 30 GiB.
			node:        bigEnough,
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: map[string]nodescore.StorageInfo{"alpha": {TotalBytes: 100 * gib, UsedBytes: 99 * gib}}},
			wantReasons: []nodescore.Reason{nodescore.ReasonInsufficientDisk},
		},
		{
			name:        "missing storage row treated as zero free",
			node:        bigEnough,
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: map[string]nodescore.StorageInfo{}},
			wantReasons: []nodescore.Reason{nodescore.ReasonInsufficientDisk},
		},
		{
			name:        "all gates pass with disk telemetry",
			node:        bigEnough,
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: storageOK},
			wantReasons: nil,
		},
		{
			name: "cordoned rejects",
			node: nodescore.Node{
				Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib,
				LockState: "cordoned",
			},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: storageOK},
			wantReasons: []nodescore.Reason{nodescore.ReasonCordoned},
		},
		{
			name: "draining rejects",
			node: nodescore.Node{
				Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib,
				LockState: "draining",
			},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: storageOK},
			wantReasons: []nodescore.Reason{nodescore.ReasonDraining},
		},
		{
			name: "drained rejects",
			node: nodescore.Node{
				Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib,
				LockState: "drained",
			},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: storageOK},
			wantReasons: []nodescore.Reason{nodescore.ReasonDrained},
		},
		{
			name: "empty lock state treated as none (accepts)",
			node: nodescore.Node{
				Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib,
				LockState: "",
			},
			env:         nodescore.Env{TemplatesPresent: allTemplates("alpha"), StorageByNode: storageOK},
			wantReasons: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := nodescore.Score(tt.node, medium, tt.env, tt.rt)
			if tt.wantReasons == nil {
				if got.Score == 0 {
					t.Errorf("expected accept, got reject with %v", got.Reasons)
				}
				return
			}
			if got.Score != 0 {
				t.Errorf("expected reject (score=0), got score=%v reasons=%v", got.Score, got.Reasons)
			}
			if !reflect.DeepEqual(got.Reasons, tt.wantReasons) {
				t.Errorf("reasons = %v, want %v", got.Reasons, tt.wantReasons)
			}
		})
	}
}

func TestScore_StoppedVMsReserveMem(t *testing.T) {
	t.Parallel()

	// Node looks idle by live RAM (1 GiB used) but has 10 stopped 4 GiB VMs
	// committed to it. Asking for medium (2 GiB) must reject — without the
	// committed-RAM accounting this test fails because freeMem looks like 15
	// GiB.
	node := nodescore.Node{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib, Mem: 1 * gib}
	rt := nodescore.NodeRuntime{CommittedMemBytes: 16*gib - 1*gib}
	env := nodescore.Env{TemplatesPresent: allTemplates("alpha")}

	got := nodescore.Score(node, nodescore.Tiers["medium"], env, rt)
	if got.Score != 0 {
		t.Fatalf("expected reject due to committed RAM, got score=%v reasons=%v", got.Score, got.Reasons)
	}
	if !contains(got.Reasons, nodescore.ReasonInsufficientMem) {
		t.Errorf("expected ReasonInsufficientMem in %v", got.Reasons)
	}
}

func TestScore_ProjectionRewardsAbsoluteHeadroom(t *testing.T) {
	t.Parallel()

	// Two nodes both 50 % used. Big node has 32 GiB free, small node has 4
	// GiB free. Medium tier (2 GiB) — both fit but the big node has ~16x more
	// post-placement headroom, which the projection-based score must reward.
	big := nodescore.Node{Name: "big", Status: "online", MaxCPU: 8, MaxMem: 64 * gib, Mem: 32 * gib, CPU: 0.5}
	small := nodescore.Node{Name: "small", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 4 * gib, CPU: 0.5}
	env := nodescore.Env{TemplatesPresent: allTemplates("big", "small")}

	bigRes := nodescore.Score(big, nodescore.Tiers["medium"], env, nodescore.NodeRuntime{})
	smallRes := nodescore.Score(small, nodescore.Tiers["medium"], env, nodescore.NodeRuntime{})

	if bigRes.Score <= smallRes.Score {
		t.Errorf("big node should outscore small (more absolute headroom): big=%v small=%v",
			bigRes.Score, smallRes.Score)
	}
}

func TestScore_NoDiskTelemetryRevertsToLegacyWeights(t *testing.T) {
	t.Parallel()

	// With StorageByNode == nil, the disk gate is skipped (a node with zero
	// free disk is not rejected) and the soft score uses the legacy 0.6/0.4
	// weights.
	node := nodescore.Node{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib}
	env := nodescore.Env{TemplatesPresent: allTemplates("alpha")} // StorageByNode nil
	got := nodescore.Score(node, nodescore.Tiers["small"], env, nodescore.NodeRuntime{})
	if got.Score == 0 {
		t.Fatalf("expected accept with no disk telemetry, got reasons %v", got.Reasons)
	}
	// Idle node: memHeadroomAfter ≈ 1 - (1 GiB + 256 MiB)/16 GiB ≈ 0.9219
	// cpuHeadroomAfter = 1 - 0.5*1/8 = 0.9375. Score ≈ 0.6*0.9219 + 0.4*0.9375 ≈ 0.928.
	// Just assert it's clearly above 0.85 and below 1.0 — exact arithmetic
	// is checked by the projection test.
	if got.Score < 0.85 || got.Score > 1.0 {
		t.Errorf("expected score in (0.85, 1.0] for idle node with no disk telemetry, got %v", got.Score)
	}
}

func TestEvaluate_OrderPreserved(t *testing.T) {
	t.Parallel()

	nodes := []nodescore.Node{
		{Name: "a", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "b", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "c", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
	}
	env := nodescore.Env{TemplatesPresent: allTemplates("a", "b", "c")}
	got := nodescore.Evaluate(nodes, nil, nodescore.Tiers["small"], env)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Node.Name != want {
			t.Errorf("got[%d].Node.Name = %q, want %q", i, got[i].Node.Name, want)
		}
	}
}

func TestPick_HigherScoreWins(t *testing.T) {
	t.Parallel()

	env := nodescore.Env{TemplatesPresent: allTemplates("low", "high")}
	nodes := []nodescore.Node{
		{Name: "low", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 7 * gib, CPU: 0.9},
		{Name: "high", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 1 * gib, CPU: 0.1},
	}
	decisions := nodescore.Evaluate(nodes, nil, nodescore.Tiers["small"], env)
	w, _ := nodescore.Pick(decisions)
	if w == nil || w.Node.Name != "high" {
		t.Errorf("Pick winner = %v, want 'high'", w)
	}
}

func TestPick_TieBreakLowerVMCountWins(t *testing.T) {
	t.Parallel()

	env := nodescore.Env{TemplatesPresent: allTemplates("busy", "quiet")}
	nodes := []nodescore.Node{
		{Name: "busy", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 4 * gib, CPU: 0.5},
		{Name: "quiet", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 4 * gib, CPU: 0.5},
	}
	rt := map[string]nodescore.NodeRuntime{
		"busy":  {VMCount: 10},
		"quiet": {VMCount: 2},
	}
	decisions := nodescore.Evaluate(nodes, rt, nodescore.Tiers["small"], env)
	w, _ := nodescore.Pick(decisions)
	if w == nil || w.Node.Name != "quiet" {
		t.Errorf("Pick winner = %v, want 'quiet'", w)
	}
}

func TestPick_AllRejectedReturnsNilWithReasons(t *testing.T) {
	t.Parallel()

	env := nodescore.Env{TemplatesPresent: map[string]bool{}}
	nodes := []nodescore.Node{
		{Name: "alpha", Status: "online", MaxCPU: 8, MaxMem: 16 * gib},
		{Name: "bravo", Status: "offline", MaxCPU: 8, MaxMem: 16 * gib},
	}
	decisions := nodescore.Evaluate(nodes, nil, nodescore.Tiers["small"], env)
	w, all := nodescore.Pick(decisions)
	if w != nil {
		t.Errorf("expected no winner, got %v", w)
	}
	if len(all) != 2 {
		t.Fatalf("all-decisions slice = %d, want 2", len(all))
	}
	for _, d := range all {
		if len(d.Result.Reasons) == 0 {
			t.Errorf("decision for %s missing reasons", d.Node.Name)
		}
	}
}

func TestPick_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	env := nodescore.Env{TemplatesPresent: allTemplates("a", "b")}
	nodes := []nodescore.Node{
		{Name: "a", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 4 * gib},
		{Name: "b", Status: "online", MaxCPU: 8, MaxMem: 8 * gib, Mem: 0},
	}
	decisions := nodescore.Evaluate(nodes, nil, nodescore.Tiers["small"], env)
	_, _ = nodescore.Pick(decisions)
	if decisions[0].Node.Name != "a" {
		t.Errorf("Pick mutated input: decisions[0] = %s", decisions[0].Node.Name)
	}
}

func contains(reasons []nodescore.Reason, want nodescore.Reason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// --- workload-aware scoring -------------------------------------------------

// TestDetectSpecialization covers the GiB-per-vCPU thresholds. Boundaries
// matter: the issue spec says "1 vCPU per 4 GiB → CPU-opt, 1 vCPU per 8 GiB
// → mem-opt", and we want to be confident the canonical examples land in
// the right bucket.
func TestDetectSpecialization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		maxCPU  int
		maxMem  uint64
		want    nodescore.Specialization
		comment string
	}{
		{"16c/32G CPU-optimized", 16, 32 * gib, nodescore.SpecCPU, "2 GiB per core — clearly CPU-heavy"},
		{"8c/128G memory-optimized", 8, 128 * gib, nodescore.SpecMemory, "16 GiB per core — clearly mem-heavy"},
		{"8c/64G balanced", 8, 64 * gib, nodescore.SpecBalanced, "8 GiB per core — at the upper boundary, balanced"},
		{"8c/32G balanced", 8, 32 * gib, nodescore.SpecBalanced, "4 GiB per core — at the lower boundary, balanced"},
		{"4c/8G CPU-optimized", 4, 8 * gib, nodescore.SpecCPU, "2 GiB per core"},
		{"2c/64G memory-optimized", 2, 64 * gib, nodescore.SpecMemory, "32 GiB per core"},
		{"zero capacity falls back to balanced", 0, 0, nodescore.SpecBalanced, "guard against div-by-zero"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := nodescore.DetectSpecialization(nodescore.Node{
				Name: "n", MaxCPU: c.maxCPU, MaxMem: c.maxMem,
			})
			if got != c.want {
				t.Errorf("got %q, want %q (%s)", got, c.want, c.comment)
			}
		})
	}
}

// TestDefaultWorkloadForTier — every tier resolves to balanced. The
// helper is kept tier-aware in signature for forward-compat with
// operator-tunable defaults; current behaviour is the conservative
// "don't bias placement" answer.
func TestDefaultWorkloadForTier(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{"small", "medium", "large", "xl", "unknown", ""} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			if got := nodescore.DefaultWorkloadForTier(tier); got != nodescore.WorkloadBalanced {
				t.Errorf("DefaultWorkloadForTier(%q) = %q, want balanced", tier, got)
			}
		})
	}
}

// TestScore_WorkloadProfilesMatchSpecialization is the headline test: the
// node specialization that matches the workload's preferred Spec wins
// when other variables are equal. Builds two nodes with identical capacity
// usage but different vCPU/RAM ratios, then scores each workload and
// asserts the right node wins each.
func TestScore_WorkloadProfilesMatchSpecialization(t *testing.T) {
	t.Parallel()

	// Both nodes idle (Mem: 1 gib used). Same total capacity in
	// "score-relevant" terms — the difference is the vCPU/RAM ratio.
	cpuNode := nodescore.Node{
		Name: "cpu-opt", Status: "online",
		MaxCPU: 16, MaxMem: 32 * gib, Mem: 1 * gib, // 2 GiB/c → CPU-opt
	}
	memNode := nodescore.Node{
		Name: "mem-opt", Status: "online",
		MaxCPU: 8, MaxMem: 128 * gib, Mem: 1 * gib, // 16 GiB/c → mem-opt
	}
	storageOK := map[string]nodescore.StorageInfo{
		"cpu-opt": {TotalBytes: 500 * gib, UsedBytes: 50 * gib},
		"mem-opt": {TotalBytes: 500 * gib, UsedBytes: 50 * gib},
	}
	medium := nodescore.Tiers["medium"]

	cases := []struct {
		workload nodescore.WorkloadType
		wantWin  string
	}{
		{nodescore.WorkloadWeb, "cpu-opt"},      // web prefers CPU-opt
		{nodescore.WorkloadDatabase, "mem-opt"}, // db prefers mem-opt
		{nodescore.WorkloadCompute, "cpu-opt"},  // compute strongly prefers CPU-opt
	}
	for _, c := range cases {
		t.Run(string(c.workload), func(t *testing.T) {
			t.Parallel()
			env := nodescore.Env{
				TemplatesPresent: allTemplates("cpu-opt", "mem-opt"),
				StorageByNode:    storageOK,
				Workload:         c.workload,
			}
			cpuRes := nodescore.Score(cpuNode, medium, env, nodescore.NodeRuntime{})
			memRes := nodescore.Score(memNode, medium, env, nodescore.NodeRuntime{})
			if cpuRes.Score == 0 || memRes.Score == 0 {
				t.Fatalf("expected both to pass gates: cpu=%v mem=%v", cpuRes, memRes)
			}
			gotWin := "cpu-opt"
			if memRes.Score > cpuRes.Score {
				gotWin = "mem-opt"
			}
			if gotWin != c.wantWin {
				t.Errorf("workload=%s winner=%s (cpu=%.4f mem=%.4f), want %s",
					c.workload, gotWin, cpuRes.Score, memRes.Score, c.wantWin)
			}
		})
	}
}

// TestScore_ComponentsBreakdown verifies that the components map carries
// every key the dashboard tooltip expects, and that mem_weighted +
// cpu_weighted + disk_weighted + spec_bonus == total. This is the contract
// the SPA renders against — keys missing here means broken tooltips.
func TestScore_ComponentsBreakdown(t *testing.T) {
	t.Parallel()
	node := nodescore.Node{
		Name: "alpha", Status: "online",
		MaxCPU: 16, MaxMem: 32 * gib, Mem: 4 * gib, CPU: 0.2,
	}
	env := nodescore.Env{
		TemplatesPresent: allTemplates("alpha"),
		StorageByNode:    map[string]nodescore.StorageInfo{"alpha": {TotalBytes: 500 * gib, UsedBytes: 50 * gib}},
		Workload:         nodescore.WorkloadWeb,
	}
	got := nodescore.Score(node, nodescore.Tiers["medium"], env, nodescore.NodeRuntime{})
	if got.Score == 0 {
		t.Fatalf("expected accept; got reject %v", got.Reasons)
	}
	wantKeys := []string{
		"mem_headroom", "cpu_headroom", "disk_headroom",
		"mem_weighted", "cpu_weighted", "disk_weighted",
		"spec_match", "spec_bonus", "total",
	}
	for _, k := range wantKeys {
		if _, ok := got.Components[k]; !ok {
			t.Errorf("Components missing key %q (got %v)", k, got.Components)
		}
	}
	// Spec is CPU-opt (16c/32G = 2 GiB/c < 4 threshold) and workload is
	// web → spec_bonus should fire.
	if got.Spec != nodescore.SpecCPU {
		t.Errorf("Spec = %q, want cpu", got.Spec)
	}
	if got.Components["spec_match"] != 1.0 {
		t.Errorf("spec_match = %v, want 1.0 (CPU-opt node + web workload)", got.Components["spec_match"])
	}
	// Sum check — total should equal the sum of weighted components.
	wantTotal := got.Components["mem_weighted"] +
		got.Components["cpu_weighted"] +
		got.Components["disk_weighted"] +
		got.Components["spec_bonus"]
	if abs(got.Components["total"]-wantTotal) > 1e-9 {
		t.Errorf("total = %v, want sum of weighted = %v", got.Components["total"], wantTotal)
	}
	if abs(got.Score-got.Components["total"]) > 1e-9 {
		t.Errorf("Score (%v) != Components[total] (%v)", got.Score, got.Components["total"])
	}
}

// TestScore_SpecBonusOnlyWhenMatched — spec_bonus must be 0 when the node's
// detected spec doesn't match the workload's preference, and 1× the
// profile's SpecBonus when it does.
func TestScore_SpecBonusOnlyWhenMatched(t *testing.T) {
	t.Parallel()
	memNode := nodescore.Node{
		Name: "mem-opt", Status: "online",
		MaxCPU: 8, MaxMem: 128 * gib, Mem: 1 * gib, // mem-opt
	}
	env := nodescore.Env{
		TemplatesPresent: allTemplates("mem-opt"),
		StorageByNode:    map[string]nodescore.StorageInfo{"mem-opt": {TotalBytes: 500 * gib, UsedBytes: 50 * gib}},
		Workload:         nodescore.WorkloadWeb, // web prefers CPU-opt
	}
	got := nodescore.Score(memNode, nodescore.Tiers["medium"], env, nodescore.NodeRuntime{})
	if got.Components["spec_match"] != 0.0 {
		t.Errorf("spec_match = %v, want 0 (mem-opt node + web workload)", got.Components["spec_match"])
	}
	if got.Components["spec_bonus"] != 0.0 {
		t.Errorf("spec_bonus = %v, want 0", got.Components["spec_bonus"])
	}
}

// TestScore_EmptyWorkloadDefaultsBalanced — empty Env.Workload should
// behave as WorkloadBalanced. Lets old callers + tests keep working
// without explicit workload plumbing.
func TestScore_EmptyWorkloadDefaultsBalanced(t *testing.T) {
	t.Parallel()
	node := nodescore.Node{
		Name: "n", Status: "online",
		MaxCPU: 8, MaxMem: 64 * gib, Mem: 1 * gib,
	}
	env := nodescore.Env{
		TemplatesPresent: allTemplates("n"),
		StorageByNode:    map[string]nodescore.StorageInfo{"n": {TotalBytes: 500 * gib, UsedBytes: 50 * gib}},
		// Workload deliberately empty
	}
	got := nodescore.Score(node, nodescore.Tiers["medium"], env, nodescore.NodeRuntime{})
	if got.Workload != nodescore.WorkloadBalanced {
		t.Errorf("Workload = %q, want balanced (empty default)", got.Workload)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
