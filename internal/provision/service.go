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
	DestroyVM(ctx context.Context, node string, vmid int) (string, error)
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

	// guards concurrent provisions from racing on cluster/nextid by
	// serializing the clone path. SQLite already serializes ippool.Reserve.
	cloneMu sync.Mutex
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
	return &Service{
		px:       px,
		pool:     pool,
		verifier: noopVerifier{},
		db:       database,
		cipher:   cipher,
		keys:     keys,
		cfg:      cfg,
	}
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
// On any failure after the IP is reserved, we release the IP back to the pool
// before returning. The Proxmox-side artifact (a half-cloned VM) is *not*
// cleaned up automatically in Phase 1 — that's a follow-up. We do persist a
// VM row with status=failed for visibility.
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

	// Resolve SSH key. The service may either reuse an existing vault entry
	// or create a new one (generate / BYO / default-fallback).
	sshKey, sshPrivateKey, err := s.resolveSSHKey(ctx, req)
	if err != nil {
		return nil, err
	}
	keyID := sshKey.ID
	keyName := sshKey.Name
	sshPubKey := sshKey.PublicKey

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

	// Step 1b: verify the picked IP is not already held by a VM elsewhere on
	// the cluster (catches the cross-instance race where two Nimbus instances
	// each picked the same lowest-free IP from their independent SQLite caches).
	// On race-loss, releases the local reservation and tries the next free IP,
	// up to maxVerifyAttempts.
	ip, err = s.verifyAndRetryReserve(ctx, ip, req.Hostname)
	if err != nil {
		return nil, err
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
	target, templateVMID, err := s.pickNode(ctx, tier, req.OSTemplate)
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

	// Step 4: cloud-init.
	username := proxmox.TemplateUsername(req.OSTemplate)
	cloudInit := proxmox.CloudInitConfig{
		CIUser:       username,
		SSHKeys:      sshPubKey,
		IPConfig0:    fmt.Sprintf("ip=%s/24,gw=%s", ip, s.cfg.GatewayIP),
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
	if err := s.px.SetVMDescription(ctx, target, newVMID, proxmox.EncodeNimbusDescription(req.Tier, req.OSTemplate)); err != nil {
		log.Printf("set description vmid=%d: %v (continuing)", newVMID, err)
	}

	// Step 5: resize the disk to tier spec. The cloud image ships at a small
	// size; we *grow* it by the difference. (Proxmox accepts +<n>G deltas.)
	resizeDelta := tier.DiskGB - 10 // cloud images are typically 10G base
	if resizeDelta > 0 {
		if err := s.px.ResizeDisk(ctx, target, newVMID, "scsi0", fmt.Sprintf("+%dG", resizeDelta)); err != nil {
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
	report(StepWaitAgent, "Guest agent ready")

	// Step 7b: Gopher tunnel bootstrap. If we successfully registered a tunnel
	// AND WaitForIP confirmed reachability (no warning), SSH in and run the
	// one-line bootstrap, then poll Gopher for active. Skipped on soft-success
	// because Nimbus can't reach the VM to bootstrap from there. All failures
	// are recorded as tunnel_error — the VM provision never fails for tunnel
	// reasons (design §10).
	tunnelURL := ""
	if machineObj != nil {
		switch {
		case warning != "":
			tunnelError = "VM unreachable from Nimbus, can't bootstrap tunnel — " +
				"machine registered but inactive. Run the bootstrap manually:" +
				"\n  curl " + machineObj.BootstrapURL + " | sh"
			// Keep the registered machine — user can finish bootstrap manually.
		default:
			privKey, perr := s.privateKeyForBootstrap(ctx, sshKey, sshPrivateKey)
			if perr != nil {
				log.Printf("tunnel: cannot bootstrap (no private key available): %v", perr)
				tunnelError = "tunnel bootstrap skipped: " + perr.Error()
			} else if berr := runTunnelBootstrap(ctx, ip, username, privKey, machineObj.BootstrapURL, req.Hostname); berr != nil {
				log.Printf("tunnel: bootstrap ssh failed: %v", berr)
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

	// Step 7c: GPU env bootstrap. Mirrors the tunnel bootstrap pattern but
	// always-on (default) — when the GPU plane is configured cluster-wide,
	// every VM gets `OPENAI_BASE_URL` and a `gx10` CLI helper unless the
	// caller asked to skip. Failures are logged, never block provisioning.
	gpuCfg := s.gpuBootstrapConfig()
	if !req.SkipGPUBootstrap && gpuCfg.BaseURL != "" && warning == "" {
		privKey, perr := s.privateKeyForBootstrap(ctx, sshKey, sshPrivateKey)
		if perr != nil {
			log.Printf("gpu bootstrap: cannot bootstrap (no private key available): %v", perr)
		} else if berr := runGPUBootstrap(ctx, ip, username, privKey, gpuCfg); berr != nil {
			log.Printf("gpu bootstrap vmid=%d: %v (continuing)", newVMID, berr)
		}
	}

	// Step 8: commit. IP transitions reserved -> allocated; VM row written.
	if err := s.pool.MarkAllocated(ctx, ip, newVMID); err != nil {
		return nil, fmt.Errorf("mark allocated: %w", err)
	}
	released = true // success path — do NOT run the deferred release

	// The encrypted private key (if any) lives on the ssh_keys row referenced
	// by SSHKeyID — not on the VM itself anymore.
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
		SSHKeyID:   &keyID,
		KeyName:    keyName,
		SSHPubKey:  sshPubKey,
		ErrorMsg:   warning, // doubles as a soft-warning record on the persisted row
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

	return &Result{
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
	}, nil
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
		if err := s.pool.Release(ctx, vm.IP); err != nil {
			log.Printf("delete vm %d: release ip %s: %v", vm.VMID, vm.IP, err)
		}
	}

	if err := s.db.WithContext(ctx).Unscoped().Delete(vm).Error; err != nil {
		return fmt.Errorf("delete vm row %d: %w", vm.ID, err)
	}
	return nil
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

	scoringNodes := make([]nodescore.Node, 0, len(nodes))
	for _, n := range nodes {
		scoringNodes = append(scoringNodes, nodescore.Node{
			Name: n.Name, Status: n.Status, CPU: n.CPU,
			MaxCPU: n.MaxCPU, Mem: n.Mem, MaxMem: n.MaxMem,
		})
	}

	env := nodescore.Env{
		Excluded:         s.cfg.ExcludedNodes,
		TemplatesPresent: templatesPresent,
		StorageByNode:    storageByNode,
		MemBufferMiB:     s.cfg.MemBufferMiB,
		CPULoadFactor:    s.cfg.CPULoadFactor,
	}
	decisions := nodescore.Evaluate(scoringNodes, runtime, tier, env)
	winner, all := nodescore.Pick(decisions)
	if winner == nil {
		log.Printf("pickNode: tier=%s os=%s no_winner decisions: %s",
			tier.Name, osKey, formatDecisions(all))
		return "", 0, &internalerrors.ConflictError{
			Message: fmt.Sprintf("no eligible node for tier=%s os_template=%s: %s",
				tier.Name, osKey, formatRejections(all)),
		}
	}
	log.Printf("pickNode: tier=%s os=%s winner=%s decisions: %s",
		tier.Name, osKey, winner.Node.Name, formatDecisions(all))
	return winner.Node.Name, templateVMIDByNode[winner.Node.Name], nil
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
