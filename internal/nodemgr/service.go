// Package nodemgr owns admin-facing node lifecycle: lock state (cordoned /
// draining / drained), tag editing, drain orchestration, and "remove from
// cluster" tear-down. The package is the source of truth for db.Node rows;
// the rest of the codebase only reads them (notably the provision scheduler,
// which checks LockState to filter candidates).
//
// Telemetry (CPU/RAM/status, VM placement) is read straight from Proxmox at
// request time. We deliberately do NOT mirror live counters into db.Node —
// they go stale within seconds and reading them from the source avoids a
// reconciler that just chases moving numbers.
package nodemgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// Errors surfaced to handlers + UI.
var (
	ErrNodeNotFound   = errors.New("node not found")
	ErrInvalidLock    = errors.New("invalid lock-state transition")
	ErrAlreadyDrained = errors.New("node has no managed VMs to drain")
	ErrNotDrained     = errors.New("node must be drained before removal")
	ErrDrainInFlight  = errors.New("a drain is already in flight for this node")
	ErrSelfHosted     = errors.New("cannot remove the node Nimbus itself runs on")
)

// ProxmoxClient is the small subset of *proxmox.Client nodemgr depends on.
// Defined here per the "accept interfaces" idiom so tests can swap in a
// fake without dragging the whole HTTP client.
type ProxmoxClient interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	GetClusterVMs(ctx context.Context) ([]proxmox.ClusterVM, error)
	GetClusterStorage(ctx context.Context) ([]proxmox.ClusterStorage, error)
	GetNodeStatus(ctx context.Context, node string) (*proxmox.NodeStatus, error)
	GetClusterStatus(ctx context.Context) ([]proxmox.ClusterStatusEntry, error)
	NodeAddresses(ctx context.Context) (map[string]string, error)
	ClusterName(ctx context.Context) (string, error)
	Version(ctx context.Context) (string, error)
	MigrateVM(ctx context.Context, sourceNode string, vmid int, targetNode string, online bool) (string, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	DeleteNode(ctx context.Context, node string) error
}

// Service is the admin-facing handle for node management. Callers obtain one
// from New and share it across handlers; methods are safe for concurrent use
// (per-node mutations serialize on a per-name mutex held inside the service).
type Service struct {
	db  *gorm.DB
	px  ProxmoxClient
	cfg Config

	// drainsMu guards drainsInFlight. Entries are added when a drain
	// starts and removed when it returns; Cordon/Uncordon/Remove consult
	// the map to refuse mid-flight mutations on the same node.
	drainsMu       sync.Mutex
	drainsInFlight map[string]bool
}

// Config tunes per-VM execution timeouts and reconciler thresholds. Zero
// values fall back to defaults wired in New.
type Config struct {
	// PerVMMigrateTimeout caps each individual VM's migration. Live
	// migrations of large VMs can run minutes; 30 min is the safe default
	// matching Proxmox's own migrate-vm task default.
	PerVMMigrateTimeout time.Duration
	// TaskPollInterval is how often we poll Proxmox task status during a
	// migration. 2 s mirrors the rest of the codebase.
	TaskPollInterval time.Duration
	// VacateMissThreshold is how many reconcile cycles a node may be
	// missing from Proxmox before its db.Node row is pruned. Defaults to
	// 3 — same as ippool.
	VacateMissThreshold int
	// SelfHostName is the hostname Nimbus itself runs on (typically
	// resolved from the system hostname). RemoveNode refuses to delete
	// this node even when drained — pulling it would brick the API
	// the operator is talking to.
	SelfHostName string
	// VMDiskStorage is the Proxmox storage pool name VMs land on
	// (cfg.VMDiskStorage, e.g. "local-lvm"). When set, List populates
	// per-node disk metrics filtered to this pool. Empty leaves disk
	// fields zeroed.
	VMDiskStorage string
}

// New constructs a Service. database is the shared *gorm.DB; px is the live
// Proxmox client (or a test fake implementing ProxmoxClient).
func New(database *gorm.DB, px ProxmoxClient, cfg Config) *Service {
	if cfg.PerVMMigrateTimeout == 0 {
		cfg.PerVMMigrateTimeout = 30 * time.Minute
	}
	if cfg.TaskPollInterval == 0 {
		cfg.TaskPollInterval = 2 * time.Second
	}
	if cfg.VacateMissThreshold == 0 {
		cfg.VacateMissThreshold = 3
	}
	return &Service{
		db:             database,
		px:             px,
		cfg:            cfg,
		drainsInFlight: make(map[string]bool),
	}
}

// View is the per-node row served to the SPA. Combines persistent fields
// (lock state, tags) with live telemetry (CPU/RAM/VM count) read at
// request time. Used by GET /api/nodes.
type View struct {
	Name         string     `json:"name"`
	Status       string     `json:"status"`     // "online" / "offline" / "unknown" — from Proxmox
	LockState    string     `json:"lock_state"` // "none"/"cordoned"/"draining"/"drained"
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	LockedBy     *uint      `json:"locked_by,omitempty"`
	LockReason   string     `json:"lock_reason,omitempty"`
	Tags         []string   `json:"tags"`
	CPU          float64    `json:"cpu"`
	MaxCPU       int        `json:"max_cpu"`
	MemUsed      uint64     `json:"mem_used"`
	MemTotal     uint64     `json:"mem_total"`
	MemAllocated uint64     `json:"mem_allocated"`
	// SwapUsed/SwapTotal come from /nodes/{node}/status (fan-out per
	// online node). Both 0 when the per-node call fails — single dead
	// node never blanks the table.
	SwapUsed  uint64 `json:"swap_used"`
	SwapTotal uint64 `json:"swap_total"`
	// DiskUsed/DiskTotal are the configured VM-disk pool's capacity
	// on this node (from /cluster/resources?type=storage filtered by
	// cfg.VMDiskStorage). DiskAllocated is the sum of every non-
	// template VM's configured maxdisk on this node — the pessimistic
	// "what would fill if every VM grew to its declared size" number.
	// All three zero when no pool name is configured (cfg.VMDiskStorage
	// empty) or when this node doesn't expose the pool.
	DiskUsed      uint64    `json:"disk_used"`
	DiskTotal     uint64    `json:"disk_total"`
	DiskAllocated uint64    `json:"disk_allocated"`
	DiskPoolName  string    `json:"disk_pool_name,omitempty"`
	VMCount       int       `json:"vm_count"`       // running, non-template
	VMCountTotal  int       `json:"vm_count_total"` // all non-template
	IP            string    `json:"ip,omitempty"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	IsSelfHost    bool      `json:"is_self_host"` // true for the node Nimbus runs on
}

// ListView is the cluster-wide view-model the SPA renders on /nodes. Lifts
// expensive Proxmox calls behind one entry point so the handler stays a
// thin shim.
type ListView struct {
	Nodes []View `json:"nodes"`
}

// List composes the per-node view from Proxmox telemetry + the local db.Node
// rows (lock state, tags, lock context). Reads are best-effort: if a per-node
// status call fails we serve zeroes for swap and keep going so a single dead
// node doesn't blank the whole table.
func (s *Service) List(ctx context.Context) (*ListView, error) {
	nodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("get nodes: %w", err)
	}
	clusterVMs, err := s.px.GetClusterVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get cluster vms: %w", err)
	}

	// Aggregate per-node VM counts + committed mem + committed disk.
	type agg struct {
		running       int
		total         int
		memAllocated  uint64
		diskAllocated uint64
	}
	perNode := make(map[string]agg, len(nodes))
	for _, vm := range clusterVMs {
		if vm.Template != 0 {
			continue
		}
		a := perNode[vm.Node]
		a.total++
		a.memAllocated += vm.MaxMem
		a.diskAllocated += vm.MaxDisk
		if vm.Status == "running" {
			a.running++
		}
		perNode[vm.Node] = a
	}

	// Per-node disk capacity for the configured VM pool. Best-effort:
	// failure here just leaves disk fields zeroed; the rest of the
	// table still renders.
	diskByNode := s.fanoutDisk(ctx)

	nodeIP, err := s.px.NodeAddresses(ctx)
	if err != nil {
		nodeIP = nil
	}

	// Swap counters live on /nodes/{node}/status — fan out per online
	// node in parallel. Failures fall back to zero so a single dead node
	// doesn't blank the rest of the table. Mirrors the original handler's
	// behaviour so the Admin dashboard's swap UsageBars keep working.
	swapByNode := s.fanoutSwap(ctx, nodes)

	// Reconcile DB rows: ensure each observed node has a row, bump
	// LastSeenAt on every observation. This piggy-backs on every List
	// call so the row state stays current without a dedicated loop.
	persistByName, err := s.reconcileObserved(ctx, nodes)
	if err != nil {
		return nil, fmt.Errorf("reconcile node rows: %w", err)
	}

	out := make([]View, 0, len(nodes))
	for _, n := range nodes {
		a := perNode[n.Name]
		row := persistByName[n.Name]
		swap := swapByNode[n.Name]
		disk := diskByNode[n.Name]
		out = append(out, View{
			Name:          n.Name,
			Status:        n.Status,
			LockState:     lockOrNone(row.LockState),
			LockedAt:      row.LockedAt,
			LockedBy:      row.LockedBy,
			LockReason:    row.LockReason,
			Tags:          splitTags(row.Tags),
			CPU:           n.CPU,
			MaxCPU:        n.MaxCPU,
			MemUsed:       n.Mem,
			MemTotal:      n.MaxMem,
			MemAllocated:  a.memAllocated,
			SwapUsed:      swap.Used,
			SwapTotal:     swap.Total,
			DiskUsed:      disk.Used,
			DiskTotal:     disk.Total,
			DiskAllocated: a.diskAllocated,
			DiskPoolName:  s.cfg.VMDiskStorage,
			VMCount:       a.running,
			VMCountTotal:  a.total,
			IP:            nodeIP[n.Name],
			LastSeenAt:    row.LastSeenAt,
			IsSelfHost:    s.cfg.SelfHostName != "" && n.Name == s.cfg.SelfHostName,
		})
	}
	return &ListView{Nodes: out}, nil
}

// diskCapacity is a stripped-down storage projection per node. Total +
// Used in bytes for the configured VM-disk pool. Both zero when the
// pool isn't visible on this node (different storage tier, mis-config).
type diskCapacity struct {
	Used  uint64
	Total uint64
}

// fanoutDisk returns name → diskCapacity for the configured VMDiskStorage
// pool. One cluster-wide call to /cluster/resources?type=storage; rows
// are filtered by Storage name. Shared pools repeat per node with the
// same numbers — fine, we just stamp the same value onto each node.
//
// Returns an empty map (no error) when no pool is configured or when
// the cluster-storage call fails — disk fields then stay zeroed and
// the SPA hides the disk bar.
func (s *Service) fanoutDisk(ctx context.Context) map[string]diskCapacity {
	out := make(map[string]diskCapacity)
	if s.cfg.VMDiskStorage == "" {
		return out
	}
	rows, err := s.px.GetClusterStorage(ctx)
	if err != nil {
		return out
	}
	for _, st := range rows {
		if st.Storage != s.cfg.VMDiskStorage {
			continue
		}
		out[st.Node] = diskCapacity{Used: st.Used, Total: st.Total}
	}
	return out
}

// fanoutSwap reads /nodes/{node}/status in parallel for each online node
// and returns name→Swap. Per-node failures fall through with zeroes — the
// caller renders empty swap bars for that node rather than erroring out
// the whole table.
func (s *Service) fanoutSwap(ctx context.Context, nodes []proxmox.Node) map[string]proxmox.MemPair {
	out := make(map[string]proxmox.MemPair, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			st, err := s.px.GetNodeStatus(ctx, name)
			if err != nil || st == nil {
				return
			}
			mu.Lock()
			out[name] = st.Swap
			mu.Unlock()
		}(n.Name)
	}
	wg.Wait()
	return out
}

// reconcileObserved upserts a db.Node for every node Proxmox returned and
// bumps LastSeenAt. Returns the resulting rows keyed by name. Pruning of
// long-missing nodes is intentionally NOT done here — it'd surprise the
// operator mid-list-view; runs in a background loop instead.
func (s *Service) reconcileObserved(ctx context.Context, observed []proxmox.Node) (map[string]db.Node, error) {
	out := make(map[string]db.Node, len(observed))
	now := time.Now().UTC()
	for _, n := range observed {
		var row db.Node
		err := s.db.WithContext(ctx).
			Where(&db.Node{Name: n.Name}).
			Attrs(&db.Node{LockState: "none", LastSeenAt: now}).
			FirstOrCreate(&row).Error
		if err != nil {
			return nil, fmt.Errorf("upsert node %s: %w", n.Name, err)
		}
		// Bump LastSeenAt on every observation, but skip the write if the
		// row is unchanged enough to avoid hot-looping the SQLite single
		// writer when /nodes is polled.
		if now.Sub(row.LastSeenAt) > 30*time.Second {
			if err := s.db.WithContext(ctx).Model(&db.Node{}).
				Where("name = ?", n.Name).
				Update("last_seen_at", now).Error; err != nil {
				return nil, fmt.Errorf("touch last_seen_at %s: %w", n.Name, err)
			}
			row.LastSeenAt = now
		}
		out[n.Name] = row
	}
	return out, nil
}

// PruneMissing removes db.Node rows whose corresponding Proxmox node has
// been unobserved for VacateMissThreshold consecutive reconcile cycles.
// Called by the background loop; not by handlers.
//
// Implementation: anything older than (cycle interval × threshold) ago
// gets pruned. Caller passes the cycle interval so the threshold has a
// concrete time dimension regardless of how often Reconcile fires.
func (s *Service) PruneMissing(ctx context.Context, cycleInterval time.Duration) (pruned int64, err error) {
	cutoff := time.Now().UTC().Add(-cycleInterval * time.Duration(s.cfg.VacateMissThreshold))
	res := s.db.WithContext(ctx).
		Where("last_seen_at < ?", cutoff).
		Delete(&db.Node{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune missing nodes: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// Reconcile runs one observe + prune cycle. Suitable for the background
// loop in main.go. Failures are returned to the caller; the caller is
// expected to log and continue (next cycle will retry).
func (s *Service) Reconcile(ctx context.Context, cycleInterval time.Duration) (observed int, pruned int64, err error) {
	nodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("get nodes: %w", err)
	}
	if _, err := s.reconcileObserved(ctx, nodes); err != nil {
		return 0, 0, err
	}
	pruned, err = s.PruneMissing(ctx, cycleInterval)
	if err != nil {
		return len(nodes), 0, err
	}
	return len(nodes), pruned, nil
}

// Binding is what GET /api/proxmox/binding returns — the Nodes page
// consumes it as the "what cluster are we talking to" indicator.
type Binding struct {
	Host string `json:"host"` // configured Proxmox base URL
	// TokenID is the user@realm!tokenname half of the Proxmox token
	// (the secret stays write-only). Returned so the change-binding
	// modal can pre-fill rather than asking the operator to retype it.
	// Not actually a secret — anyone with read access to the env file
	// can read it; surfacing here just saves a copy-paste step.
	TokenID string `json:"token_id,omitempty"`
	// ConnectedNode is the cluster member whose Proxmox API actually
	// served this request (the node where corosync `Local=1`). For
	// single-host deployments it equals the only node; for multi-node
	// clusters it's the entry-point node — telling the operator "you
	// reach the cluster *through* this host." Distinct from
	// ClusterName, which is the corosync-wide identifier.
	ConnectedNode string    `json:"connected_node"`
	ClusterName   string    `json:"cluster_name"` // empty for single-node deployments
	Version       string    `json:"version"`      // e.g. "8.2.7"
	NodeCount     int       `json:"node_count"`
	LastSeen      time.Time `json:"last_seen"` // wall time of the last successful call
	Reachable     bool      `json:"reachable"` // false when ALL the calls failed
}

// GetBinding returns the cluster-binding payload. Failures are not
// fatal — partial responses (e.g. version OK, cluster name failed) are
// served with the missing fields blank.
//
// host + tokenID are provided by the caller (config) since the proxmox
// client doesn't expose either; passing them through Service avoids
// leaking the config dependency to every consumer.
//
// ConnectedNode comes from /cluster/status — Proxmox marks the
// API-receiving node with Local=1 so we can name the entry point
// rather than just the cluster.
func (s *Service) GetBinding(ctx context.Context, host, tokenID string) (*Binding, error) {
	out := &Binding{Host: host, TokenID: tokenID}

	if v, err := s.px.Version(ctx); err == nil {
		out.Version = v
		out.Reachable = true
		out.LastSeen = time.Now().UTC()
	}
	if entries, err := s.px.GetClusterStatus(ctx); err == nil {
		for _, e := range entries {
			if e.Type == "cluster" && out.ClusterName == "" {
				out.ClusterName = e.Name
			}
			if e.Type == "node" && e.Local == 1 {
				out.ConnectedNode = e.Name
			}
		}
	}
	if nodes, err := s.px.GetNodes(ctx); err == nil {
		out.NodeCount = len(nodes)
		out.Reachable = true
		if out.LastSeen.IsZero() {
			out.LastSeen = time.Now().UTC()
		}
		// /cluster/status omits the cluster row on single-node
		// deployments — fall back to the lone node's name so the chip
		// has *something* concrete to display rather than "—".
		if out.ConnectedNode == "" && len(nodes) == 1 {
			out.ConnectedNode = nodes[0].Name
		}
	}
	return out, nil
}

// loadOrCreate returns the db.Node row for name, creating a default-state
// row if the reconciler hasn't seen it yet. Used by every mutator before it
// inspects/updates lock state — guarantees the row exists.
func (s *Service) loadOrCreate(ctx context.Context, name string) (*db.Node, error) {
	var row db.Node
	err := s.db.WithContext(ctx).
		Where(&db.Node{Name: name}).
		Attrs(&db.Node{LockState: "none", LastSeenAt: time.Now().UTC()}).
		FirstOrCreate(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// markDrainInFlight returns false if a drain is already in flight for this
// node. Caller must defer markDrainDone with the same name so the lock
// releases on completion (success or failure).
func (s *Service) markDrainInFlight(name string) bool {
	s.drainsMu.Lock()
	defer s.drainsMu.Unlock()
	if s.drainsInFlight[name] {
		return false
	}
	s.drainsInFlight[name] = true
	return true
}

func (s *Service) markDrainDone(name string) {
	s.drainsMu.Lock()
	defer s.drainsMu.Unlock()
	delete(s.drainsInFlight, name)
}

func lockOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// splitTags + joinTags map between the CSV column and []string the SPA
// consumes. Both trim whitespace and skip empties so " a, b ,, c " round-
// trips as ["a","b","c"].
func splitTags(csv string) []string {
	if csv == "" {
		return []string{}
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinTags(tags []string) string {
	cleaned := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	return strings.Join(cleaned, ",")
}
