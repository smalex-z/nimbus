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
// Affinity model: operators tag nodes (db.Node.Tags) per their actual
// capabilities — gpu, fast-cpu, nvme, etc. — and the user opts into a
// constraint at provision time via Env.RequiredTags. Nodes missing any
// required tag get rejected with ReasonMissingTag. This mirrors OpenStack
// host aggregates / Kubernetes nodeSelector — the operator classifies
// hardware, the user requests a hardware profile.
//
// Specialization (cpu/mem/balanced) is auto-detected from the vCPU/RAM
// ratio and exposed on Result.Spec for UI labels — informational only.
// It does NOT drive scoring (the heuristic was too coarse to be useful
// across heterogeneous clusters); operators use it as a hint when
// deciding which tags to apply.
package nodescore

import (
	"math"
	"sort"
	"strings"
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
	// Tags are the operator-applied labels from db.Node.Tags (CSV
	// in storage; []string here). Used by the host-aggregate filter:
	// when Env.RequiredTags is set, this node passes only when its
	// Tags is a superset.
	Tags []string
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
	VMCount int
	// CommittedMemBytes is the sum of every non-template VM's configured
	// MaxMem on this node (running or not) — the pessimistic
	// reservation-based number the RAM gate compares against.
	CommittedMemBytes uint64
	// CommittedCPU is the sum of every non-template VM's configured
	// max vCPUs on this node (running or not). Used by the CPU gate
	// so cumulative oversubscription respects the allocation ratio
	// (e.g. with ratio=4 on an 8-thread host, cap at 32 committed
	// vCPUs across all VMs).
	CommittedCPU int
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
	// ReasonMissingTag fires when Env.RequiredTags asks for a tag the
	// node doesn't carry (operator-driven host-aggregate filter).
	ReasonMissingTag Reason = "missing_tag"
)

// Specialization classifies a node by its vCPU-to-RAM ratio. Surfaced
// on Result.Spec for UI labels (the dashboard renders cpu-opt / mem-opt
// chips per node). Informational only — does not drive scoring.
// Operators reading the chips know how to tag the node accordingly.
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

// scoreWeights is the single soft-score weight vector. With workload
// type retired, every score uses the same formula — operators express
// hardware preference through tags (Env.RequiredTags) instead of by
// picking a workload label. The values below mirror the previous
// "balanced" profile.
const (
	weightMem  = 0.45
	weightCPU  = 0.30
	weightDisk = 0.25
)

// AutoTagInput is the per-node hardware-introspection bundle the
// scheduler-side auto-tag derivation reads. CPUModel comes from
// /nodes/{n}/status; HasSSD from /nodes/{n}/disks/list; HasGPU from
// /nodes/{n}/hardware/pci (NVIDIA-only today). Both Has* default false
// when introspection hasn't run yet (new node) or returned 403 (limited
// API token), in which case those tags simply don't appear.
type AutoTagInput struct {
	CPUModel string
	HasSSD   bool
	HasGPU   bool
}

// DeriveAutoTags returns the system-derived tags Nimbus auto-applies to a
// node based on hardware introspection. Three signals today:
//   - arch: "x86" (Intel/AMD) or "arm" (Apple/Ampere/Cortex/Snapdragon/
//     Neoverse) — derived from the CPU model string.
//   - "ssd": at least one disk on the node is type=ssd or type=nvme.
//   - "gpu": at least one PCI device is from NVIDIA (vendor 0x10de).
//
// Operators don't see these as editable in the Nodes UI (they live
// alongside operator tags but aren't writable). The scheduler treats
// them identically to operator tags for RequiredTags matching, so a
// user asking for `required_tags=arm,gpu` will land only on ARM hosts
// that carry an NVIDIA GPU.
//
// Pure function — no I/O, runs on every score call.
func DeriveAutoTags(in AutoTagInput) []string {
	var tags []string
	if a := archTag(in.CPUModel); a != "" {
		tags = append(tags, a)
	}
	if in.HasSSD {
		tags = append(tags, "ssd")
	}
	if in.HasGPU {
		tags = append(tags, "gpu")
	}
	return tags
}

// archTag returns "x86" / "arm" / "" from a CPU model string.
func archTag(cpuModel string) string {
	if cpuModel == "" {
		return ""
	}
	low := strings.ToLower(cpuModel)
	switch {
	case strings.Contains(low, "intel"),
		strings.Contains(low, "amd"),
		strings.Contains(low, "xeon"),
		strings.Contains(low, "epyc"),
		strings.Contains(low, "ryzen"),
		strings.Contains(low, "core(tm)"),
		strings.Contains(low, "pentium"),
		strings.Contains(low, "celeron"),
		strings.Contains(low, "x86"):
		return "x86"
	case strings.Contains(low, "arm"),
		strings.Contains(low, "aarch64"),
		strings.Contains(low, "cortex"),
		strings.Contains(low, "apple"),
		strings.Contains(low, "ampere"),
		strings.Contains(low, "snapdragon"),
		strings.Contains(low, "neoverse"):
		return "arm"
	}
	return ""
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

// Env carries cluster-wide knobs and the per-node lookups the caller has
// already gathered. Disk telemetry is optional: a nil StorageByNode disables
// the disk gate AND zeroes the disk weight in the soft score.
//
// RequiredTags is the host-aggregate filter: when non-empty, every tag
// listed must appear in the node's Tags slice or the node is rejected
// with ReasonMissingTag. Operators tag hardware ("fast-cpu", "nvme",
// "gpu"); user opts in to a constraint at provision time.
type Env struct {
	Excluded         []string
	TemplatesPresent map[string]bool
	StorageByNode    map[string]StorageInfo // nil disables disk gate + diskweight
	MemBufferMiB     uint64                 // RAM headroom on top of tier; default 256
	CPULoadFactor    float64                // K, share of new VM's cores assumed used; default 0.5
	RequiredTags     []string               // operator-defined affinity constraints

	// Allocation ratios — cluster-wide overcommit knobs. A node's
	// effective capacity for placement is `physical × ratio`; the
	// hard gate rejects when committed + needed exceeds that. Zero
	// values fall back to the defaults below (4.0/1.0/1.0). Anything
	// less than 1.0 is clamped to 1.0 — sub-1 ratios would *under*-
	// commit, which is what the buffer/load-factor knobs already do
	// from the score side, and would silently leave physical capacity
	// unusable.
	CPUAllocationRatio  float64
	RAMAllocationRatio  float64
	DiskAllocationRatio float64
}

// Result is what Score returns per node. Score == 0 ⇔ rejected; Reasons is
// populated for rejections (one or more) and empty for accepts.
//
// Components is a labelled breakdown of how the soft score was computed —
// keys: mem_headroom, cpu_headroom, disk_headroom, mem_weighted,
// cpu_weighted, disk_weighted, total. Empty for rejected scores
// (Score == 0). Used by the dashboard tooltip to render
// "0.45·mem(0.85) + 0.30·cpu(0.92) + 0.25·disk(0.60) = 0.78"
// explanations.
//
// Spec is the node's auto-detected specialization (cpu/memory/balanced)
// at score time. Informational only — surfaced as a chip on the
// dashboard so operators know how to tag the node, but does not drive
// scoring directly.
type Result struct {
	Score      float64
	Reasons    []Reason
	Components map[string]float64
	Spec       Specialization
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
	// Default allocation ratios — homelab-friendly. CPU 4× because
	// most VMs idle far below their declared vCPUs; RAM/disk 1× to
	// avoid OOM/no-space surprises.
	defaultCPURatio  float64 = 4.0
	defaultRAMRatio  float64 = 1.0
	defaultDiskRatio float64 = 1.0
)

const (
	mibBytes = uint64(1 << 20)
	gibBytes = uint64(1 << 30)
)

// Score evaluates one node against one tier under one environment. Pure.
//
// Hard gates run first in order of cheapness; any failure returns Score=0
// with the rejecting reason(s) attached. When all gates pass, the soft
// score projects post-placement headroom across mem/cpu/disk and returns
// a value in (0, 1]. Components carries the per-term breakdown for
// transparency.
func Score(n Node, t Tier, env Env, rt NodeRuntime) Result {
	memBufferMiB := env.MemBufferMiB
	if memBufferMiB == 0 {
		memBufferMiB = defaultMemBufferMiB
	}
	cpuLoadFactor := env.CPULoadFactor
	if cpuLoadFactor == 0 {
		cpuLoadFactor = defaultCPULoadFactor
	}
	// Allocation ratios — clamp to ≥1.0 so a misconfigured 0/negative
	// value can't silently strand physical capacity.
	cpuRatio := env.CPUAllocationRatio
	if cpuRatio < 1.0 {
		cpuRatio = defaultCPURatio
	}
	ramRatio := env.RAMAllocationRatio
	if ramRatio < 1.0 {
		ramRatio = defaultRAMRatio
	}
	diskRatio := env.DiskAllocationRatio
	if diskRatio < 1.0 {
		diskRatio = defaultDiskRatio
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
	// Host-aggregate filter: every required tag must be on the node.
	// Cheaper than capacity math so it runs early, but after lock-state
	// so the operator's intent (cordon/drain) reports first.
	if missing := missingTags(n.Tags, env.RequiredTags); len(missing) > 0 {
		reasons = append(reasons, ReasonMissingTag)
	}
	if !env.TemplatesPresent[n.Name] {
		reasons = append(reasons, ReasonNoTemplate)
	}
	if n.MaxMem == 0 {
		reasons = append(reasons, ReasonNoCapacity)
	}
	// CPU gate: the new VM's vCPUs plus everything already committed
	// must fit inside `MaxCPU × cpuRatio`. With ratio=4 on an
	// 8-thread host that's a 32-vCPU sum cap. Operators typically
	// land at ratio 2-4 for homelab density; 1.0 = strict 1:1 (Nova
	// strict mode equivalent).
	cpuCap := int(float64(n.MaxCPU) * cpuRatio)
	if rt.CommittedCPU+t.CPU > cpuCap {
		reasons = append(reasons, ReasonInsufficientCores)
	}

	// usedMem takes the larger of live host usage (which already includes file
	// cache and ZFS ARC) and the sum of every VM's configured RAM (which
	// includes stopped VMs). Pessimistic-by-design: never under-reports what
	// would be consumed if every VM were running flat-out. With overcommit,
	// the comparison happens against `MaxMem × ramRatio` so the operator can
	// dial in 1.2× / 1.5× density when their VMs sit far below declared
	// MaxMem.
	usedMem := n.Mem
	if rt.CommittedMemBytes > usedMem {
		usedMem = rt.CommittedMemBytes
	}
	var freeMem int64
	if n.MaxMem > 0 {
		freeMem = int64(float64(n.MaxMem)*ramRatio) - int64(usedMem)
	}
	needMem := int64(t.MemMB+memBufferMiB) * int64(mibBytes)
	if freeMem < needMem {
		reasons = append(reasons, ReasonInsufficientMem)
	}

	// Disk gate is optional — only enforced when the caller supplied storage
	// telemetry. A node missing from StorageByNode is treated as having zero
	// free bytes (the operator's configured pool isn't visible there at all).
	// Effective capacity = `TotalBytes × diskRatio`. 1.0 is the safe default
	// because every backend honors it; the failure mode of declaring past the
	// pool size on a thick-provisioned backend is "writes fail and filesystems
	// corrupt." Operators on thin-capable pools (LVM-thin, Ceph thin, ZFS
	// sparse) can raise this to 1.5–2.0 to expose the storage layer's lazy
	// allocation to placement decisions, but only once a pool-fill monitor is
	// in place — the runtime safety net for "declared > physical" lives there,
	// not here.
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
		freeDisk = int64(float64(s.TotalBytes)*diskRatio) - int64(s.UsedBytes)
		if freeDisk < needDiskByte {
			reasons = append(reasons, ReasonInsufficientDisk)
		}
	}

	if len(reasons) > 0 {
		return Result{Score: 0, Reasons: reasons, Spec: spec}
	}

	// Soft score — every component clamped to [0, 1] so the weighted sum
	// stays in (0, 1]. Disk component is zero when telemetry is off; the
	// disk weight is then re-distributed across mem/cpu proportionally so
	// the score scale stays stable when an operator hasn't configured a
	// VM-disk pool.
	memHeadroom := clamp01(float64(freeMem-needMem) / float64(n.MaxMem))
	cpuHeadroom := clamp01((1.0 - n.CPU) - cpuLoadFactor*float64(t.CPU)/float64(n.MaxCPU))

	memW := weightMem
	cpuW := weightCPU
	diskW := weightDisk
	if !diskGateOn {
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

	total := memWeighted + cpuWeighted + diskWeighted

	return Result{
		Score: total,
		Components: map[string]float64{
			"mem_headroom":  memHeadroom,
			"cpu_headroom":  cpuHeadroom,
			"disk_headroom": diskHeadroom,
			"mem_weighted":  memWeighted,
			"cpu_weighted":  cpuWeighted,
			"disk_weighted": diskWeighted,
			"total":         total,
		},
		Spec: spec,
	}
}

// missingTags returns the slice of required tags that aren't present in
// nodeTags. Empty required → empty result (no constraint). Both inputs
// are case-sensitive; operators should keep tag casing consistent.
func missingTags(nodeTags, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]bool, len(nodeTags))
	for _, t := range nodeTags {
		have[t] = true
	}
	var miss []string
	for _, t := range required {
		if !have[t] {
			miss = append(miss, t)
		}
	}
	return miss
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
