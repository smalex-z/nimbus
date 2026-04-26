// Package nodescore picks the best Proxmox cluster node for a new VM.
//
// Scoring is pure and deterministic — no I/O. The caller is expected to fetch
// live cluster telemetry (via the proxmox package) and pass it in.
//
// The scorer collapses eligibility filtering and ranking into a single
// function: Score returns 0 for any node that fails a hard gate, with the
// reason(s) captured on the Result. A non-zero score is always greater than
// any rejected node, so callers can rank without re-applying the gates.
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

// Node is the subset of Proxmox node telemetry the scorer needs.
type Node struct {
	Name   string
	Status string  // "online" / "offline" / "unknown"
	CPU    float64 // 0.0 = idle, 1.0 = saturated
	MaxCPU int     // physical core count
	Mem    uint64  // bytes used (live qemu + host overhead)
	MaxMem uint64  // bytes total
}

// StorageInfo describes the configured VM-disk pool's capacity on one node.
// Used bytes already account for stopped VMs because their disk images persist
// on storage.
type StorageInfo struct {
	TotalBytes uint64
	UsedBytes  uint64
}

// NodeRuntime is the per-node, per-provision data the scorer can't derive
// from the Node struct alone — VM count for tie-break, and committed RAM
// (sum of every non-template VM's configured MaxMem on this node, running or
// not) for the reservation-based memory gate.
type NodeRuntime struct {
	VMCount           int
	CommittedMemBytes uint64
}

// Reason names a single rejection cause. A node may carry multiple reasons
// when several gates fail; the first cheap-to-evaluate reason fires first
// but later gates still record their findings for diagnostics.
type Reason string

const (
	ReasonOffline           Reason = "offline"
	ReasonExcluded          Reason = "excluded"
	ReasonNoTemplate        Reason = "no_template"
	ReasonNoCapacity        Reason = "no_capacity"
	ReasonInsufficientCores Reason = "insufficient_cores"
	ReasonInsufficientMem   Reason = "insufficient_mem"
	ReasonInsufficientDisk  Reason = "insufficient_disk"
)

// Env carries cluster-wide knobs and the per-node lookups the caller has
// already gathered. Disk telemetry is optional: a nil StorageByNode disables
// the disk gate and reverts the soft-score weights to the legacy 0.6/0.4
// mem/cpu split.
type Env struct {
	Excluded         []string
	TemplatesPresent map[string]bool
	StorageByNode    map[string]StorageInfo // nil disables disk gate + diskweight
	MemBufferMiB     uint64                 // RAM headroom on top of tier; default 256
	CPULoadFactor    float64                // K, share of new VM's cores assumed used; default 0.5
}

// Result is what Score returns per node. Score == 0 ⇔ rejected; Reasons is
// populated for rejections (one or more) and empty for accepts.
type Result struct {
	Score   float64
	Reasons []Reason
}

// Decision pairs a candidate with its score result and the runtime data the
// caller supplied. Pick consumes and returns slices of these so callers can
// render rejection diagnostics for losers, not just the winner.
type Decision struct {
	Node    Node
	Runtime NodeRuntime
	Result  Result
}

// tieBreakDelta is the score window within which two nodes are considered
// effectively tied — within this window the lower VM count wins.
const tieBreakDelta = 0.05

// defaultMemBufferMiB and defaultCPULoadFactor are the values applied when
// Env leaves the corresponding fields zero. Kept as package vars so tests can
// reference them.
const (
	defaultMemBufferMiB  uint64  = 256
	defaultCPULoadFactor float64 = 0.5
)

const (
	mibBytes = uint64(1 << 20)
	gibBytes = uint64(1 << 30)
)

// Score evaluates one node against one tier under one environment. Pure.
//
// Hard gates run first in order of cheapness; any failure returns Score=0
// with the rejecting reason(s) attached. When all gates pass, the soft score
// projects post-placement headroom across mem/cpu (and disk, when telemetry
// is enabled) and returns a value in (0, 1].
func Score(n Node, t Tier, env Env, rt NodeRuntime) Result {
	memBufferMiB := env.MemBufferMiB
	if memBufferMiB == 0 {
		memBufferMiB = defaultMemBufferMiB
	}
	cpuLoadFactor := env.CPULoadFactor
	if cpuLoadFactor == 0 {
		cpuLoadFactor = defaultCPULoadFactor
	}

	var reasons []Reason

	if n.Status != "online" {
		reasons = append(reasons, ReasonOffline)
	}
	for _, name := range env.Excluded {
		if name == n.Name {
			reasons = append(reasons, ReasonExcluded)
			break
		}
	}
	if !env.TemplatesPresent[n.Name] {
		reasons = append(reasons, ReasonNoTemplate)
	}
	if n.MaxMem == 0 {
		reasons = append(reasons, ReasonNoCapacity)
	}
	if t.CPU > n.MaxCPU {
		reasons = append(reasons, ReasonInsufficientCores)
	}

	// usedMem takes the larger of live host usage (which already includes file
	// cache and ZFS ARC) and the sum of every VM's configured RAM (which
	// includes stopped VMs). Pessimistic-by-design: never under-reports what
	// would be consumed if every VM were running flat-out.
	usedMem := n.Mem
	if rt.CommittedMemBytes > usedMem {
		usedMem = rt.CommittedMemBytes
	}
	var freeMem int64
	if n.MaxMem > 0 {
		freeMem = int64(n.MaxMem) - int64(usedMem)
	}
	needMem := int64(t.MemMB+memBufferMiB) * int64(mibBytes)
	if freeMem < needMem {
		reasons = append(reasons, ReasonInsufficientMem)
	}

	// Disk gate is optional — only enforced when the caller supplied storage
	// telemetry. A node missing from StorageByNode is treated as having zero
	// free bytes (the operator's configured pool isn't visible there at all).
	var (
		diskGateOn   bool
		freeDisk     int64
		totalDisk    uint64
		needDiskByte = int64(t.DiskGB) * int64(gibBytes)
	)
	if env.StorageByNode != nil {
		diskGateOn = true
		s := env.StorageByNode[n.Name]
		totalDisk = s.TotalBytes
		freeDisk = int64(s.TotalBytes) - int64(s.UsedBytes)
		if freeDisk < needDiskByte {
			reasons = append(reasons, ReasonInsufficientDisk)
		}
	}

	if len(reasons) > 0 {
		return Result{Score: 0, Reasons: reasons}
	}

	memHeadroom := float64(freeMem-needMem) / float64(n.MaxMem)
	cpuHeadroom := (1.0 - n.CPU) - cpuLoadFactor*float64(t.CPU)/float64(n.MaxCPU)
	cpuHeadroom = clamp01(cpuHeadroom)

	if !diskGateOn {
		return Result{Score: 0.6*memHeadroom + 0.4*cpuHeadroom}
	}

	diskHeadroom := float64(freeDisk-needDiskByte) / float64(totalDisk)
	return Result{Score: 0.5*memHeadroom + 0.3*cpuHeadroom + 0.2*diskHeadroom}
}

// Evaluate scores every node in input order. Use this when you want
// diagnostics for every node — rejected and accepted alike.
func Evaluate(nodes []Node, runtime map[string]NodeRuntime, t Tier, env Env) []Decision {
	out := make([]Decision, 0, len(nodes))
	for _, n := range nodes {
		rt := runtime[n.Name]
		out = append(out, Decision{
			Node:    n,
			Runtime: rt,
			Result:  Score(n, t, env, rt),
		})
	}
	return out
}

// Pick returns the highest-scoring acceptable Decision plus the full slice
// (so callers can render rejection reasons for the losers). Returns
// (nil, decisions) when no node is acceptable.
//
// Tie-break: scores within tieBreakDelta of each other are treated as tied
// and the one with the lower VMCount wins. Stable sort preserves input order
// for true ties.
func Pick(decisions []Decision) (winner *Decision, all []Decision) {
	if len(decisions) == 0 {
		return nil, decisions
	}

	sorted := make([]Decision, len(decisions))
	copy(sorted, decisions)

	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := sorted[i].Result.Score, sorted[j].Result.Score
		if math.Abs(si-sj) < tieBreakDelta {
			return sorted[i].Runtime.VMCount < sorted[j].Runtime.VMCount
		}
		return si > sj
	})

	if sorted[0].Result.Score == 0 {
		return nil, decisions
	}
	w := sorted[0]
	return &w, decisions
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
