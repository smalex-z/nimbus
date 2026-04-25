// Package nodescore picks the best Proxmox cluster node for a new VM.
//
// Scoring is pure and deterministic — no I/O. The caller is expected to fetch
// live cluster telemetry (via the proxmox package) and pass it in.
package nodescore

import (
	"math"
	"sort"
)

// Tier describes a VM size class.
type Tier struct {
	Name   string
	CPU    int    // vCPU count
	MemMB  uint64 // memory in MiB
	DiskGB int    // disk in GiB
}

// Tiers is the canonical Phase-1 size catalogue. The xl tier is admin-only and
// the API rejects unprivileged xl requests at the handler.
var Tiers = map[string]Tier{
	"small":  {Name: "small", CPU: 1, MemMB: 1024, DiskGB: 15},
	"medium": {Name: "medium", CPU: 2, MemMB: 2048, DiskGB: 30},
	"large":  {Name: "large", CPU: 4, MemMB: 4096, DiskGB: 60},
	"xl":     {Name: "xl", CPU: 8, MemMB: 8192, DiskGB: 120},
}

// Node is the subset of Proxmox node telemetry we need for scoring.
type Node struct {
	Name   string
	Status string  // "online" / "offline" / "unknown"
	CPU    float64 // 0.0 = idle, 1.0 = saturated
	MaxCPU int     // physical core count
	Mem    uint64  // bytes used
	MaxMem uint64  // bytes total
}

// Candidate is a node that has passed eligibility filtering, plus the live
// count of running VMs on it (used for tie-breaking).
type Candidate struct {
	Node    Node
	VMCount int
}

// tieBreakDelta is the score window within which two nodes are considered
// "effectively tied". When tied, the candidate with fewer running VMs wins —
// preventing all new VMs from piling onto whichever node is barely ahead.
const tieBreakDelta = 0.05

// Score returns a single number in [0, 1] where higher is better. Memory has
// 60 % weight (cannot be overcommitted on Proxmox by default) and CPU has
// 40 % (can be overcommitted, less brittle).
func Score(n Node) float64 {
	if n.MaxMem == 0 {
		return 0
	}
	memFree := float64(n.MaxMem-n.Mem) / float64(n.MaxMem)
	cpuFree := 1.0 - n.CPU
	if cpuFree < 0 {
		cpuFree = 0
	}
	return 0.6*memFree + 0.4*cpuFree
}

// Eligible filters candidate nodes against four criteria from design doc §7.2:
//
//  1. Node status must be "online".
//  2. Free memory must accommodate the requested tier.
//  3. Node name must not be in `excluded`.
//  4. The OS template VMID must be present on the node (look-up provided by
//     the caller via `templatesPresent`).
//
// Returns an empty slice when no node qualifies — caller should surface a
// 503 with a meaningful message ("no node has X RAM free" vs. "all nodes
// offline").
func Eligible(
	nodes []Node,
	vmCounts map[string]int,
	tier Tier,
	excluded []string,
	templatesPresent map[string]bool,
) []Candidate {
	excludedSet := toSet(excluded)
	candidates := make([]Candidate, 0, len(nodes))

	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		if excludedSet[n.Name] {
			continue
		}
		if !templatesPresent[n.Name] {
			continue
		}
		freeMemBytes := int64(n.MaxMem) - int64(n.Mem)
		requiredBytes := int64(tier.MemMB) * 1024 * 1024
		if freeMemBytes < requiredBytes {
			continue
		}

		candidates = append(candidates, Candidate{
			Node:    n,
			VMCount: vmCounts[n.Name],
		})
	}

	return candidates
}

// Pick returns the highest-scoring candidate, applying the tie-break rule
// (lower VM count wins when scores fall within tieBreakDelta). Returns nil
// when the input is empty.
func Pick(candidates []Candidate) *Candidate {
	if len(candidates) == 0 {
		return nil
	}

	// Stable sort so the input order is preserved for true ties (same score AND
	// same VM count) — keeps behavior deterministic in tests.
	sorted := make([]Candidate, len(candidates))
	copy(sorted, candidates)

	sort.SliceStable(sorted, func(i, j int) bool {
		si := Score(sorted[i].Node)
		sj := Score(sorted[j].Node)
		if math.Abs(si-sj) < tieBreakDelta {
			return sorted[i].VMCount < sorted[j].VMCount
		}
		return si > sj
	})

	return &sorted[0]
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
