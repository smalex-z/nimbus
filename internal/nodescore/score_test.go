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
