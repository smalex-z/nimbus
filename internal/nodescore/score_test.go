package nodescore

import (
	"math"
	"testing"
)

const gib = uint64(1024 * 1024 * 1024)

func TestScore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		node Node
		want float64
	}{
		{
			name: "fully idle 16GB node",
			node: Node{Status: "online", CPU: 0.0, Mem: 0, MaxMem: 16 * gib},
			want: 1.0,
		},
		{
			name: "fully saturated",
			node: Node{Status: "online", CPU: 1.0, Mem: 16 * gib, MaxMem: 16 * gib},
			want: 0.0,
		},
		{
			name: "half memory used, half cpu used",
			node: Node{Status: "online", CPU: 0.5, Mem: 8 * gib, MaxMem: 16 * gib},
			want: 0.5,
		},
		{
			name: "MaxMem zero is safe",
			node: Node{Status: "online", CPU: 0.5, Mem: 0, MaxMem: 0},
			want: 0.0,
		},
		{
			name: "negative cpuFree clamped",
			node: Node{Status: "online", CPU: 1.5, Mem: 0, MaxMem: 16 * gib},
			want: 0.6, // memFree = 1.0, cpuFree clamped to 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Score(tt.node)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Score(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestEligible(t *testing.T) {
	t.Parallel()

	mediumTier := Tiers["medium"] // requires 2 GiB
	templatesEverywhere := map[string]bool{
		"alpha": true, "bravo": true, "charlie": true, "delta": true,
	}

	tests := []struct {
		name             string
		nodes            []Node
		excluded         []string
		templatesPresent map[string]bool
		wantNames        []string
	}{
		{
			name: "all online with capacity and template",
			nodes: []Node{
				{Name: "alpha", Status: "online", Mem: 0, MaxMem: 8 * gib, CPU: 0.2},
				{Name: "bravo", Status: "online", Mem: 4 * gib, MaxMem: 8 * gib, CPU: 0.5},
			},
			templatesPresent: templatesEverywhere,
			wantNames:        []string{"alpha", "bravo"},
		},
		{
			name: "drops offline node",
			nodes: []Node{
				{Name: "alpha", Status: "online", MaxMem: 8 * gib},
				{Name: "bravo", Status: "offline", MaxMem: 8 * gib},
			},
			templatesPresent: templatesEverywhere,
			wantNames:        []string{"alpha"},
		},
		{
			name: "drops node with insufficient free memory",
			nodes: []Node{
				{Name: "alpha", Status: "online", Mem: 7*gib + 800*1024*1024, MaxMem: 8 * gib}, // <2GiB free
				{Name: "bravo", Status: "online", Mem: 1 * gib, MaxMem: 8 * gib},
			},
			templatesPresent: templatesEverywhere,
			wantNames:        []string{"bravo"},
		},
		{
			name: "drops excluded node",
			nodes: []Node{
				{Name: "alpha", Status: "online", MaxMem: 8 * gib},
				{Name: "bravo", Status: "online", MaxMem: 8 * gib},
			},
			excluded:         []string{"alpha"},
			templatesPresent: templatesEverywhere,
			wantNames:        []string{"bravo"},
		},
		{
			name: "drops node missing template",
			nodes: []Node{
				{Name: "alpha", Status: "online", MaxMem: 8 * gib},
				{Name: "bravo", Status: "online", MaxMem: 8 * gib},
			},
			templatesPresent: map[string]bool{"alpha": true, "bravo": false},
			wantNames:        []string{"alpha"},
		},
		{
			name:             "empty input",
			nodes:            nil,
			templatesPresent: templatesEverywhere,
			wantNames:        nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Eligible(tt.nodes, nil, mediumTier, tt.excluded, tt.templatesPresent)
			gotNames := make([]string, len(got))
			for i, c := range got {
				gotNames[i] = c.Node.Name
			}
			if !sameSet(gotNames, tt.wantNames) {
				t.Errorf("got %v, want %v", gotNames, tt.wantNames)
			}
		})
	}
}

func TestPick(t *testing.T) {
	t.Parallel()

	t.Run("nil input returns nil", func(t *testing.T) {
		if got := Pick(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("higher score wins", func(t *testing.T) {
		c := []Candidate{
			{Node: Node{Name: "low", Mem: 7 * gib, MaxMem: 8 * gib, CPU: 0.9}, VMCount: 0},
			{Node: Node{Name: "high", Mem: 1 * gib, MaxMem: 8 * gib, CPU: 0.1}, VMCount: 5},
		}
		if Pick(c).Node.Name != "high" {
			t.Errorf("expected 'high' to win on score, got %s", Pick(c).Node.Name)
		}
	})

	t.Run("tie-break by lower VM count when within delta", func(t *testing.T) {
		// Two nodes with identical scores
		c := []Candidate{
			{Node: Node{Name: "busy", Mem: 4 * gib, MaxMem: 8 * gib, CPU: 0.5}, VMCount: 10},
			{Node: Node{Name: "quiet", Mem: 4 * gib, MaxMem: 8 * gib, CPU: 0.5}, VMCount: 2},
		}
		if Pick(c).Node.Name != "quiet" {
			t.Errorf("expected 'quiet' (lower VM count) to win, got %s", Pick(c).Node.Name)
		}
	})

	t.Run("difference exceeding delta does NOT trigger tie-break", func(t *testing.T) {
		// "high" scores 0.6+ (idle) vs "busy" 0.0
		c := []Candidate{
			{Node: Node{Name: "busy", Mem: 8 * gib, MaxMem: 8 * gib, CPU: 1.0}, VMCount: 0},
			{Node: Node{Name: "high", Mem: 0, MaxMem: 8 * gib, CPU: 0}, VMCount: 100},
		}
		if Pick(c).Node.Name != "high" {
			t.Errorf("expected 'high' to win regardless of VM count, got %s", Pick(c).Node.Name)
		}
	})

	t.Run("does not mutate caller slice", func(t *testing.T) {
		c := []Candidate{
			{Node: Node{Name: "first", Mem: 4 * gib, MaxMem: 8 * gib}},
			{Node: Node{Name: "second", Mem: 0, MaxMem: 8 * gib}},
		}
		_ = Pick(c)
		if c[0].Node.Name != "first" {
			t.Errorf("Pick mutated input slice: %+v", c)
		}
	})
}

// sameSet returns true if a and b contain the same elements regardless of order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	bm := make(map[string]int, len(b))
	for _, v := range b {
		bm[v]++
	}
	for _, v := range a {
		if bm[v] == 0 {
			return false
		}
		bm[v]--
	}
	return true
}
