// Package nodescore picks the best Proxmox cluster node for a new VM.
//
// Scoring is pure and deterministic — no I/O. The caller is expected to fetch
// live cluster telemetry (via the proxmox package) and pass it in.
//
// The scorer collapses eligibility filtering and ranking into a single
// function: Score returns 0 for any node that fails a hard gate, with the
// reason(s) captured on the Result. A non-zero score is always greater than
// any rejected node, so callers can rank without re-applying the gates.
//
// Workload-aware: every score is computed under a `WorkloadType` that
// selects a `Profile` of weights + specialization-match bonus. Empty
// workload behaves as `WorkloadBalanced` so callers that don't (yet)
// pass a workload still get sensible scoring. See Profiles for the
// per-workload weight vectors.
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
//
// LockState is the operator-set lock from db.Node.LockState — "none" /
// "cordoned" / "draining" / "drained". An empty value is treated as "none"
// for forward compat with callers that haven't been wired to populate it.
// The scorer rejects everything other than "none" before any capacity math.
type Node struct {
	Name      string
	Status    string  // "online" / "offline" / "unknown"
	CPU       float64 // 0.0 = idle, 1.0 = saturated
	MaxCPU    int     // physical core count
	Mem       uint64  // bytes used (live qemu + host overhead)
	MaxMem    uint64  // bytes total
	LockState string  // "" / "none" / "cordoned" / "draining" / "drained"
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
	ReasonCordoned          Reason = "cordoned"
	ReasonDraining          Reason = "draining"
	ReasonDrained           Reason = "drained"
	ReasonNoTemplate        Reason = "no_template"
	ReasonNoCapacity        Reason = "no_capacity"
	ReasonInsufficientCores Reason = "insufficient_cores"
	ReasonInsufficientMem   Reason = "insufficient_mem"
	ReasonInsufficientDisk  Reason = "insufficient_disk"
)

// WorkloadType is the operator-supplied (or tier-defaulted) hint about what
// kind of work the VM will do. Drives weight selection in Profiles. Empty
// string is normalized to WorkloadBalanced inside Score.
type WorkloadType string

const (
	WorkloadWeb      WorkloadType = "web"
	WorkloadDatabase WorkloadType = "database"
	WorkloadCompute  WorkloadType = "compute"
	WorkloadBalanced WorkloadType = "balanced"
)

// AllWorkloads is the canonical iteration order for "score this node against
// every workload" (used by the dashboard's scoring matrix). Listing them
// here lets handlers iterate without hard-coding the enum at every call site.
var AllWorkloads = []WorkloadType{
	WorkloadWeb,
	WorkloadDatabase,
	WorkloadCompute,
	WorkloadBalanced,
}

// Specialization classifies a node by its vCPU-to-RAM ratio. Used to apply
// the workload-matching bonus — a database VM gets a bonus on a memory-
// optimized node, and so on. Detection is operator-config-free: pure ratio
// math. See DetectSpecialization for the thresholds.
type Specialization string

const (
	SpecCPU      Specialization = "cpu"
	SpecMemory   Specialization = "memory"
	SpecBalanced Specialization = "balanced"
)

// Specialization detection thresholds, in GiB-of-RAM per vCPU. The GitHub
// issue spec phrases it as "1 vCPU per 4 GiB → CPU-opt; 1 vCPU per 8 GiB →
// mem-opt"; this is the same math written in the canonical direction
// (ratio = MaxMem / MaxCPU, in GiB per core).
//
//	ratio < specCPUMaxRatio       → SpecCPU      (e.g. 16c/32G = 2 GiB/c)
//	ratio > specMemoryMinRatio    → SpecMemory   (e.g. 8c/128G = 16 GiB/c)
//	otherwise                     → SpecBalanced (e.g. 8c/64G = 8 GiB/c)
const (
	specCPUMaxRatio    = 4.0 // GiB-per-vCPU; below → CPU-optimized
	specMemoryMinRatio = 8.0 // GiB-per-vCPU; above → memory-optimized
)

// Profile carries the per-workload weight vector + specialization-match
// bonus. Sums to ~1.0 + bonus; the bonus only fires when the node's
// detected specialization matches SpecPreference.
type Profile struct {
	MemWeight      float64
	CPUWeight      float64
	DiskWeight     float64
	SpecBonus      float64
	SpecPreference Specialization
}

// Profiles maps workload → weight vector. Public so handlers can serve
// /api/scoring/profiles without re-importing values; mutating from outside
// the package is not supported.
//
// Tuning notes (all values per the GitHub issue spec, translated into the
// unified-weighted-sum model):
//   - web: CPU-leaning (web servers are CPU-bound), bonus on CPU-opt nodes.
//   - database: memory-leaning (caches benefit from RAM), bonus on mem-opt.
//   - compute: very CPU-leaning (training, builds, ML inference), bigger
//     bonus on CPU-opt to push compute jobs hard onto the right hardware.
//   - balanced: closer to the legacy 0.6/0.4 mem/cpu split with a small
//     specialization bonus when the node is also balanced.
var Profiles = map[WorkloadType]Profile{
	WorkloadWeb:      {MemWeight: 0.30, CPUWeight: 0.45, DiskWeight: 0.10, SpecBonus: 0.15, SpecPreference: SpecCPU},
	WorkloadDatabase: {MemWeight: 0.55, CPUWeight: 0.20, DiskWeight: 0.10, SpecBonus: 0.15, SpecPreference: SpecMemory},
	WorkloadCompute:  {MemWeight: 0.15, CPUWeight: 0.55, DiskWeight: 0.10, SpecBonus: 0.20, SpecPreference: SpecCPU},
	WorkloadBalanced: {MemWeight: 0.45, CPUWeight: 0.30, DiskWeight: 0.20, SpecBonus: 0.05, SpecPreference: SpecBalanced},
}

// DetectSpecialization classifies a node by its RAM-per-vCPU ratio. Pure;
// callers compute it once per node per scoring pass.
func DetectSpecialization(n Node) Specialization {
	if n.MaxCPU <= 0 || n.MaxMem == 0 {
		return SpecBalanced
	}
	gibPerCore := float64(n.MaxMem) / float64(gibBytes) / float64(n.MaxCPU)
	switch {
	case gibPerCore < specCPUMaxRatio:
		return SpecCPU
	case gibPerCore > specMemoryMinRatio:
		return SpecMemory
	default:
		return SpecBalanced
	}
}

// DefaultWorkloadForTier maps tier name → recommended default workload.
// Used at provision time when the operator doesn't explicitly pick a
// workload, and as the read-time fallback for db.VM rows that predate
// the workload_type column.
//
//	small, medium → web      (most common deployments — web services)
//	large         → balanced (mixed workloads at the larger sizes)
//	xl            → compute  (the size people pick for ML/training)
func DefaultWorkloadForTier(tierName string) WorkloadType {
	switch tierName {
	case "small", "medium":
		return WorkloadWeb
	case "xl":
		return WorkloadCompute
	default:
		return WorkloadBalanced
	}
}

// Env carries cluster-wide knobs and the per-node lookups the caller has
// already gathered. Disk telemetry is optional: a nil StorageByNode disables
// the disk gate AND zeroes the disk weight in the soft score.
//
// Workload selects a Profile from Profiles; empty value is normalized to
// WorkloadBalanced. Callers that don't (yet) plumb workload still get
// sensible scoring.
type Env struct {
	Excluded         []string
	TemplatesPresent map[string]bool
	StorageByNode    map[string]StorageInfo // nil disables disk gate + diskweight
	MemBufferMiB     uint64                 // RAM headroom on top of tier; default 256
	CPULoadFactor    float64                // K, share of new VM's cores assumed used; default 0.5
	Workload         WorkloadType           // "" → WorkloadBalanced
}

// Result is what Score returns per node. Score == 0 ⇔ rejected; Reasons is
// populated for rejections (one or more) and empty for accepts.
//
// Components is a labelled breakdown of how the soft score was computed —
// keys: mem_headroom, cpu_headroom, disk_headroom, mem_weighted,
// cpu_weighted, disk_weighted, spec_match (0 or 1), spec_bonus, total.
// Empty for rejected scores (Score == 0). Used by the dashboard tooltip
// to render "0.30·mem(0.85) + 0.45·cpu(0.92) + …" explanations.
//
// Spec is the node's auto-detected specialization at score time; Workload
// is the resolved workload (post-default-balanced normalization). Both
// surface to the dashboard scoring matrix.
type Result struct {
	Score      float64
	Reasons    []Reason
	Components map[string]float64
	Spec       Specialization
	Workload   WorkloadType
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
// projects post-placement headroom across mem/cpu/disk and adds a
// specialization-match bonus when the node's spec matches the workload's
// preference. Components carries the per-term breakdown for transparency.
func Score(n Node, t Tier, env Env, rt NodeRuntime) Result {
	memBufferMiB := env.MemBufferMiB
	if memBufferMiB == 0 {
		memBufferMiB = defaultMemBufferMiB
	}
	cpuLoadFactor := env.CPULoadFactor
	if cpuLoadFactor == 0 {
		cpuLoadFactor = defaultCPULoadFactor
	}
	workload := env.Workload
	if workload == "" {
		workload = WorkloadBalanced
	}
	profile, ok := Profiles[workload]
	if !ok {
		profile = Profiles[WorkloadBalanced]
		workload = WorkloadBalanced
	}
	spec := DetectSpecialization(n)

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
	// Operator-set lock states. Cordoned/draining/drained nodes never receive
	// new VMs; the gate runs after offline/excluded so a single dead node
	// reports both reasons (helps diagnostics) and before any capacity math.
	switch n.LockState {
	case "cordoned":
		reasons = append(reasons, ReasonCordoned)
	case "draining":
		reasons = append(reasons, ReasonDraining)
	case "drained":
		reasons = append(reasons, ReasonDrained)
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
		return Result{Score: 0, Reasons: reasons, Spec: spec, Workload: workload}
	}

	// Soft score — every component clamped to [0, 1] so the weighted sum
	// stays in (0, 1+SpecBonus]. Disk component is zero when telemetry
	// is off; the disk weight is then re-distributed across mem/cpu in
	// proportion to their existing weights so the soft score stays
	// roughly comparable in magnitude (otherwise no-disk-telemetry
	// rejections would silently inflate scores by the disk weight).
	memHeadroom := clamp01(float64(freeMem-needMem) / float64(n.MaxMem))
	cpuHeadroom := clamp01((1.0 - n.CPU) - cpuLoadFactor*float64(t.CPU)/float64(n.MaxCPU))

	memW := profile.MemWeight
	cpuW := profile.CPUWeight
	diskW := profile.DiskWeight
	if !diskGateOn {
		// Redistribute disk weight proportionally — keeps the score
		// scale stable when an operator hasn't configured a VM-disk
		// pool. Tests + provision flows will normally have telemetry on.
		split := memW + cpuW
		if split > 0 {
			memW += diskW * memW / split
			cpuW += diskW * cpuW / split
		}
		diskW = 0
	}

	memWeighted := memW * memHeadroom
	cpuWeighted := cpuW * cpuHeadroom
	var diskHeadroom, diskWeighted float64
	if diskGateOn && totalDisk > 0 {
		diskHeadroom = clamp01(float64(freeDisk-needDiskByte) / float64(totalDisk))
		diskWeighted = diskW * diskHeadroom
	}

	specMatch := 0.0
	specBonus := 0.0
	if spec == profile.SpecPreference {
		specMatch = 1.0
		specBonus = profile.SpecBonus
	}

	total := memWeighted + cpuWeighted + diskWeighted + specBonus

	return Result{
		Score: total,
		Components: map[string]float64{
			"mem_headroom":  memHeadroom,
			"cpu_headroom":  cpuHeadroom,
			"disk_headroom": diskHeadroom,
			"mem_weighted":  memWeighted,
			"cpu_weighted":  cpuWeighted,
			"disk_weighted": diskWeighted,
			"spec_match":    specMatch,
			"spec_bonus":    specBonus,
			"total":         total,
		},
		Spec:     spec,
		Workload: workload,
	}
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
