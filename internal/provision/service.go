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
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/ippool"
	"nimbus/internal/nodescore"
	"nimbus/internal/proxmox"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
	"nimbus/internal/tunnel"
	"nimbus/internal/vnetmgr"
)

// maxVerifyAttempts caps how many times we'll re-reserve when the verifier
// reports a candidate IP as already claimed (or fails to look it up). Three
// attempts is enough — losing three races on three different IPs implies
// state skew worth surfacing as an error rather than spinning indefinitely.
const maxVerifyAttempts = 3

// IPVerifier checks whether an IP is unclaimed across the Proxmox cluster.
// Defined here in the consumer package per the "accept interfaces" idiom; in
// production this is satisfied by *ippool.Reconciler.
type IPVerifier interface {
	// VerifyFree returns (true, nil, nil) when no VM in the cluster claims the
	// supplied IP, (false, &vmid, nil) when one does, and (false, nil, err) on
	// transient lookup error (treat as unsafe).
	VerifyFree(ctx context.Context, ip string) (bool, *int, error)
}

// noopVerifier always reports IPs as free. Used when no IPVerifier has been
// installed on the Service — preserves single-instance behavior so existing
// tests of the provisioning logic still work, and so the service degrades to
// pre-reconciliation behavior if the operator disables it.
type noopVerifier struct{}

func (noopVerifier) VerifyFree(_ context.Context, _ string) (bool, *int, error) {
	return true, nil, nil
}

// ProxmoxClient is the small subset of *proxmox.Client the orchestrator needs.
// Defined here (in the consumer) per the "accept interfaces" idiom — keeps the
// service trivially testable.
type ProxmoxClient interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	GetClusterVMs(ctx context.Context) ([]proxmox.ClusterVM, error)
	GetClusterStorage(ctx context.Context) ([]proxmox.ClusterStorage, error)
	GetVMConfig(ctx context.Context, node string, vmid int) (map[string]any, error)
	TemplateExists(ctx context.Context, node string, vmid int) (bool, error)
	NextVMID(ctx context.Context) (int, error)
	CloneVM(ctx context.Context, sourceNode, targetNode string, templateVMID, newVMID int, name string) (string, error)
	WaitForTask(ctx context.Context, node, taskID string, interval time.Duration) error
	SetCloudInit(ctx context.Context, node string, vmid int, cfg proxmox.CloudInitConfig) error
	SetVMTags(ctx context.Context, node string, vmid int, tags []string) error
	SetVMDescription(ctx context.Context, node string, vmid int, description string) error
	ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) error
	StartVM(ctx context.Context, node string, vmid int) (string, error)
	StopVM(ctx context.Context, node string, vmid int) (string, error)
	ShutdownVM(ctx context.Context, node string, vmid int) (string, error)
	RebootVM(ctx context.Context, node string, vmid int) (string, error)
	DestroyVM(ctx context.Context, node string, vmid int) (string, error)
	GetAgentInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.NetworkInterface, error)
	UploadFile(ctx context.Context, node, storage, contentType, filename string, content []byte) error
	DeleteStorageVolume(ctx context.Context, node, volid string) error
	AttachCDROM(ctx context.Context, node string, vmid int, slot, volid string) error
	DetachDrive(ctx context.Context, node string, vmid int, slot string) error
	// AgentRun submits a command to the in-guest qemu-guest-agent and
	// blocks until it exits (or ctx expires). The data path is
	// virtio-serial via the host hypervisor — no L3 reach to the VM
	// is required, which is what lets bootstrap paths work on
	// isolated SDN subnets.
	AgentRun(ctx context.Context, node string, vmid int, command []string, inputData string, pollInterval time.Duration) (*proxmox.AgentExecStatus, error)
	MigrateVM(ctx context.Context, sourceNode string, vmid int, targetNode string, online bool) (string, error)
	// SetVMNetwork rewrites net0's bridge on a cloned VM. Used to
	// switch from the template's inherited `bridge=vmbr0` to the
	// user's per-subnet SDN VNet at provision time. Proxmox
	// auto-generates a fresh MAC when net0 is rewritten without
	// macaddr=, which is what we want.
	SetVMNetwork(ctx context.Context, node string, vmid int, dev, bridge string) error
}

// Config holds the deployment-specific knobs the Service needs at construction
// time. All values come from the Config package — kept distinct so tests can
// supply arbitrary values without going through env loading.
type Config struct {
	TemplateBaseVMID int
	ExcludedNodes    []string
	GatewayIP        string
	// PrefixLen is the netmask length applied to every VM's cloud-init
	// ipconfig0 (24 → /24, 16 → /16, etc.). 0 falls back to the historical
	// 24 default; explicit values are passed through verbatim. Live-rotated
	// from the Settings → Network page via SetPrefixLen.
	PrefixLen    int
	Nameserver   string
	SearchDomain string
	// CPUType is the Proxmox CPU model applied to each clone. Empty leaves
	// whatever the template set, which on default Proxmox installs is the
	// AVX-less kvm64/x86-64-v2-AES — see config.VMCPUType for the default
	// and why x86-64-v3 is the right baseline.
	CPUType string

	// IPReadyTimeout caps the agent/TCP polling loop. 0 means use the default
	// (120s, per design doc).
	IPReadyTimeout time.Duration

	// PollInterval controls how often we poll the agent. 0 means use 3s.
	PollInterval time.Duration

	// NetworkOpPerVMTimeout caps each per-VM iteration in ForceGatewayUpdate /
	// RenumberAllVMs so a single hung Proxmox reboot task can't wedge the
	// whole batch behind one VM. 0 means use the default (3 min).
	NetworkOpPerVMTimeout time.Duration

	// SourceNode is the node Proxmox queries for "clone source". Templates
	// are typically replicated to every node, but the clone API still wants
	// a source-node URL — by convention, we use the same target node. If
	// SourceNode is set, it overrides this.
	SourceNode string

	// VMDiskStorage names the Proxmox storage pool the disk gate checks for
	// free capacity. Defaults to bootstrap.DefaultDiskStorage ("local-lvm")
	// when unset. An empty value also disables the disk gate (the scorer
	// reverts to mem+cpu only).
	VMDiskStorage string

	// MemBufferMiB is the RAM headroom required above the tier's request.
	// Default 256. Operators on tight clusters may set 0; on generous
	// clusters they may raise it for safer packing.
	MemBufferMiB uint64

	// CPULoadFactor (K) is the share of a fresh VM's vCPUs we assume it
	// will consume on average — used by the soft-score CPU projection.
	// Default 0.5. Range typically [0.25, 1.0].
	CPULoadFactor float64

	// CIDataStorage names the Proxmox storage Nimbus uploads per-VM
	// cloud-init ISOs to. We attach the ISO at ide2,media=cdrom on
	// every clone — cloud-init's NoCloud datasource finds it via the
	// `cidata` label and runs our user-data (including the qga
	// install). The storage must accept `iso` content (every default
	// PVE storage does). Empty string falls back to attaching no
	// ISO; clones get only Proxmox's auto cloud-init drive (which
	// doesn't install qga, so WaitForIP times out — set this in
	// production).
	CIDataStorage string
}

// Service runs the orchestrated provision flow.
type Service struct {
	px       ProxmoxClient
	pool     *ippool.Pool
	verifier IPVerifier
	db       *gorm.DB
	cipher   *secrets.Cipher // encrypts SSH private keys at rest
	keys     *sshkeys.Service
	tunnels  *tunnel.Client // optional Gopher client; nil disables tunnel support
	cfg      Config

	// gpuMu / gpuCfg guard the live-reloadable GPU bootstrap config. Setting
	// is rare (admins flip the toggle in Settings); reads happen on every
	// provision. A simple RWMutex would also work; we use plain Mutex because
	// contention is negligible.
	gpuMu  sync.Mutex
	gpuCfg GPUBootstrapConfig

	// gatewayMu guards the live-reloadable gateway IP + cloud-init prefix
	// length. Both are seeded from Config in New and overwritten by the
	// Settings → Network page without a restart. The prefix is the netmask
	// length applied to every cloud-init ipconfig0 (24 for /24, 16 for /16,
	// etc.) — wrong values here cause VMs to come up with a mask that
	// doesn't reach their gateway.
	gatewayMu sync.RWMutex
	gatewayIP string
	prefixLen int

	// unreachableNodes is the per-cycle reachability probe consulted by
	// ReconcileVMs. nil → no guard (legacy reap-on-miss behaviour). Set via
	// SetUnreachableNodesProbe. Lock-free: a single pointer write swaps the
	// closure atomically, and ReconcileVMs reads it once per call.
	unreachableNodes UnreachableNodesFunc

	// guards concurrent provisions from racing on cluster/nextid by
	// serializing the clone path. SQLite already serializes ippool.Reserve.
	cloneMu sync.Mutex

	// quota resolves a user's effective VM cap (per-user override or
	// workspace default) at gate-time. nil falls back to the package
	// constant MemberMaxVMs — preserves the path tests use without
	// booting auth.
	quota QuotaResolver

	// subnetResolver resolves the user's SDN subnet at provision time.
	// nil = SDN is disabled cluster-wide; provision falls through to
	// the legacy global vmbr0 pool path. *vnetmgr.Service satisfies it.
	subnetResolver SubnetResolver
}

// SubnetResolver is the small interface vnetmgr.Service satisfies for
// the provision flow. Resolves a user's chosen subnet (existing /
// inline-create / default) into a UserSubnet row whose VNet, Subnet,
// Gateway are used to build cloud-init + the post-clone bridge override.
// Returning (nil, nil) means SDN is disabled — caller uses the legacy
// global vmbr0 pool.
type SubnetResolver interface {
	ResolveForProvision(ctx context.Context, ownerID uint, subnetID *uint, subnetName string) (*db.UserSubnet, error)
}

// SetSubnetResolver wires the resolver onto the Service. Idempotent;
// pass nil to disable per-subnet provision (every VM lands on vmbr0).
func (s *Service) SetSubnetResolver(r SubnetResolver) {
	s.subnetResolver = r
}

// SetQuotaResolver wires a QuotaResolver onto the Service so the
// member quota gate can consult per-user overrides + workspace
// defaults rather than the legacy MemberMaxVMs constant. Idempotent;
// pass nil to revert to the constant fallback.
func (s *Service) SetQuotaResolver(q QuotaResolver) {
	s.quota = q
}

// New constructs a Service. The verifier defaults to a noop that always
// reports IPs as free; production wiring should call SetIPVerifier with a
// real *ippool.Reconciler so concurrent provisions across multiple Nimbus
// instances catch each other's reservations before the clone step.
func New(px ProxmoxClient, pool *ippool.Pool, database *gorm.DB, cipher *secrets.Cipher, keys *sshkeys.Service, cfg Config) *Service {
	if cfg.IPReadyTimeout == 0 {
		cfg.IPReadyTimeout = 120 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.NetworkOpPerVMTimeout == 0 {
		cfg.NetworkOpPerVMTimeout = 3 * time.Minute
	}
	prefix := cfg.PrefixLen
	if prefix < 1 || prefix > 32 {
		// Default + safety: keep historical /24 behaviour when the caller
		// hasn't configured a prefix. Out-of-range explicit values are
		// treated as unset rather than letting cloud-init choke later.
		prefix = 24
	}
	return &Service{
		px:        px,
		pool:      pool,
		verifier:  noopVerifier{},
		db:        database,
		cipher:    cipher,
		keys:      keys,
		cfg:       cfg,
		gatewayIP: cfg.GatewayIP,
		prefixLen: prefix,
	}
}

// SetGatewayIP rotates the gateway used for new provisions. Live-reloadable
// from Settings → Network. Empty input is a no-op so the handler can blindly
// push the persisted value without re-checking. Existing VMs are NOT updated
// — that requires the explicit force-gateway-update op.
func (s *Service) SetGatewayIP(ip string) {
	if ip == "" {
		return
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	s.gatewayIP = ip
}

// GatewayIP returns the gateway used for new provisions, under lock.
func (s *Service) GatewayIP() string {
	s.gatewayMu.RLock()
	defer s.gatewayMu.RUnlock()
	return s.gatewayIP
}

// SetPrefixLen rotates the cloud-init prefix length applied to new VMs.
// Live-reloadable from Settings → Network. Out-of-range values (0 or
// outside 1..32) are dropped with a no-op so the handler can blindly push
// the persisted value without re-checking.
func (s *Service) SetPrefixLen(n int) {
	if n < 1 || n > 32 {
		return
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	s.prefixLen = n
}

// PrefixLen returns the cloud-init prefix length used for new provisions,
// under lock. Always returns a value in 1..32; never zero.
func (s *Service) PrefixLen() int {
	s.gatewayMu.RLock()
	defer s.gatewayMu.RUnlock()
	return s.prefixLen
}

// SetIPVerifier installs (or replaces) the IP verifier used after each Reserve
// to confirm the candidate IP is not held by a VM elsewhere on the cluster.
// Passing nil is a no-op so callers can wire optional dependencies safely.
func (s *Service) SetIPVerifier(v IPVerifier) {
	if v != nil {
		s.verifier = v
	}
}

// SetTunnelClient installs (or replaces) the Gopher tunnel client. Passing
// nil disables tunnel support — Provision will silently skip tunnel work
// regardless of req.PublicTunnel.
func (s *Service) SetTunnelClient(t *tunnel.Client) {
	s.tunnels = t
}

// SetGPUBootstrapConfig installs the GPU plane info that's stamped into
// each freshly provisioned VM (env vars + ~/bin/gx10 CLI). Passing a zero
// value (BaseURL == "") disables GPU bootstrap — Provision will silently
// skip the SSH-based config delivery. Live-reloadable from the Settings
// page so admins can flip the GPU plane on/off without a Nimbus restart.
//
// Refuses to install a config whose NimbusGPUAPI is localhost — those URLs
// would bake into the VM's profile and leave the in-VM `gx10` CLI calling
// the *VM's* localhost instead of Nimbus. Operator must set APP_URL to a
// publicly-reachable hostname before pairing for the bootstrap to fire.
func (s *Service) SetGPUBootstrapConfig(cfg GPUBootstrapConfig) {
	if cfg.NimbusGPUAPI != "" && looksLikeLocalhostURL(cfg.NimbusGPUAPI) {
		log.Printf("provision: refusing GPU bootstrap — NIMBUS_GPU_API would resolve to localhost (%s). Set APP_URL to a publicly-reachable URL and restart.", cfg.NimbusGPUAPI)
		cfg = GPUBootstrapConfig{} // empty cfg → bootstrap step is skipped
	}
	s.gpuMu.Lock()
	defer s.gpuMu.Unlock()
	s.gpuCfg = cfg
}

// gpuBootstrapConfig returns the current GPU config under lock.
func (s *Service) gpuBootstrapConfig() GPUBootstrapConfig {
	s.gpuMu.Lock()
	defer s.gpuMu.Unlock()
	return s.gpuCfg
}

// Provision executes the 9-step flow from design doc §5.2.
//
// On any failure after the IP is reserved, we release the IP back to
// the pool before returning. Once the clone task settles the VM exists
// in Proxmox; any later failure also runs a deferred best-effort
// stop+destroy so we don't leave a hung shell on the cluster.
// Soft-success (WaitForIP deadline) commits the VM with a warning —
// the credentials are valid, just unreachable from Nimbus's vantage.
//
// progress is optional; when non-nil it receives a ProgressEvent each time a
// user-visible phase boundary closes. The handler uses it to drive the
// frontend's checklist; service tests use it to assert step ordering.
func (s *Service) Provision(ctx context.Context, req Request, progress ProgressReporter) (*Result, error) {
	report := func(step, label string) {
		if progress != nil {
			progress(ProgressEvent{Step: step, Label: label})
		}
	}
	tier, ok := nodescore.Tiers[req.Tier]
	if !ok {
		return nil, &internalerrors.ValidationError{Field: "tier", Message: fmt.Sprintf("unknown tier %q", req.Tier)}
	}

	if _, ok := proxmox.TemplateOffsets[req.OSTemplate]; !ok {
		return nil, &internalerrors.ValidationError{Field: "os_template", Message: fmt.Sprintf("unknown os_template %q", req.OSTemplate)}
	}

	// Member quota gate. Skipped for admins and for legacy/test paths that
	// don't supply an OwnerID. We check here — before IP reserve and clone —
	// so a flooding member can't burn cluster resources spinning up rejects.
	if !req.RequesterIsAdmin && req.OwnerID != nil {
		if !IsTierMemberAllowed(req.Tier) {
			return nil, &internalerrors.ValidationError{
				Field:   "tier",
				Message: fmt.Sprintf("tier %q is admin-only", req.Tier),
			}
		}
		var owned int64
		if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("owner_id = ?", *req.OwnerID).Count(&owned).Error; err != nil {
			return nil, fmt.Errorf("count owned vms: %w", err)
		}
		quotaCap := MemberMaxVMs
		if s.quota != nil {
			c, err := s.quota.EffectiveVMQuota(*req.OwnerID)
			if err != nil {
				return nil, fmt.Errorf("resolve effective vm quota: %w", err)
			}
			quotaCap = c
		}
		if int(owned) >= quotaCap {
			return nil, &internalerrors.ConflictError{
				Message: fmt.Sprintf("VM quota reached: members may own at most %d VMs at once", quotaCap),
			}
		}
	}

	// Resolve SSH key. The service may either reuse an existing vault entry
	// or create a new one (generate / BYO / default-fallback).
	sshKey, sshPrivateKey, err := s.resolveSSHKey(ctx, req)
	if err != nil {
		return nil, err
	}
	keyID := sshKey.ID
	keyName := sshKey.Name
	sshPubKey := sshKey.PublicKey

	// Step 0: resolve network attachment.
	//
	// Bridge override (admin-only escape hatch): when req.Bridge is
	// set, the VM lands on that bridge directly with the global IP
	// pool, bypassing per-user SDN entirely. Used by admins for
	// management VMs that need cluster-LAN reachability. Non-admins
	// trying this get a 403 — isolation is enforced.
	//
	// Otherwise: resolve the per-user SDN subnet (when SDN is enabled
	// and the resolver is wired). Returns nil for the legacy vmbr0
	// path. Lazy-creates a "default" subnet on first provision when
	// neither SubnetID nor SubnetName is specified — same UX as SSH
	// keys, where a fresh user never gets blocked at the gate.
	var subnet *db.UserSubnet
	bridgeOverride := ""
	switch {
	case req.Bridge != "":
		if !req.RequesterIsAdmin {
			return nil, &internalerrors.ValidationError{
				Field:   "bridge",
				Message: "only admins can attach a VM directly to a cluster bridge — use a subnet instead",
			}
		}
		bridgeOverride = req.Bridge
		// Admin override: skip subnet resolution. Falls through to
		// the legacy global pool + cloud-init gateway path.
	case s.subnetResolver != nil && req.OwnerID != nil:
		subnet, err = s.subnetResolver.ResolveForProvision(ctx, *req.OwnerID, req.SubnetID, req.SubnetName)
		if err != nil {
			return nil, err
		}
	}

	// Step 1: reserve IP. SDN path uses the per-subnet pool (scoped
	// to one user-subnet's CIDR); legacy path uses the global pool.
	// defer release on any later failure — same shape both paths.
	var ip string
	if subnet != nil {
		ip, err = s.pool.ReserveInSubnet(ctx, subnet.VNet, req.Hostname)
	} else {
		ip, err = s.pool.Reserve(ctx, req.Hostname)
	}
	if err != nil {
		if errors.Is(err, ippool.ErrPoolExhausted) {
			return nil, &internalerrors.ConflictError{Message: "no free IP addresses in pool"}
		}
		return nil, fmt.Errorf("reserve ip: %w", err)
	}
	released := false
	defer func() {
		if !released {
			if subnet != nil {
				_ = s.pool.ReleaseInSubnet(context.Background(), subnet.VNet, ip)
			} else {
				_ = s.pool.Release(context.Background(), ip)
			}
		}
	}()

	// Step 1b: verify the picked IP is not already held by a VM elsewhere on
	// the cluster (catches the cross-instance race where two Nimbus instances
	// each picked the same lowest-free IP from their independent SQLite caches).
	// On race-loss, releases the local reservation and tries the next free IP,
	// up to maxVerifyAttempts.
	//
	// Skipped for SDN VMs — per-subnet pools are scoped per user and
	// drawn from a non-overlapping carve of the supernet, so the
	// cross-Nimbus-instance race verifyAndRetryReserve guards against
	// can't fire there.
	if subnet == nil {
		ip, err = s.verifyAndRetryReserve(ctx, ip, req.Hostname)
		if err != nil {
			return nil, err
		}
	}

	// Step 1c: register a Gopher machine BEFORE the clone. Provision-time
	// SSH exposure rides on the machine's public_ssh flag (no subdomain, no
	// target_port — those are for the post-provision tunnel surface). On
	// any failure here we soft-fail and keep going without a tunnel —
	// "Gopher failure must not fail the VM provision" (design §10).
	var (
		machineObj  *tunnel.Machine
		tunnelError string
	)
	if req.PublicTunnel && s.tunnels != nil {
		m, terr := s.tunnels.CreateMachine(ctx, tunnel.CreateMachineRequest{PublicSSH: true})
		if terr != nil {
			log.Printf("tunnel: register machine failed (continuing without tunnel): %v", terr)
			tunnelError = "tunnel registration failed: " + terr.Error()
		} else {
			machineObj = m
		}
	}
	// Deferred best-effort cleanup. Cleared when we reach the success path so
	// the machine survives. Mirrors the IP-release pattern.
	machineToCleanup := ""
	if machineObj != nil {
		machineToCleanup = machineObj.ID
	}
	defer func() {
		if machineToCleanup != "" && s.tunnels != nil {
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.tunnels.DeleteMachine(delCtx, machineToCleanup); err != nil {
				log.Printf("tunnel: cleanup delete failed for machine %s: %v", machineToCleanup, err)
			}
		}
	}()

	// Step 2: gather cluster snapshot and score, restricted to nodes that
	// have a template for the requested OS. The per-node templateVMID lookup
	// uses the node_templates table (filled in by bootstrap) so we don't have
	// to fan out a TemplateExists call per node.
	//
	// RequiredTags is the host-aggregate filter — operator tags
	// hardware, user opts in at provision time. Empty = no constraint
	// (score by capacity only). Persisted on db.VM so drain
	// replacement applies the same filter.
	requiredTags := splitRequiredTags(req.RequiredTags)
	target, templateVMID, err := s.pickNode(ctx, tier, req.OSTemplate, requiredTags)
	if err != nil {
		return nil, err
	}
	report(StepReserveIP, "Reserved IP and selected node")

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
	report(StepCloneTpl, "Cloned golden template")

	// Defer destroy on any later failure. Once the clone task settles
	// the VM exists in Proxmox; bailing out at any subsequent step
	// (cloud-init, disk resize, start, etc.) would otherwise leave a
	// half-configured ghost VM that operators have to clean up by hand.
	// Mirrors the machineToCleanup pattern above.
	cleanupVMID := newVMID
	defer func() {
		if cleanupVMID == 0 {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if upid, err := s.px.StopVM(cleanCtx, target, cleanupVMID); err == nil {
			_ = s.px.WaitForTask(cleanCtx, target, upid, s.cfg.PollInterval)
		}
		if upid, err := s.px.DestroyVM(cleanCtx, target, cleanupVMID); err != nil {
			if !isAlreadyGone(err) {
				log.Printf("provision cleanup: destroy vmid=%d on %s failed: %v", cleanupVMID, target, err)
			}
		} else {
			_ = s.px.WaitForTask(cleanCtx, target, upid, s.cfg.PollInterval)
		}
	}()

	// Bridge / VNet override: clones inherit the template's
	// `net0=virtio,bridge=vmbr0`. Switch to the user's subnet VNet
	// (or the admin's chosen bridge) before configuring cloud-init.
	// vmbr0 is what the template already has, so we only call
	// SetVMNetwork when the target bridge actually differs. Proxmox
	// auto-generates a fresh MAC when net0 is rewritten without a
	// macaddr= component, so two cloned VMs never share a MAC across
	// isolated VLANs (no L2 collision, but surprising during debug).
	switch {
	case subnet != nil:
		if err := s.px.SetVMNetwork(ctx, target, newVMID, "net0", subnet.VNet); err != nil {
			return nil, fmt.Errorf("set vm network to %s: %w", subnet.VNet, err)
		}
	case bridgeOverride != "" && bridgeOverride != "vmbr0":
		if err := s.px.SetVMNetwork(ctx, target, newVMID, "net0", bridgeOverride); err != nil {
			return nil, fmt.Errorf("set vm network to %s: %w", bridgeOverride, err)
		}
	}

	// Step 4: cloud-init. Per-subnet path uses the subnet's gateway +
	// CIDR prefix; legacy path uses the cluster-wide values from
	// NetworkSettings.
	gatewayIP, prefixLen := s.GatewayIP(), s.PrefixLen()
	if subnet != nil {
		gatewayIP = subnet.Gateway
		prefixLen = vnetmgr.PrefixLenOf(subnet)
	}
	username := proxmox.TemplateUsername(req.OSTemplate)

	// Per-VM cloud-init ISO. Built in Go, uploaded as content=iso,
	// attached at ide2 as a CD-ROM. cloud-init's NoCloud datasource
	// finds it on /dev/sr0 (label `cidata`) and processes the
	// user-data — including the qemu-guest-agent install that
	// makes WaitForIP's agent path actually work.
	//
	// Replaces the entire Proxmox-auto-cloud-init flow. The
	// SetCloudInit call below populates the same fields decoratively
	// for visibility in PVE's Cloud-Init tab; the actual delivery is
	// our ISO. SetVMDescription warns admins not to edit the tab.
	cidata := s.installCIDataISO(ctx, target, newVMID, CIDataInput{
		Hostname:        req.Hostname,
		Username:        username,
		SSHPublicKey:    sshPubKey,
		ConsolePassword: generateConsolePassword(),
		IP:              ip,
		PrefixLen:       prefixLen,
		Gateway:         gatewayIP,
		Nameservers:     splitNameservers(s.cfg.Nameserver),
	})

	cloudInit := proxmox.CloudInitConfig{
		CIUser:       username,
		SSHKeys:      sshPubKey,
		IPConfig0:    fmt.Sprintf("ip=%s/%d,gw=%s", ip, prefixLen, gatewayIP),
		Nameserver:   s.cfg.Nameserver,
		SearchDomain: s.cfg.SearchDomain,
		Cores:        tier.CPU,
		Memory:       int(tier.MemMB),
		CPU:          s.cfg.CPUType,
	}
	if err := s.px.SetCloudInit(ctx, target, newVMID, cloudInit); err != nil {
		return nil, fmt.Errorf("set cloud-init: %w", err)
	}

	// Stamp the Nimbus marker so other instances sharing the cluster can
	// recognize this VM as foreign-Nimbus-managed. Tier/OS go into the VM's
	// description as a hidden HTML comment — keeps the Proxmox dashboard
	// from filling up with three colored chips per VM. Both writes are
	// non-fatal — the VM is otherwise complete and a follow-up backfill
	// will retry on the next startup.
	if err := s.px.SetVMTags(ctx, target, newVMID, proxmox.EncodeNimbusTags()); err != nil {
		log.Printf("set tags vmid=%d: %v (continuing)", newVMID, err)
	}
	desc := proxmox.EncodeNimbusDescription(req.Tier, req.OSTemplate)
	if cidata.Attached {
		desc = pveCloudInitTabWarning + "\n\n" + desc
	}
	if err := s.px.SetVMDescription(ctx, target, newVMID, desc); err != nil {
		log.Printf("set description vmid=%d: %v (continuing)", newVMID, err)
	}

	// Step 5: resize the disk to tier spec. Proxmox accepts both absolute
	// (e.g. "15G") and additive ("+5G") sizes; we use absolute. The earlier
	// "+(tier - 10)" form quietly assumed every cloud image shipped at a
	// 10 GiB base, but Ubuntu noble actually ships at 3.5 GiB — so a
	// "small" (15 GiB) tier was coming up at 8.5 GiB, large at 53.5 GiB,
	// etc. Absolute sizing makes the result match the tier regardless of
	// which image we cloned from.
	if tier.DiskGB > 0 {
		if err := s.px.ResizeDisk(ctx, target, newVMID, "scsi0", fmt.Sprintf("%dG", tier.DiskGB)); err != nil {
			return nil, fmt.Errorf("resize disk: %w", err)
		}
	}
	report(StepConfigure, "Configured cloud-init and disk")

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
	report(StepStartVM, "Started VM")

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
	//     return error; the deferred cleanup destroys the half-built
	//     Proxmox VM so we don't leave a hung shell behind.
	var warning string
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.IPReadyTimeout)
	defer cancel()
	diag, err := WaitForIP(waitCtx, s.px, target, newVMID, ip, s.cfg.PollInterval)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Post-mortem: fetch the VM config under a fresh ctx so we
			// can verify what's actually configured. Most informative
			// when the agent never responded — surfaces "is `agent`
			// even enabled on this clone?" without operator grep.
			cfgCtx, cfgCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if cfg, cerr := s.px.GetVMConfig(cfgCtx, target, newVMID); cerr == nil {
				if v, ok := cfg["agent"].(string); ok {
					diag.AgentConfig = v
				}
			}
			cfgCancel()
			// Server-side log the full diagnostic so an operator grep
			// can see exactly what we observed. User-facing warning
			// gets the one-line summary.
			log.Printf("WaitForIP timeout vmid=%d node=%s expected=%s elapsed=%s agent_seen=%t agent_ips=%v first_err=%q agent_config=%q tcp_reachable=%t",
				newVMID, target, ip, diag.Elapsed, diag.AgentSeen, diag.AgentIPs, diag.FirstAgentErr, diag.AgentConfig, diag.TCPReachable)
			warning = fmt.Sprintf(
				"VM is up but the readiness check timed out after %s: %s. "+
					"Credentials are valid — re-check from the VM's page in a minute or two.",
				s.cfg.IPReadyTimeout, diag.Summary())
			// fall through to the success path
		} else {
			return nil, fmt.Errorf("wait for ready: %w", err)
		}
	}
	report(StepWaitAgent, "Guest agent ready")

	// Step 7b: Gopher tunnel bootstrap. We run it via qemu-guest-agent,
	// not SSH — the data path is virtio-serial through the hypervisor,
	// so isolation-subnet VMs bootstrap identically to vmbr0 ones. The
	// "warning" branch (WaitForIP soft-success) used to skip bootstrap
	// because we couldn't SSH; agent/exec doesn't care about L3, so
	// the only thing the warning gates now is whether the agent was
	// healthy enough for WaitForIP to confirm — which it wasn't. Stay
	// conservative: skip with a recovery note when warning is set.
	// All failures are recorded as tunnel_error — VM provision never
	// fails for tunnel reasons (design §10).
	tunnelURL := ""
	if machineObj != nil {
		switch {
		case warning != "":
			tunnelError = "Guest agent did not confirm readiness — bootstrap skipped." +
				" Re-run manually once the VM is up:\n  curl " + machineObj.BootstrapURL + " | sh"
		default:
			if berr := runTunnelBootstrap(ctx, s.px, target, newVMID, machineObj.BootstrapURL, req.Hostname); berr != nil {
				log.Printf("tunnel: bootstrap failed: %v", berr)
				tunnelError = "tunnel bootstrap failed: " + berr.Error()
			} else {
				active, perr := s.waitMachineActive(ctx, machineObj.ID)
				switch {
				case perr != nil:
					log.Printf("tunnel: poll failed: %v", perr)
					tunnelError = "tunnel did not become active: " + perr.Error()
				case active.PublicSSHHost != "" && active.PublicSSHPort != 0:
					tunnelURL = fmt.Sprintf("%s:%d", active.PublicSSHHost, active.PublicSSHPort)
				default:
					tunnelError = "machine active but no public SSH host/port returned"
				}
			}
		}
	}

	// Step 7c: GPU env bootstrap. Opt-in — only fires when the caller asked
	// for it AND the GPU plane is configured cluster-wide. Failures are
	// logged, never block provisioning. Same agent/exec data path as the
	// tunnel bootstrap.
	gpuCfg := s.gpuBootstrapConfig()
	if req.EnableGPU && gpuCfg.BaseURL != "" && warning == "" {
		if berr := runGPUBootstrap(ctx, s.px, target, newVMID, username, gpuCfg); berr != nil {
			log.Printf("gpu bootstrap vmid=%d: %v (continuing)", newVMID, berr)
		}
	}

	// Step 8: commit. IP transitions reserved -> allocated; VM row written.
	// SDN path uses the per-subnet pool variant; legacy path uses the
	// global one. Either way, mark-allocated semantics are identical.
	if subnet != nil {
		if err := s.pool.MarkAllocatedInSubnet(ctx, subnet.VNet, ip, newVMID); err != nil {
			return nil, fmt.Errorf("mark allocated: %w", err)
		}
	} else {
		if err := s.pool.MarkAllocated(ctx, ip, newVMID); err != nil {
			return nil, fmt.Errorf("mark allocated: %w", err)
		}
	}
	released = true // success path — do NOT run the deferred release
	cleanupVMID = 0 // success path — keep the Proxmox VM

	// The encrypted private key (if any) lives on the ssh_keys row referenced
	// by SSHKeyID — not on the VM itself anymore.
	vm := &db.VM{
		VMID:         newVMID,
		Hostname:     req.Hostname,
		IP:           ip,
		Node:         target,
		Tier:         req.Tier,
		RequiredTags: req.RequiredTags,
		OSTemplate:   req.OSTemplate,
		Username:     username,
		Status:       "running",
		OwnerID:      req.OwnerID,
		SSHKeyID:     &keyID,
		KeyName:      keyName,
		SSHPubKey:    sshPubKey,
		ErrorMsg:     warning, // doubles as a soft-warning record on the persisted row
	}
	if subnet != nil {
		sid := subnet.ID
		vm.SubnetID = &sid
	}
	if machineObj != nil {
		vm.TunnelID = machineObj.ID
	}
	vm.TunnelURL = tunnelURL
	vm.TunnelError = tunnelError
	if err := s.db.WithContext(ctx).Create(vm).Error; err != nil {
		// VM is up but we couldn't write the row — log via the error path. The
		// IP is already marked allocated so we don't strand it.
		return nil, fmt.Errorf("persist vm: %w", err)
	}
	machineToCleanup = "" // success — keep the machine

	res := &Result{
		ID:            vm.ID,
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
		TunnelURL:     tunnelURL,
		TunnelError:   tunnelError,
	}
	if subnet != nil {
		res.SubnetName = subnet.Name
		res.SubnetCIDR = subnet.Subnet
	}
	res.ConsolePassword = cidata.ConsolePassword
	res.CloudInitError = cidata.Error
	return res, nil
}

// BackfillNimbusMetadata walks every persisted VM row and ensures the Proxmox
// VM carries (a) the bare `nimbus` marker tag and (b) the structured tier/OS
// marker inside its description. Legacy `nimbus-tier-*` / `nimbus-os-*` tags
// from older Nimbus builds are stripped in the same pass — the Proxmox UI
// shows one chip per VM instead of three.
//
// Idempotent: a VM whose tags and description are already correct is skipped
// without any API writes. Returns the number of VMs whose Proxmox config was
// touched. Per-VM failures are logged but do not abort the walk.
// CountVMsOnSubnet reports how many active VMs reference a given user
// subnet by ID. Powers vnetmgr's "refuse-delete-while-VMs-attached"
// gate. Soft-deleted VMs are excluded by gorm's default scope.
func (s *Service) CountVMsOnSubnet(ctx context.Context, subnetID uint) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&db.VM{}).
		Where("subnet_id = ?", subnetID).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count vms on subnet %d: %w", subnetID, err)
	}
	return int(n), nil
}

func (s *Service) BackfillNimbusMetadata(ctx context.Context) (int, error) {
	var vms []db.VM
	if err := s.db.WithContext(ctx).Find(&vms).Error; err != nil {
		return 0, fmt.Errorf("list vms: %w", err)
	}
	updated := 0
	for _, vm := range vms {
		if vm.Tier == "" || vm.OSTemplate == "" || vm.Node == "" || vm.VMID == 0 {
			continue
		}
		cfg, err := s.px.GetVMConfig(ctx, vm.Node, vm.VMID)
		if err != nil {
			log.Printf("backfill: get config vmid=%d on %s: %v (skipping)", vm.VMID, vm.Node, err)
			continue
		}
		var existingTags []string
		if raw, _ := cfg["tags"].(string); raw != "" {
			existingTags = proxmox.SplitTags(raw)
		}
		existingDesc, _ := cfg["description"].(string)

		wantTags := proxmox.MergeNimbusTags(existingTags)
		wantDesc := proxmox.MergeNimbusDescription(existingDesc, vm.Tier, vm.OSTemplate)

		tagsChanged := !tagsEqual(existingTags, wantTags)
		descChanged := existingDesc != wantDesc
		if !tagsChanged && !descChanged {
			continue
		}
		if tagsChanged {
			if err := s.px.SetVMTags(ctx, vm.Node, vm.VMID, wantTags); err != nil {
				log.Printf("backfill: set tags vmid=%d on %s: %v (skipping)", vm.VMID, vm.Node, err)
				continue
			}
		}
		if descChanged {
			if err := s.px.SetVMDescription(ctx, vm.Node, vm.VMID, wantDesc); err != nil {
				log.Printf("backfill: set description vmid=%d on %s: %v (skipping)", vm.VMID, vm.Node, err)
				continue
			}
		}
		updated++
	}
	return updated, nil
}

// tagsEqual compares two tag slices for set equality (order-insensitive).
func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, t := range a {
		seen[t] = true
	}
	for _, t := range b {
		if !seen[t] {
			return false
		}
	}
	return true
}

// List returns persisted VM rows. When ownerID is non-nil the result is
// strictly scoped to rows owned by that user. Passing nil returns every row
// (used by background reconciliation, not by user-facing endpoints).
//
// Legacy rows with owner_id IS NULL are NOT returned to any user — at
// startup BackfillOwnership reassigns them to the first admin so they
// surface naturally on that admin's My Machines.
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

// ListWithLiveStatus is List enriched with live Proxmox power state so a VM
// stopped/paused outside Nimbus surfaces correctly on the user's dashboard.
// The DB-side `vm.status` is otherwise only written when Nimbus itself drives
// a lifecycle op, so it goes stale the moment someone uses the Proxmox UI or
// the VM crashes.
//
// Rows with no matching Proxmox record keep their DB status unchanged — that
// preserves "provisioning" mid-clone (the VM doesn't exist on Proxmox yet)
// and "failed" rows that never got past clone. If the Proxmox call fails the
// DB rows are returned as-is so a transient outage doesn't blank the page.
func (s *Service) ListWithLiveStatus(ctx context.Context, ownerID *uint) ([]db.VM, error) {
	vms, err := s.List(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		return vms, nil
	}
	cluster, err := s.px.GetClusterVMs(ctx)
	if err != nil {
		log.Printf("list vms: live status lookup failed, returning DB status: %v", err)
		return vms, nil
	}
	live := make(map[int]string, len(cluster))
	for _, c := range cluster {
		live[c.VMID] = c.Status
	}
	for i := range vms {
		if status, ok := live[vms[i].VMID]; ok && status != "" {
			vms[i].Status = status
		}
	}
	return vms, nil
}

// BackfillOwnership assigns every VM with a NULL owner_id to the
// lowest-ID admin on the instance. Pre-ownership-tracking VMs (provisioned
// before this feature shipped) get bound to the original setup admin so
// they appear on someone's My Machines and become deletable through the
// regular UI flow.
//
// Idempotent: re-running after every row already has an owner is a no-op.
// Returns the number of rows updated, or 0 (with no error) when there's
// no admin yet — typical for an instance still in the setup wizard.
func (s *Service) BackfillOwnership(ctx context.Context) (int64, error) {
	var admin db.User
	err := s.db.WithContext(ctx).
		Where("is_admin = ?", true).
		Order("id ASC").
		First(&admin).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("find first admin: %w", err)
	}

	res := s.db.WithContext(ctx).Model(&db.VM{}).
		Where("owner_id IS NULL").
		Update("owner_id", admin.ID)
	if res.Error != nil {
		return 0, fmt.Errorf("backfill owner_id: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// GetPrivateKey returns the decrypted private key for a VM, if one is
// available. Reads through the ssh_keys vault via SSHKeyID. Returns NotFound
// when the VM has no linked key, or the linked key has no vaulted private
// half (e.g. user imported a public-only key, or deleted the key after
// provision).
//
// requesterID enforces VM ownership — non-nil values must match vm.OwnerID
// or NotFound is returned (no info leak about other users' VMs). Pass nil
// for trusted internal callers; the handler always passes the signed-in user.
func (s *Service) GetPrivateKey(ctx context.Context, id uint, requesterID *uint) (keyName, privateKey string, err error) {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return "", "", fmt.Errorf("get vm %d: %w", id, err)
	}
	if requesterID != nil && (vm.OwnerID == nil || *vm.OwnerID != *requesterID) {
		return "", "", &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
	}
	if vm.SSHKeyID == nil {
		return "", "", &internalerrors.NotFoundError{
			Resource: "private_key",
			ID:       fmt.Sprintf("vm:%d", id),
		}
	}
	return s.keys.GetPrivateKey(ctx, *vm.SSHKeyID, requesterID)
}

// Get returns a single VM by row ID.
//
// requesterID enforces ownership — non-nil values must match vm.OwnerID or
// NotFound is returned (no info leak about other users' VMs). Pass nil for
// trusted internal callers; handlers always pass the signed-in user. Mirrors
// GetPrivateKey's gating semantics.
// FindByVMID looks up a VM by its Proxmox VMID (not the Nimbus DB
// row id). Used by audit emit sites that have the VMID from the URL
// (e.g. /cluster/vms/{node}/{vmid}/{op}) and want to enrich the
// audit row with hostname. Returns NotFoundError when no row matches —
// foreign or external VMs are legitimately absent here.
func (s *Service) FindByVMID(ctx context.Context, vmid int) (*db.VM, error) {
	var vm db.VM
	if err := s.db.WithContext(ctx).Where("vmid = ?", vmid).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("vmid=%d", vmid)}
		}
		return nil, fmt.Errorf("get vm vmid=%d: %w", vmid, err)
	}
	return &vm, nil
}

func (s *Service) Get(ctx context.Context, id uint, requesterID *uint) (*db.VM, error) {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return nil, fmt.Errorf("get vm %d: %w", id, err)
	}
	if requesterID != nil && (vm.OwnerID == nil || *vm.OwnerID != *requesterID) {
		return nil, &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
	}
	return &vm, nil
}

// Delete tears down a VM end-to-end: stop on Proxmox if running, destroy with
// disk purge, release the IP back to the pool, and hard-delete the DB row.
//
// Authorization is strict: returns NotFound (not Forbidden) when the row
// either has no owner (legacy / pre-ownership) or belongs to a different
// user. NotFound — rather than 403 — avoids disclosing existence of other
// users' VMs and keeps legacy VMs immutable through this code path.
//
// Order of operations is Proxmox → IP → DB so a partial failure leaves the
// row + IP intact and the user can retry. Stop failures are tolerated as
// long as the destroy itself succeeds (covers the "already stopped" race).
func (s *Service) Delete(ctx context.Context, id, requesterID uint) error {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return fmt.Errorf("get vm %d: %w", id, err)
	}
	if vm.OwnerID == nil || *vm.OwnerID != requesterID {
		return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
	}
	return s.deleteVM(ctx, &vm)
}

// VMLifecycleOp identifies one of the four user-facing power operations.
// Used by the user-scoped lifecycle wrapper so the handler doesn't have to
// know which Proxmox endpoint corresponds to each verb.
type VMLifecycleOp string

const (
	VMOpStart    VMLifecycleOp = "start"
	VMOpShutdown VMLifecycleOp = "shutdown" // graceful (guest agent / ACPI)
	VMOpStop     VMLifecycleOp = "stop"     // force (pull plug)
	VMOpReboot   VMLifecycleOp = "reboot"
)

// LifecycleOp issues a power operation against a VM the requester owns.
// Same ownership semantics as Delete: a non-owning caller gets NotFound so
// existence isn't disclosed. On success vms.status is updated optimistically
// (the reconciler will correct if the change actually fails on Proxmox after
// the task UPID is returned).
func (s *Service) LifecycleOp(ctx context.Context, id, requesterID uint, op VMLifecycleOp) error {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return fmt.Errorf("get vm %d: %w", id, err)
	}
	if vm.OwnerID == nil || *vm.OwnerID != requesterID {
		return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
	}
	return s.runLifecycleOp(ctx, &vm, op)
}

// AdminLifecycleOp issues a power operation without an owner gate. Used by
// the admin cluster handler so an admin can power-cycle any local VM (foreign
// and external are reachable via the by-(node,vmid) cluster endpoints).
func (s *Service) AdminLifecycleOp(ctx context.Context, id uint, op VMLifecycleOp) error {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return fmt.Errorf("get vm %d: %w", id, err)
	}
	return s.runLifecycleOp(ctx, &vm, op)
}

// AdminLifecycleByVMID issues a power operation against a Proxmox VM that
// may not have a local DB row (foreign / external). When a local row exists
// for this (node, vmid), its status is updated optimistically; otherwise the
// op is purely a Proxmox API call.
func (s *Service) AdminLifecycleByVMID(ctx context.Context, node string, vmid int, op VMLifecycleOp) error {
	if node == "" || vmid <= 0 {
		return &internalerrors.ValidationError{Field: "vmid", Message: "node and vmid are required"}
	}
	var (
		upid       string
		err        error
		nextStatus string
	)
	switch op {
	case VMOpStart:
		upid, err = s.px.StartVM(ctx, node, vmid)
		nextStatus = "running"
	case VMOpShutdown:
		upid, err = s.px.ShutdownVM(ctx, node, vmid)
		nextStatus = "stopped"
	case VMOpStop:
		upid, err = s.px.StopVM(ctx, node, vmid)
		nextStatus = "stopped"
	case VMOpReboot:
		upid, err = s.px.RebootVM(ctx, node, vmid)
		nextStatus = "running"
	default:
		return &internalerrors.ValidationError{Field: "op", Message: fmt.Sprintf("unknown lifecycle op %q", op)}
	}
	if err != nil && !isAlreadyInTargetState(op, err) {
		return fmt.Errorf("%s vm %d on %s: %w", op, vmid, node, err)
	}
	if err == nil && upid != "" {
		if waitErr := s.px.WaitForTask(ctx, node, upid, s.cfg.PollInterval); waitErr != nil {
			if !isAlreadyInTargetState(op, waitErr) {
				return fmt.Errorf("%s task vm %d: %w", op, vmid, waitErr)
			}
			log.Printf("admin lifecycle %s vm=%d node=%s: task surfaced a benign already-in-state error: %v", op, vmid, node, waitErr)
		}
	}
	// Best-effort optimistic status sync for local rows.
	if err := s.db.WithContext(ctx).Model(&db.VM{}).
		Where("vmid = ? AND node = ?", vmid, node).
		Update("status", nextStatus).Error; err != nil {
		log.Printf("admin lifecycle %s vm=%d node=%s: status update failed: %v", op, vmid, node, err)
	}
	return nil
}

// runLifecycleOp dispatches op to the right Proxmox endpoint, waits for the
// task, then updates vms.status. Returns ValidationError for an unknown op.
//
// Idempotency: a Start on an already-running VM (or a Stop/Shutdown on an
// already-stopped one) returns no-error and still updates the local status.
// Proxmox surfaces these as either an HTTP 500 from the API call (handled by
// isAlreadyInTargetState's HTTPError branch) or as a task-failed string from
// WaitForTask (handled by the substring branch), depending on whether
// pveproxy or qm/qmeventd notices first. The user's intent — "this VM should
// be in state X" — is satisfied either way; surfacing a 500 from Start when
// the VM is already up is just confusing.
func (s *Service) runLifecycleOp(ctx context.Context, vm *db.VM, op VMLifecycleOp) error {
	var (
		upid       string
		err        error
		nextStatus string
	)
	switch op {
	case VMOpStart:
		upid, err = s.px.StartVM(ctx, vm.Node, vm.VMID)
		nextStatus = "running"
	case VMOpShutdown:
		upid, err = s.px.ShutdownVM(ctx, vm.Node, vm.VMID)
		nextStatus = "stopped"
	case VMOpStop:
		upid, err = s.px.StopVM(ctx, vm.Node, vm.VMID)
		nextStatus = "stopped"
	case VMOpReboot:
		upid, err = s.px.RebootVM(ctx, vm.Node, vm.VMID)
		nextStatus = "running"
	default:
		return &internalerrors.ValidationError{Field: "op", Message: fmt.Sprintf("unknown lifecycle op %q", op)}
	}
	if err != nil && !isAlreadyInTargetState(op, err) {
		return fmt.Errorf("%s vm %d on %s: %w", op, vm.VMID, vm.Node, err)
	}
	if err == nil && upid != "" {
		if waitErr := s.px.WaitForTask(ctx, vm.Node, upid, s.cfg.PollInterval); waitErr != nil {
			if !isAlreadyInTargetState(op, waitErr) {
				return fmt.Errorf("%s task vm %d: %w", op, vm.VMID, waitErr)
			}
			// Task failed but only because the VM was already in the
			// target state — fall through and let the status write run.
			log.Printf("lifecycle %s vm=%d: task surfaced a benign already-in-state error: %v", op, vm.ID, waitErr)
		}
	}
	if err := s.db.WithContext(ctx).Model(&db.VM{}).Where("id = ?", vm.ID).
		Update("status", nextStatus).Error; err != nil {
		log.Printf("lifecycle %s vm=%d: status update failed (proxmox already applied): %v", op, vm.ID, err)
	}
	return nil
}

// isAlreadyInTargetState reports whether a Proxmox-side error means "the VM
// is already in the state op was trying to put it in" — the kind of failure
// a user-facing Start/Shutdown click should treat as success rather than a
// 500.
//
// Two surfaces produce this kind of error and they look different:
//   - The status/start | status/stop endpoint can return HTTP 500 with a body
//     like "VM N is not running" / "VM N already running".
//   - The async task can succeed-then-fail in qmeventd with the same wording
//     in ExitStatus, surfacing as an opaque "task ... failed: VM N ..."
//     wrapped error from WaitForTask.
//
// We accept both. Reboot stays strict — "VM not running" on a reboot click is
// a real misuse the user should see.
func isAlreadyInTargetState(op VMLifecycleOp, err error) bool {
	if err == nil {
		return false
	}
	body := err.Error()
	if httpErr := (*proxmox.HTTPError)(nil); errors.As(err, &httpErr) {
		body = httpErr.Body
	}
	body = strings.ToLower(body)
	switch op {
	case VMOpStart:
		return strings.Contains(body, "already running")
	case VMOpShutdown, VMOpStop:
		return strings.Contains(body, "not running") ||
			strings.Contains(body, "already stopped")
	}
	return false
}

// AdminDelete destroys a VM regardless of who owns it. Same semantics as
// Delete (stop → Proxmox destroy → release IP → hard-delete row), but no
// owner gate. Intended for the admin Dashboard cluster view.
func (s *Service) AdminDelete(ctx context.Context, id uint) error {
	var vm db.VM
	if err := s.db.WithContext(ctx).First(&vm, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &internalerrors.NotFoundError{Resource: "vm", ID: fmt.Sprintf("%d", id)}
		}
		return fmt.Errorf("get vm %d: %w", id, err)
	}
	return s.deleteVM(ctx, &vm)
}

// TransferUserVMs reparents every VM currently owned by fromID to toID.
// Used by the user-deletion flow when the admin opts to take ownership
// of the deleted user's VMs instead of destroying them. Returns the
// number of rows updated. No Proxmox interaction — this is purely a
// metadata change in the local DB.
func (s *Service) TransferUserVMs(ctx context.Context, fromID, toID uint) (int64, error) {
	res := s.db.WithContext(ctx).Model(&db.VM{}).
		Where("owner_id = ?", fromID).
		Update("owner_id", toID)
	if res.Error != nil {
		return 0, fmt.Errorf("transfer vms %d -> %d: %w", fromID, toID, res.Error)
	}
	return res.RowsAffected, nil
}

// deleteVM holds the shared destroy sequence used by both the user-scoped
// Delete and the admin-scoped AdminDelete. Order: stop on Proxmox if running,
// destroy with disk purge, release the IP back to the pool, hard-delete the
// row. A partial failure leaves the row + IP intact for retry.
func (s *Service) deleteVM(ctx context.Context, vm *db.VM) error {
	// Best-effort stop. Proxmox returns an error if the VM is already stopped;
	// that's fine — destroy below will succeed regardless.
	if vm.Status == "running" {
		if upid, err := s.px.StopVM(ctx, vm.Node, vm.VMID); err == nil {
			_ = s.px.WaitForTask(ctx, vm.Node, upid, s.cfg.PollInterval)
		} else {
			log.Printf("delete vm %d: stop failed (continuing to destroy): %v", vm.VMID, err)
		}
	}

	// Destroy. If Proxmox says the VM is already gone (the user manually
	// removed it on the cluster), treat that as success and proceed to clean
	// up our local state.
	upid, err := s.px.DestroyVM(ctx, vm.Node, vm.VMID)
	if err != nil && !isAlreadyGone(err) {
		return fmt.Errorf("destroy vm on proxmox: %w", err)
	}
	if err == nil {
		if waitErr := s.px.WaitForTask(ctx, vm.Node, upid, s.cfg.PollInterval); waitErr != nil {
			return fmt.Errorf("wait for destroy task: %w", waitErr)
		}
	}

	if vm.IP != "" {
		if err := s.releaseVMIP(ctx, vm); err != nil {
			log.Printf("delete vm %d: release ip %s: %v", vm.VMID, vm.IP, err)
		}
	}

	// Clean up the per-VM cloud-init ISO. Best-effort — leftover
	// ISOs are harmless (~few KB each), but cleanup keeps the
	// storage list tidy in the PVE UI.
	s.deleteCIDataISO(ctx, vm.Node, vm.VMID)

	if err := s.db.WithContext(ctx).Unscoped().Delete(vm).Error; err != nil {
		return fmt.Errorf("delete vm row %d: %w", vm.ID, err)
	}
	return nil
}

// releaseVMIP returns the VM's IP to the right pool — per-subnet for
// SDN VMs (vm.SubnetID set), global for legacy vmbr0 VMs. Looking up
// the subnet by id gives us the VNet name needed by ReleaseInSubnet.
// Soft-failure (logged, not propagated) on a missing subnet — the row
// might have been deleted concurrently and the IP still needs freeing.
func (s *Service) releaseVMIP(ctx context.Context, vm *db.VM) error {
	if vm.SubnetID != nil {
		var sub db.UserSubnet
		if err := s.db.WithContext(ctx).First(&sub, *vm.SubnetID).Error; err == nil {
			return s.pool.ReleaseInSubnet(ctx, sub.VNet, vm.IP)
		}
		// Subnet row gone — fall through to global release as a
		// last-ditch attempt. Doesn't free the per-subnet row but
		// keeps "delete is idempotent" the contract.
		log.Printf("delete vm %d: subnet %d missing during release; falling back to global pool", vm.VMID, *vm.SubnetID)
	}
	return s.pool.Release(ctx, vm.IP)
}

// isAlreadyGone reports whether a Proxmox error means the VM no longer exists
// on the cluster (manually removed, race with another delete, etc.). Proxmox
// uses 500 with "does not exist" in the body for this case rather than 404,
// matching the GetVMConfig quirk.
func isAlreadyGone(err error) bool {
	if errors.Is(err, proxmox.ErrNotFound) {
		return true
	}
	var httpErr *proxmox.HTTPError
	if errors.As(err, &httpErr) && strings.Contains(httpErr.Body, "does not exist") {
		return true
	}
	return false
}

// verifyAndRetryReserve runs the verify-after-Reserve loop. Given an initial
// IP that the caller has already reserved, it verifies the IP is not held by
// another VM on the cluster; on race-loss it leapfrogs to the next free IP
// (Reserve → Release in that order, so Pool.Reserve doesn't hand back the
// same lowest-free IP that we just released) and retries — up to
// maxVerifyAttempts.
//
// On success returns the (possibly different) IP the caller should proceed
// with; the IP is left reserved.
//
// On failure releases the most recently held IP and returns an error along
// with that IP — the caller's deferred Release on the same variable is then
// an idempotent no-op against an already-free row.
func (s *Service) verifyAndRetryReserve(ctx context.Context, initialIP, hostname string) (string, error) {
	ip := initialIP
	for attempt := 1; ; attempt++ {
		free, holder, vErr := s.verifier.VerifyFree(ctx, ip)
		if vErr == nil && free {
			return ip, nil
		}
		if vErr != nil {
			log.Printf("verify ip %s failed: %v (attempt %d/%d)", ip, vErr, attempt, maxVerifyAttempts)
		} else {
			heldBy := -1
			if holder != nil {
				heldBy = *holder
			}
			log.Printf("ip %s already claimed by vmid=%d on cluster (attempt %d/%d)",
				ip, heldBy, attempt, maxVerifyAttempts)
		}

		if attempt >= maxVerifyAttempts {
			_ = s.pool.Release(ctx, ip)
			return ip, &internalerrors.ConflictError{
				Message: fmt.Sprintf("could not secure free IP after %d verification attempts", maxVerifyAttempts),
			}
		}

		// Leapfrog: keep the contested reservation in place while we Reserve a
		// different free IP, then release the contested one. Without this, the
		// Pool's "first free IP" selection would just hand back the same IP we
		// released a moment ago.
		next, err := s.pool.Reserve(ctx, hostname)
		if err != nil {
			_ = s.pool.Release(ctx, ip)
			if errors.Is(err, ippool.ErrPoolExhausted) {
				return ip, &internalerrors.ConflictError{Message: "no free IP addresses in pool"}
			}
			return ip, fmt.Errorf("reserve ip retry: %w", err)
		}
		if err := s.pool.Release(ctx, ip); err != nil {
			_ = s.pool.Release(ctx, next)
			return next, fmt.Errorf("release contested ip %s: %w", ip, err)
		}
		ip = next
	}
}

// resolveSSHKey returns the SSHKey row to use for this VM and, if a fresh
// private key was generated as part of this provision, the plaintext private
// half so it can be returned to the user once.
//
// Cases:
//   - SSHKeyID set        → load the named row from the vault.
//   - GenerateKey=true    → mint a new keypair, persist as a new vault row.
//   - SSHPubKey set       → import (with optional PrivKey vaulted) as a new vault row.
//   - none of the above   → use the owner's default key, if any.
func (s *Service) resolveSSHKey(ctx context.Context, req Request) (*db.SSHKey, string, error) {
	switch {
	case req.SSHKeyID != nil:
		// Trusted internal flow: the entry handler has already authorized
		// the requester. Bypass the ownership check via nil so the
		// provision flow doesn't have to thread requester semantics through
		// every call site.
		row, err := s.keys.Get(ctx, *req.SSHKeyID, nil)
		if err != nil {
			return nil, "", err
		}
		return row, "", nil

	case req.GenerateKey:
		row, err := s.keys.Create(ctx, sshkeys.CreateRequest{
			Name:     "nimbus-" + req.Hostname,
			Generate: true,
			OwnerID:  req.OwnerID,
		})
		if err != nil {
			return nil, "", err
		}
		// Pull the plaintext back out so the API response can show it once.
		_, priv, err := s.keys.GetPrivateKey(ctx, row.ID, nil)
		if err != nil {
			return nil, "", fmt.Errorf("retrieve generated key: %w", err)
		}
		return row, priv, nil

	case req.SSHPubKey != "":
		row, err := s.keys.Create(ctx, sshkeys.CreateRequest{
			Name:       "nimbus-" + req.Hostname,
			PublicKey:  req.SSHPubKey,
			PrivateKey: req.SSHPrivKey,
			OwnerID:    req.OwnerID,
		})
		if err != nil {
			// Remap field names so errors reference the JSON keys the VMs API
			// exposes, not the keys-service internal field names.
			return nil, "", remapKeyFields(err)
		}
		return row, "", nil

	default:
		row, err := s.keys.GetDefault(ctx, req.OwnerID)
		if err != nil {
			var nf *internalerrors.NotFoundError
			if errors.As(err, &nf) {
				return nil, "", &internalerrors.ValidationError{
					Field:   "ssh",
					Message: "no SSH key supplied and no default key is set — pick a key, paste one, or generate one",
				}
			}
			return nil, "", err
		}
		return row, "", nil
	}
}

// pickNode collects live cluster telemetry, intersects it with the set of
// nodes that have a template for the requested OS (via the node_templates
// table), scores the survivors, and returns the winner along with the
// templateVMID to clone from on that node.
//
// Three Proxmox calls run in parallel: GetNodes (capacity + load),
// GetClusterVMs (per-node VM count and committed RAM), and GetClusterStorage
// (free disk on the configured VM-disk pool). On rejection the returned
// ConflictError lists each node's reason for diagnostics.
func (s *Service) pickNode(
	ctx context.Context,
	tier nodescore.Tier,
	osKey string,
	requiredTags []string,
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

	var (
		nodes        []proxmox.Node
		clusterVMs   []proxmox.ClusterVM
		clusterStore []proxmox.ClusterStorage
		nodesErr     error
		vmsErr       error
		storeErr     error
		wg           sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); nodes, nodesErr = s.px.GetNodes(ctx) }()
	go func() { defer wg.Done(); clusterVMs, vmsErr = s.px.GetClusterVMs(ctx) }()
	go func() { defer wg.Done(); clusterStore, storeErr = s.px.GetClusterStorage(ctx) }()
	wg.Wait()
	if nodesErr != nil {
		return "", 0, fmt.Errorf("get nodes: %w", nodesErr)
	}
	if vmsErr != nil {
		return "", 0, fmt.Errorf("get cluster vms: %w", vmsErr)
	}
	if storeErr != nil {
		return "", 0, fmt.Errorf("get cluster storage: %w", storeErr)
	}

	runtime := make(map[string]nodescore.NodeRuntime, len(nodes))
	for _, vm := range clusterVMs {
		if vm.Template == 1 {
			continue
		}
		rt := runtime[vm.Node]
		rt.VMCount++
		rt.CommittedMemBytes += vm.MaxMem
		rt.CommittedCPU += vm.MaxCPU
		runtime[vm.Node] = rt
	}

	// StorageByNode is nil when the operator hasn't configured a VM-disk
	// pool — disables the disk gate per the nodescore contract.
	var storageByNode map[string]nodescore.StorageInfo
	if s.cfg.VMDiskStorage != "" {
		storageByNode = make(map[string]nodescore.StorageInfo, len(nodes))
		// Shared storage repeats per node with identical capacity; first row
		// wins, then stamp the same StorageInfo onto every node so the disk
		// gate sees the same pool everywhere.
		var sharedInfo *nodescore.StorageInfo
		for _, st := range clusterStore {
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
			storageByNode[st.Node] = info
		}
		if sharedInfo != nil {
			for _, n := range nodes {
				storageByNode[n.Name] = *sharedInfo
			}
		}
	}

	// db.Node carries the operator-set state pickNode needs — lock
	// state for the cordoning gate, tags for the host-aggregate
	// filter. One SELECT per provision; row count = cluster size.
	metaByNode, err := s.nodeMetaByNode(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("load node meta: %w", err)
	}

	scoringNodes := make([]nodescore.Node, 0, len(nodes))
	for _, n := range nodes {
		meta := metaByNode[n.Name]
		scoringNodes = append(scoringNodes, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
			LockState: meta.LockState,
			Tags:      meta.Tags,
		})
	}

	// Read cluster-wide overcommit ratios. Empty/missing row defaults
	// to the nodescore fallback constants — never a fatal error.
	cpuRatio, ramRatio, diskRatio := readSchedulingRatios(ctx, s.db)
	env := nodescore.Env{
		Excluded:            s.cfg.ExcludedNodes,
		TemplatesPresent:    templatesPresent,
		StorageByNode:       storageByNode,
		MemBufferMiB:        s.cfg.MemBufferMiB,
		CPULoadFactor:       s.cfg.CPULoadFactor,
		RequiredTags:        requiredTags,
		CPUAllocationRatio:  cpuRatio,
		RAMAllocationRatio:  ramRatio,
		DiskAllocationRatio: diskRatio,
	}
	decisions := nodescore.Evaluate(scoringNodes, runtime, tier, env)
	winner, all := nodescore.Pick(decisions)
	tagsLog := strings.Join(requiredTags, ",")
	if tagsLog == "" {
		tagsLog = "<none>"
	}
	if winner == nil {
		log.Printf("pickNode: tier=%s os=%s tags=%s no_winner decisions: %s",
			tier.Name, osKey, tagsLog, formatDecisions(all))
		return "", 0, &internalerrors.ConflictError{
			Message: fmt.Sprintf("no eligible node for tier=%s os_template=%s tags=%s: %s",
				tier.Name, osKey, tagsLog, formatRejections(all)),
		}
	}
	log.Printf("pickNode: tier=%s os=%s tags=%s winner=%s spec=%s decisions: %s",
		tier.Name, osKey, tagsLog, winner.Node.Name, winner.Result.Spec, formatDecisions(all))
	return winner.Node.Name, templateVMIDByNode[winner.Node.Name], nil
}

// nodeMeta is the operator-set state pickNode reads off db.Node — lock
// state for the cordoning gate, tags for the host-aggregate filter.
// Tags is the union of operator-set CSV tags and Nimbus's auto-derived
// tags (CPU arch); see nodeMetaByNode for the merge.
type nodeMeta struct {
	LockState string
	Tags      []string
}

// nodeMetaByNode returns the per-node operator state (lock state +
// tags) for every db.Node row. Result is keyed by node name; missing
// entries default to a zero nodeMeta (lock "" → treated as "none";
// empty tags). Used by pickNode to filter cordoned nodes AND apply
// the operator's host-aggregate constraints.
//
// Tags includes Nimbus's auto-derived tags (currently CPU arch from
// the denormalized cpu_model column) so a `required_tags=arm` user
// constraint matches an ARM host even when no operator tags are set.
func (s *Service) nodeMetaByNode(ctx context.Context) (map[string]nodeMeta, error) {
	var rows []db.Node
	if err := s.db.WithContext(ctx).Select("name", "lock_state", "tags", "cpu_model", "has_ssd", "has_gpu").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]nodeMeta, len(rows))
	for _, r := range rows {
		auto := nodescore.DeriveAutoTags(nodescore.AutoTagInput{
			CPUModel: r.CPUModel,
			HasSSD:   r.HasSSD,
			HasGPU:   r.HasGPU,
		})
		tags := append(splitCSVTags(r.Tags), auto...)
		out[r.Name] = nodeMeta{LockState: r.LockState, Tags: tags}
	}
	return out, nil
}

// splitCSVTags decodes the CSV string we store for db.Node.Tags +
// db.VM.RequiredTags into a clean slice. Whitespace trimmed, empty
// entries dropped.
func splitCSVTags(csv string) []string {
	if csv == "" {
		return nil
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

// splitRequiredTags is splitCSVTags with a different name for the
// (admittedly cosmetic) clarity at provision-Request call sites —
// reads as "the operator-typed required tags from the request, parsed."
func splitRequiredTags(csv string) []string { return splitCSVTags(csv) }

// readSchedulingRatios fetches the cluster-wide overcommit ratios from
// the SchedulingSettings singleton. Falls back to the nodescore default
// constants on any error (missing table, busy SQLite, etc.) so a
// transient read failure doesn't strand provisioning. Logged at info
// level when fallback fires.
func readSchedulingRatios(ctx context.Context, gdb *gorm.DB) (cpu, ram, disk float64) {
	cpu, ram, disk = 4.0, 1.0, 1.0
	if gdb == nil {
		return
	}
	var row db.SchedulingSettings
	if err := gdb.WithContext(ctx).
		Where(&db.SchedulingSettings{ID: 1}).
		First(&row).Error; err != nil {
		// Row not yet seeded — use defaults.
		return
	}
	if row.CPUAllocationRatio >= 1.0 {
		cpu = row.CPUAllocationRatio
	}
	if row.RAMAllocationRatio >= 1.0 {
		ram = row.RAMAllocationRatio
	}
	if row.DiskAllocationRatio >= 1.0 {
		disk = row.DiskAllocationRatio
	}
	return
}

// formatRejections renders one "node=reason1,reason2" entry per rejected
// decision — used in the conflict-error message where every entry is a
// rejection so scores would all be 0 and add no signal.
func formatRejections(all []nodescore.Decision) string {
	parts := make([]string, 0, len(all))
	for _, d := range all {
		if len(d.Result.Reasons) == 0 {
			continue
		}
		reasons := make([]string, len(d.Result.Reasons))
		for i, r := range d.Result.Reasons {
			reasons[i] = string(r)
		}
		parts = append(parts, fmt.Sprintf("%s=%s", d.Node.Name, strings.Join(reasons, ",")))
	}
	if len(parts) == 0 {
		return "(no diagnostics)"
	}
	return strings.Join(parts, " ")
}

// formatDecisions renders one entry per node showing either the score (for
// accepted candidates) or "0(reason1,reason2)" (for rejected ones). Used in
// the per-provision log line so operators can see why each node won or lost.
func formatDecisions(all []nodescore.Decision) string {
	parts := make([]string, len(all))
	for i, d := range all {
		if len(d.Result.Reasons) == 0 {
			parts[i] = fmt.Sprintf("%s=%.3f", d.Node.Name, d.Result.Score)
			continue
		}
		reasons := make([]string, len(d.Result.Reasons))
		for j, r := range d.Result.Reasons {
			reasons[j] = string(r)
		}
		parts[i] = fmt.Sprintf("%s=0(%s)", d.Node.Name, strings.Join(reasons, ","))
	}
	return strings.Join(parts, " ")
}

// remapKeyFields rewrites a ValidationError's Field from the keys-service
// payload names ("public_key", "private_key") to the VM-API names
// ("ssh_pubkey", "ssh_privkey"), so error messages reference the JSON field
// the user actually sent. Non-validation errors are passed through unchanged.
func remapKeyFields(err error) error {
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	switch ve.Field {
	case "public_key":
		ve.Field = "ssh_pubkey"
	case "private_key":
		ve.Field = "ssh_privkey"
	}
	return ve
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
