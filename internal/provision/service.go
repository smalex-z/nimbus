// Package provision orchestrates the end-to-end VM provisioning flow.
//
// Responsibilities:
//  1. Validate the resolved request (delegated to handler — service trusts input).
//  2. Reserve an IP from the pool. Defer-release on any subsequent failure.
//  3. Pick a target node by scoring the live cluster.
//  4. Clone the OS template onto that node, set cloud-init, resize disk, start.
//  5. Wait for the agent (or TCP:22) to confirm reachability.
//  6. Persist the VM record and mark the IP allocated.
//
// The Service depends on a small ProxmoxClient interface so the orchestrator
// can be tested with a mock.
package provision

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/ippool"
	"nimbus/internal/nodescore"
	"nimbus/internal/proxmox"
)

// ProxmoxClient is the small subset of *proxmox.Client the orchestrator needs.
// Defined here (in the consumer) per the "accept interfaces" idiom — keeps the
// service trivially testable.
type ProxmoxClient interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	ListVMs(ctx context.Context, node string) ([]proxmox.VMStatus, error)
	TemplateExists(ctx context.Context, node string, vmid int) (bool, error)
	NextVMID(ctx context.Context) (int, error)
	CloneVM(ctx context.Context, sourceNode, targetNode string, templateVMID, newVMID int, name string) (string, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	SetCloudInit(ctx context.Context, node string, vmid int, cfg proxmox.CloudInitConfig) error
	ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) error
	StartVM(ctx context.Context, node string, vmid int) (string, error)
	GetAgentInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.NetworkInterface, error)
}

// Config holds the deployment-specific knobs the Service needs at construction
// time. All values come from the Config package — kept distinct so tests can
// supply arbitrary values without going through env loading.
type Config struct {
	TemplateBaseVMID int
	ExcludedNodes    []string
	GatewayIP        string
	Nameserver       string
	SearchDomain     string

	// IPReadyTimeout caps the agent/TCP polling loop. 0 means use the default
	// (120s, per design doc).
	IPReadyTimeout time.Duration

	// PollInterval controls how often we poll the agent. 0 means use 3s.
	PollInterval time.Duration

	// SourceNode is the node Proxmox queries for "clone source". Templates
	// are typically replicated to every node, but the clone API still wants
	// a source-node URL — by convention, we use the same target node. If
	// SourceNode is set, it overrides this.
	SourceNode string
}

// Service runs the orchestrated provision flow.
type Service struct {
	px   ProxmoxClient
	pool *ippool.Pool
	db   *gorm.DB
	cfg  Config

	// guards concurrent provisions from racing on cluster/nextid by
	// serializing the clone path. SQLite already serializes ippool.Reserve.
	cloneMu sync.Mutex
}

// New constructs a Service.
func New(px ProxmoxClient, pool *ippool.Pool, database *gorm.DB, cfg Config) *Service {
	if cfg.IPReadyTimeout == 0 {
		cfg.IPReadyTimeout = 120 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return &Service{px: px, pool: pool, db: database, cfg: cfg}
}

// Provision executes the 9-step flow from design doc §5.2.
//
// On any failure after the IP is reserved, we release the IP back to the pool
// before returning. The Proxmox-side artifact (a half-cloned VM) is *not*
// cleaned up automatically in Phase 1 — that's a follow-up. We do persist a
// VM row with status=failed for visibility.
func (s *Service) Provision(ctx context.Context, req Request) (*Result, error) {
	tier, ok := nodescore.Tiers[req.Tier]
	if !ok {
		return nil, &internalerrors.ValidationError{Field: "tier", Message: fmt.Sprintf("unknown tier %q", req.Tier)}
	}

	if _, ok := proxmox.TemplateOffsets[req.OSTemplate]; !ok {
		return nil, &internalerrors.ValidationError{Field: "os_template", Message: fmt.Sprintf("unknown os_template %q", req.OSTemplate)}
	}

	// Resolve SSH key.
	sshPubKey, sshPrivateKey, err := s.resolveSSHKey(req)
	if err != nil {
		return nil, err
	}
	// Only generated keys get a stored name — BYO keys are managed by the user.
	var keyName string
	if sshPrivateKey != "" {
		keyName = "nimbus-" + req.Hostname
	}

	// Step 1: reserve IP. defer release on any later failure.
	ip, err := s.pool.Reserve(ctx, req.Hostname)
	if err != nil {
		if errors.Is(err, ippool.ErrPoolExhausted) {
			return nil, &internalerrors.ConflictError{Message: "no free IP addresses in pool"}
		}
		return nil, fmt.Errorf("reserve ip: %w", err)
	}
	released := false
	defer func() {
		if !released {
			_ = s.pool.Release(context.Background(), ip)
		}
	}()

	// Step 2: gather cluster snapshot and score, restricted to nodes that
	// have a template for the requested OS.
	target, templateVMID, err := s.pickNode(ctx, tier, req.OSTemplate)
	if err != nil {
		return nil, err
	}

	// Step 3: clone the template (serialized to avoid VMID races on the
	// fresh-VMID assignment for the new VM).
	s.cloneMu.Lock()
	defer s.cloneMu.Unlock()

	newVMID, err := s.px.NextVMID(ctx)
	if err != nil {
		return nil, fmt.Errorf("nextid: %w", err)
	}

	// Source and target node are the same — the template lives on the picked
	// node by definition (pickNode only returns nodes that have a template
	// row in the DB for this OS). Local clones are fast.
	taskID, err := s.px.CloneVM(ctx, target, target, templateVMID, newVMID, req.Hostname)
	if err != nil {
		return nil, fmt.Errorf("clone vm: %w", err)
	}
	if err := s.px.WaitForTask(ctx, target, taskID, s.cfg.PollInterval); err != nil {
		return nil, fmt.Errorf("clone task: %w", err)
	}

	// Step 4: cloud-init.
	username := proxmox.TemplateUsername(req.OSTemplate)
	cloudInit := proxmox.CloudInitConfig{
		CIUser:       username,
		SSHKeys:      sshPubKey,
		IPConfig0:    fmt.Sprintf("ip=%s/24,gw=%s", ip, s.cfg.GatewayIP),
		Nameserver:   s.cfg.Nameserver,
		SearchDomain: s.cfg.SearchDomain,
	}
	if err := s.px.SetCloudInit(ctx, target, newVMID, cloudInit); err != nil {
		return nil, fmt.Errorf("set cloud-init: %w", err)
	}

	// Step 5: resize the disk to tier spec. The cloud image ships at a small
	// size; we *grow* it by the difference. (Proxmox accepts +<n>G deltas.)
	resizeDelta := tier.DiskGB - 10 // cloud images are typically 10G base
	if resizeDelta > 0 {
		if err := s.px.ResizeDisk(ctx, target, newVMID, "scsi0", fmt.Sprintf("+%dG", resizeDelta)); err != nil {
			return nil, fmt.Errorf("resize disk: %w", err)
		}
	}

	// Step 6: start the VM.
	startTask, err := s.px.StartVM(ctx, target, newVMID)
	if err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}
	if startTask != "" {
		if err := s.px.WaitForTask(ctx, target, startTask, s.cfg.PollInterval); err != nil {
			return nil, fmt.Errorf("start task: %w", err)
		}
	}

	// Step 7: wait for IP readiness.
	//
	// A timeout here is genuinely ambiguous — could mean "VM never came up"
	// or "VM is up but Nimbus's network position can't reach its IP". We
	// treat them differently:
	//
	//   - context.DeadlineExceeded → soft success: VM is real, commit the
	//     allocation, populate Result.Warning so the user knows reachability
	//     wasn't confirmed. They can SSH from a machine on the cluster LAN.
	//   - any other error (Proxmox API failure, agent crash) → hard failure:
	//     release the IP and return 500.
	var warning string
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.IPReadyTimeout)
	defer cancel()
	if err := WaitForIP(waitCtx, s.px, target, newVMID, ip, s.cfg.PollInterval); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			warning = fmt.Sprintf(
				"VM was created and configured, but Nimbus could not confirm reachability on %s within %s. "+
					"This usually means Nimbus is running outside the cluster's LAN. "+
					"The credentials are valid — try SSHing from a machine on the cluster network.",
				ip, s.cfg.IPReadyTimeout)
			// fall through to the success path
		} else {
			s.persistFailedVM(ctx, req, ip, target, newVMID, err)
			return nil, fmt.Errorf("wait for ready: %w", err)
		}
	}

	// Step 8: commit. IP transitions reserved -> allocated; VM row written.
	if err := s.pool.MarkAllocated(ctx, ip, newVMID); err != nil {
		return nil, fmt.Errorf("mark allocated: %w", err)
	}
	released = true // success path — do NOT run the deferred release

	vm := &db.VM{
		VMID:       newVMID,
		Hostname:   req.Hostname,
		IP:         ip,
		Node:       target,
		Tier:       req.Tier,
		OSTemplate: req.OSTemplate,
		Username:   username,
		Status:     "running",
		OwnerID:    req.OwnerID,
		KeyName:    keyName,
		ErrorMsg:   warning, // doubles as a soft-warning record on the persisted row
	}
	if err := s.db.WithContext(ctx).Create(vm).Error; err != nil {
		// VM is up but we couldn't write the row — log via the error path. The
		// IP is already marked allocated so we don't strand it.
		return nil, fmt.Errorf("persist vm: %w", err)
	}

	return &Result{
		VMID:          newVMID,
		Hostname:      req.Hostname,
		IP:            ip,
		Username:      username,
		OS:            req.OSTemplate,
		Tier:          req.Tier,
		Node:          target,
		SSHPrivateKey: sshPrivateKey,
		KeyName:       keyName,
		Warning:       warning,
	}, nil
}

// List returns persisted VM rows. Phase 1 ignores ownerID and returns all.
func (s *Service) List(ctx context.Context, ownerID *uint) ([]db.VM, error) {
	var vms []db.VM
	q := s.db.WithContext(ctx).Order("created_at DESC")
	if ownerID != nil {
		q = q.Where("owner_id = ?", *ownerID)
	}
	if err := q.Find(&vms).Error; err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	return vms, nil
}

// Get returns a single VM by row ID.
func (s *Service) Get(ctx context.Context, id uint) (*db.VM, error) {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return nil, fmt.Errorf("get vm %d: %w", id, err)
	}
	return &vm, nil
}

// resolveSSHKey returns (publicKey, privateKey, error). privateKey is
// non-empty only when the user asked us to generate one.
func (s *Service) resolveSSHKey(req Request) (string, string, error) {
	if req.GenerateKey {
		pub, priv, err := GenerateEd25519()
		if err != nil {
			return "", "", fmt.Errorf("generate ssh key: %w", err)
		}
		return pub, priv, nil
	}
	if req.SSHPubKey == "" {
		return "", "", &internalerrors.ValidationError{
			Field:   "ssh_pubkey",
			Message: "ssh_pubkey or generate_key must be provided",
		}
	}
	return req.SSHPubKey, "", nil
}

// pickNode collects live cluster telemetry, intersects it with the set of
// nodes that have a template for the requested OS (via the node_templates
// table), scores the survivors, and returns the winner along with the
// templateVMID to clone from on that node.
//
// The template-presence check is now a single DB query instead of a per-node
// Proxmox API fan-out — much faster and matches the actual source of truth.
func (s *Service) pickNode(
	ctx context.Context,
	tier nodescore.Tier,
	osKey string,
) (target string, templateVMID int, err error) {
	// Fetch all node_templates rows for this OS in one query. Returned
	// (node, vmid) pairs are exactly the nodes eligible to host this OS.
	var templates []db.NodeTemplate
	if err := s.db.WithContext(ctx).
		Where("os = ?", osKey).
		Find(&templates).Error; err != nil {
		return "", 0, fmt.Errorf("lookup templates for os %s: %w", osKey, err)
	}
	if len(templates) == 0 {
		return "", 0, &internalerrors.ConflictError{
			Message: fmt.Sprintf("no node has a template for os %q — run bootstrap first", osKey),
		}
	}
	templateVMIDByNode := make(map[string]int, len(templates))
	templatesPresent := make(map[string]bool, len(templates))
	for _, t := range templates {
		templateVMIDByNode[t.Node] = t.VMID
		templatesPresent[t.Node] = true
	}

	// Live telemetry: nodes for scoring + per-node VM counts for tie-break.
	nodes, err := s.px.GetNodes(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("get nodes: %w", err)
	}

	var (
		mu       sync.Mutex
		vmCounts = make(map[string]int)
		errs     []error
		wg       sync.WaitGroup
	)
	scoringNodes := make([]nodescore.Node, 0, len(nodes))
	for _, n := range nodes {
		scoringNodes = append(scoringNodes, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
		})
		if n.Status != "online" || !templatesPresent[n.Name] {
			continue // skip vm-counts for nodes we already know are ineligible
		}
		nodeName := n.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			vms, err := s.px.ListVMs(ctx, nodeName)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("list vms on %s: %w", nodeName, err))
				return
			}
			vmCounts[nodeName] = len(vms)
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return "", 0, fmt.Errorf("cluster snapshot: %w", errs[0])
	}

	candidates := nodescore.Eligible(scoringNodes, vmCounts, tier, s.cfg.ExcludedNodes, templatesPresent)
	pick := nodescore.Pick(candidates)
	if pick == nil {
		return "", 0, &internalerrors.ConflictError{
			Message: fmt.Sprintf("no eligible node for tier=%s os_template=%s (templates exist on %d node(s) but none meet resource requirements)",
				tier.Name, osKey, len(templates)),
		}
	}
	return pick.Node.Name, templateVMIDByNode[pick.Node.Name], nil
}

func (s *Service) persistFailedVM(ctx context.Context, req Request, ip, node string, vmid int, cause error) {
	_ = s.db.WithContext(ctx).Create(&db.VM{
		VMID:       vmid,
		Hostname:   req.Hostname,
		IP:         ip,
		Node:       node,
		Tier:       req.Tier,
		OSTemplate: req.OSTemplate,
		Status:     "failed",
		OwnerID:    req.OwnerID,
		ErrorMsg:   cause.Error(),
	}).Error
}

// formatResult is the body of (*Result).String — kept separate to satisfy the
// lint that fmt format functions live near other formatting code.
func formatResult(r *Result) string {
	return fmt.Sprintf(
		"VM{vmid=%d hostname=%s ip=%s node=%s tier=%s os=%s ssh_private_key=%s}",
		r.VMID, r.Hostname, r.IP, r.Node, r.Tier, r.OS,
		redactKey(r.SSHPrivateKey),
	)
}

func redactKey(k string) string {
	if k == "" {
		return "<unset>"
	}
	return "<REDACTED>"
}

// Compile-time assertion that the real *proxmox.Client satisfies our interface.
var _ ProxmoxClient = (*proxmox.Client)(nil)
