package provision_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
	"nimbus/internal/tunnel"
)

// realPubKey returns a parseable ssh-ed25519 public key. The provision
// service now validates BYO public keys (it computes a fingerprint when
// persisting them through the keys service), so tests can no longer hand in
// arbitrary stub strings.
func realPubKey(t *testing.T) string {
	t.Helper()
	pub, _, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatalf("generate test pubkey: %v", err)
	}
	return pub
}

// fakePVE is a hand-rolled mock of provision.ProxmoxClient. Each method has
// a function field the test can override to inject behavior; defaults return
// success with empty data.
type fakePVE struct {
	getNodes          func(context.Context) ([]proxmox.Node, error)
	getClusterVMs     func(context.Context) ([]proxmox.ClusterVM, error)
	getClusterStorage func(context.Context) ([]proxmox.ClusterStorage, error)
	getVMConfig       func(context.Context, string, int) (map[string]any, error)
	templateExists    func(context.Context, string, int) (bool, error)
	nextVMID          func(context.Context) (int, error)
	cloneVM           func(context.Context, string, string, int, int, string) (string, error)
	waitForTask       func(context.Context, string, string, time.Duration) error
	setCloudInit      func(context.Context, string, int, proxmox.CloudInitConfig) error
	setVMTags         func(context.Context, string, int, []string) error
	setVMDescription  func(context.Context, string, int, string) error
	resizeDisk        func(context.Context, string, int, string, string) error
	startVM           func(context.Context, string, int) (string, error)
	stopVM            func(context.Context, string, int) (string, error)
	shutdownVM        func(context.Context, string, int) (string, error)
	rebootVM          func(context.Context, string, int) (string, error)
	destroyVM         func(context.Context, string, int) (string, error)
	getAgentIfaces    func(context.Context, string, int) ([]proxmox.NetworkInterface, error)
	migrateVM         func(context.Context, string, int, string, bool) (string, error)
	setVMNetwork      func(context.Context, string, int, string, string) error
	agentRun          func(context.Context, string, int, []string, string, time.Duration) (*proxmox.AgentExecStatus, error)
	uploadFile        func(context.Context, string, string, string, string, []byte) error
	deleteVolume      func(context.Context, string, string) error
	attachCDROM       func(context.Context, string, int, string, string) error
	detachDrive       func(context.Context, string, int, string) error

	cloneCalls     atomic.Int32
	cloudInitCalls atomic.Int32
	cloudInitArgs  atomic.Pointer[proxmox.CloudInitConfig]
}

func (f *fakePVE) GetNodes(ctx context.Context) ([]proxmox.Node, error) {
	return f.getNodes(ctx)
}
func (f *fakePVE) GetClusterVMs(ctx context.Context) ([]proxmox.ClusterVM, error) {
	return f.getClusterVMs(ctx)
}
func (f *fakePVE) GetClusterStorage(ctx context.Context) ([]proxmox.ClusterStorage, error) {
	return f.getClusterStorage(ctx)
}
func (f *fakePVE) GetVMConfig(ctx context.Context, n string, vmid int) (map[string]any, error) {
	if f.getVMConfig == nil {
		return map[string]any{}, nil
	}
	return f.getVMConfig(ctx, n, vmid)
}
func (f *fakePVE) TemplateExists(ctx context.Context, n string, vmid int) (bool, error) {
	return f.templateExists(ctx, n, vmid)
}
func (f *fakePVE) NextVMID(ctx context.Context) (int, error) {
	return f.nextVMID(ctx)
}
func (f *fakePVE) CloneVM(ctx context.Context, src, tgt string, tvmid, nvmid int, name string) (string, error) {
	f.cloneCalls.Add(1)
	return f.cloneVM(ctx, src, tgt, tvmid, nvmid, name)
}
func (f *fakePVE) WaitForTask(ctx context.Context, n, t string, i time.Duration) error {
	return f.waitForTask(ctx, n, t, i)
}
func (f *fakePVE) SetCloudInit(ctx context.Context, n string, vmid int, cfg proxmox.CloudInitConfig) error {
	f.cloudInitCalls.Add(1)
	c := cfg
	f.cloudInitArgs.Store(&c)
	return f.setCloudInit(ctx, n, vmid, cfg)
}
func (f *fakePVE) SetVMTags(ctx context.Context, n string, vmid int, tags []string) error {
	if f.setVMTags == nil {
		return nil
	}
	return f.setVMTags(ctx, n, vmid, tags)
}
func (f *fakePVE) SetVMDescription(ctx context.Context, n string, vmid int, desc string) error {
	if f.setVMDescription == nil {
		return nil
	}
	return f.setVMDescription(ctx, n, vmid, desc)
}
func (f *fakePVE) ResizeDisk(ctx context.Context, n string, vmid int, d, s string) error {
	return f.resizeDisk(ctx, n, vmid, d, s)
}
func (f *fakePVE) StartVM(ctx context.Context, n string, vmid int) (string, error) {
	return f.startVM(ctx, n, vmid)
}
func (f *fakePVE) StopVM(ctx context.Context, n string, vmid int) (string, error) {
	if f.stopVM == nil {
		return "task:stop", nil
	}
	return f.stopVM(ctx, n, vmid)
}
func (f *fakePVE) ShutdownVM(ctx context.Context, n string, vmid int) (string, error) {
	if f.shutdownVM == nil {
		return "task:shutdown", nil
	}
	return f.shutdownVM(ctx, n, vmid)
}
func (f *fakePVE) RebootVM(ctx context.Context, n string, vmid int) (string, error) {
	if f.rebootVM == nil {
		return "task:reboot", nil
	}
	return f.rebootVM(ctx, n, vmid)
}
func (f *fakePVE) DestroyVM(ctx context.Context, n string, vmid int) (string, error) {
	if f.destroyVM == nil {
		return "task:destroy", nil
	}
	return f.destroyVM(ctx, n, vmid)
}
func (f *fakePVE) GetAgentInterfaces(ctx context.Context, n string, vmid int) ([]proxmox.NetworkInterface, error) {
	return f.getAgentIfaces(ctx, n, vmid)
}
func (f *fakePVE) MigrateVM(ctx context.Context, src string, vmid int, target string, online bool) (string, error) {
	if f.migrateVM == nil {
		return "task:migrate", nil
	}
	return f.migrateVM(ctx, src, vmid, target, online)
}
func (f *fakePVE) SetVMNetwork(ctx context.Context, node string, vmid int, dev, bridge string) error {
	if f.setVMNetwork == nil {
		return nil
	}
	return f.setVMNetwork(ctx, node, vmid, dev, bridge)
}
func (f *fakePVE) AgentRun(ctx context.Context, node string, vmid int, command []string, inputData string, pollInterval time.Duration) (*proxmox.AgentExecStatus, error) {
	if f.agentRun == nil {
		return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0}, nil
	}
	return f.agentRun(ctx, node, vmid, command, inputData, pollInterval)
}
func (f *fakePVE) UploadFile(ctx context.Context, node, storage, contentType, filename string, content []byte) error {
	if f.uploadFile == nil {
		return nil
	}
	return f.uploadFile(ctx, node, storage, contentType, filename, content)
}
func (f *fakePVE) DeleteStorageVolume(ctx context.Context, node, volid string) error {
	if f.deleteVolume == nil {
		return nil
	}
	return f.deleteVolume(ctx, node, volid)
}
func (f *fakePVE) AttachCDROM(ctx context.Context, node string, vmid int, slot, volid string) error {
	if f.attachCDROM == nil {
		return nil
	}
	return f.attachCDROM(ctx, node, vmid, slot, volid)
}
func (f *fakePVE) DetachDrive(ctx context.Context, node string, vmid int, slot string) error {
	if f.detachDrive == nil {
		return nil
	}
	return f.detachDrive(ctx, node, vmid, slot)
}

// happyFakePVE returns a fakePVE wired so that Provision() succeeds with
// minimal effort. Tests can override individual methods to inject failure.
func happyFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	return &fakePVE{
		getNodes: func(ctx context.Context) ([]proxmox.Node, error) {
			return []proxmox.Node{
				{Name: "alpha", Status: "online", Mem: 1 << 30, MaxMem: 16 << 30, CPU: 0.1, MaxCPU: 8},
			}, nil
		},
		getClusterVMs: func(_ context.Context) ([]proxmox.ClusterVM, error) { return nil, nil },
		getClusterStorage: func(_ context.Context) ([]proxmox.ClusterStorage, error) {
			return nil, nil
		},
		templateExists: func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
		nextVMID:       func(_ context.Context) (int, error) { return 200, nil },
		cloneVM: func(_ context.Context, _, _ string, _, _ int, _ string) (string, error) {
			return "UPID:alpha:00001234::qmclone:200:root@pam:", nil
		},
		waitForTask:  func(_ context.Context, _, _ string, _ time.Duration) error { return nil },
		setCloudInit: func(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error { return nil },
		resizeDisk:   func(_ context.Context, _ string, _ int, _, _ string) error { return nil },
		startVM:      func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		getAgentIfaces: func(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
			return []proxmox.NetworkInterface{
				{Name: "ens18", IPAddresses: []proxmox.IPAddress{{IPAddress: "10.0.0.1", IPAddressType: "ipv4"}}},
			}, nil
		},
	}
}

func newTestService(t *testing.T, fake *fakePVE) (*provision.Service, *ippool.Pool, *db.DB) {
	return newTestServiceOpts(t, fake, nil)
}

// newTestServiceOpts is the configurable variant of newTestService — it lets
// a single test override Config fields (e.g. NetworkOpPerVMTimeout) before
// provision.New applies its defaults.
func newTestServiceOpts(t *testing.T, fake *fakePVE, mutate func(*provision.Config)) (*provision.Service, *ippool.Pool, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, ippool.Model(), &db.VM{}, &db.NodeTemplate{}, &db.SSHKey{}, &db.Node{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), "10.0.0.1", "10.0.0.5"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Seed node_templates rows so the new pickNode DB lookup finds eligible
	// nodes. happyFakePVE returns one node "alpha", so all 4 OS templates
	// live there at unique VMIDs.
	vmid := 9000
	for _, os := range []string{"ubuntu-24.04", "ubuntu-22.04", "debian-12", "debian-11"} {
		if err := database.Create(&db.NodeTemplate{
			Node: "alpha", OS: os, VMID: vmid,
		}).Error; err != nil {
			t.Fatalf("seed node_templates: %v", err)
		}
		vmid++
	}

	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	keysSvc := sshkeys.New(database.DB, cipher)

	cfg := provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc := provision.New(fake, pool, database.DB, cipher, keysSvc, cfg)
	return svc, pool, database
}

func TestProvision_HappyPath_BringYourOwnKey(t *testing.T) {
	t.Parallel()
	svc, pool, _ := newTestService(t, happyFakePVE(t))

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "test-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.IP != "10.0.0.1" {
		t.Errorf("IP = %s, want 10.0.0.1", res.IP)
	}
	if res.VMID != 200 {
		t.Errorf("VMID = %d, want 200", res.VMID)
	}
	if res.Username != "ubuntu" {
		t.Errorf("Username = %s, want ubuntu", res.Username)
	}
	if res.SSHPrivateKey != "" {
		t.Errorf("expected no private key for BYO mode, got %d chars", len(res.SSHPrivateKey))
	}

	// IP should be allocated, not still reserved.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.IP == "10.0.0.1" && r.Status != ippool.StatusAllocated {
			t.Errorf("IP status = %s, want allocated", r.Status)
		}
	}
}

func TestProvision_GenerateKey_ReturnsPrivateKey(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, _ := newTestService(t, fake)

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:    "gen-vm",
		Tier:        "medium",
		OSTemplate:  "debian-12",
		GenerateKey: true,
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.Contains(res.SSHPrivateKey, "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("expected OpenSSH PEM in private key, got: %q", res.SSHPrivateKey)
	}
	if res.Username != "debian" {
		t.Errorf("Username = %s, want debian", res.Username)
	}

	// Verify the public key passed to SetCloudInit looks like an Ed25519 pubkey.
	cfg := fake.cloudInitArgs.Load()
	if cfg == nil {
		t.Fatalf("SetCloudInit was never called")
		return // unreachable; placates staticcheck SA5011 false-positive
	}
	if !strings.HasPrefix(cfg.SSHKeys, "ssh-ed25519 ") {
		t.Errorf("SetCloudInit got non-ed25519 public key: %q", cfg.SSHKeys)
	}
	// Cloned VMs inherit the template's resources (1 core / 1024 MiB), so the
	// tier's CPU/memory must be applied via the same /config call.
	if cfg.Cores != 2 {
		t.Errorf("SetCloudInit Cores = %d, want 2 (medium)", cfg.Cores)
	}
	if cfg.CPU != "x86-64-v3" {
		t.Errorf("SetCloudInit CPU = %q, want x86-64-v3", cfg.CPU)
	}
	if cfg.Memory != 2048 {
		t.Errorf("SetCloudInit Memory = %d, want 2048 (medium)", cfg.Memory)
	}
}

func TestProvision_WaitForIPTimeout_SoftSuccess(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Simulate the agent never reporting the expected IP — eventually
	// WaitForIP exhausts its budget and returns context.DeadlineExceeded.
	fake.getAgentIfaces = func(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
		return []proxmox.NetworkInterface{}, nil // no IPs reported, ever
	}
	svc, pool, _ := newTestService(t, fake)

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "unreachable-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err != nil {
		t.Fatalf("expected soft success, got error: %v", err)
	}
	if res.Warning == "" {
		t.Errorf("expected non-empty Warning on soft success")
	}
	if !strings.Contains(res.Warning, "qemu-guest-agent") {
		t.Errorf("warning text doesn't explain agent timeout: %q", res.Warning)
	}
	if res.VMID != 200 {
		t.Errorf("VMID = %d, want 200", res.VMID)
	}
	if res.IP != "10.0.0.1" {
		t.Errorf("IP = %s, want 10.0.0.1", res.IP)
	}

	// IP must be marked allocated even though reachability wasn't confirmed —
	// the VM is real and holds the IP.
	rows, _ := pool.List(context.Background())
	allocated := false
	for _, r := range rows {
		if r.IP == "10.0.0.1" && r.Status == ippool.StatusAllocated {
			allocated = true
		}
	}
	if !allocated {
		t.Errorf("expected IP 10.0.0.1 to be allocated after soft success")
	}
}

func TestProvision_WaitForIPHardError_StillFails(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Replace the agent endpoint with one that returns a non-timeout error
	// the WaitForIP loop is happy to swallow → but we want a different path.
	// Easier: hijack StartVM to return a non-timeout error to ensure hard
	// failures still return 500. (WaitForIP only returns ctx.Err — there's
	// no other "hard" error path inside it. So we exercise the same logic
	// via a different step that DOES return non-timeout errors.)
	fake.startVM = func(_ context.Context, _ string, _ int) (string, error) {
		return "", errors.New("boom: hardware on fire")
	}
	svc, pool, _ := newTestService(t, fake)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "fail-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err == nil {
		t.Fatalf("expected error for non-timeout failure")
	}
	// IP must be released back to free.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.IP == "10.0.0.1" && r.Status != ippool.StatusFree {
			t.Errorf("IP %s status = %s, want free after hard failure", r.IP, r.Status)
		}
	}
}

func TestProvision_FailureReleasesIP(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Inject a failure in SetCloudInit (after IP is reserved).
	fake.setCloudInit = func(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
		return errors.New("boom")
	}
	svc, pool, _ := newTestService(t, fake)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "boom-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err == nil {
		t.Fatalf("expected error")
	}

	// Reserved IP must have been released back to free.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.IP == "10.0.0.1" && r.Status != ippool.StatusFree {
			t.Errorf("after failure IP %s status = %s, want free", r.IP, r.Status)
		}
	}
}

func TestProvision_NoEligibleNode(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// All nodes offline.
	fake.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "offline", MaxMem: 16 << 30},
		}, nil
	}
	svc, pool, _ := newTestService(t, fake)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "lonely-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}

	// IP must be released even though we never got past node selection.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.Status != ippool.StatusFree {
			t.Errorf("IP %s status=%s, want all free after no-node failure", r.IP, r.Status)
		}
	}
}

func TestProvision_TemplateMissing_FiltersNode(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30, MaxCPU: 8},
			{Name: "bravo", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30, MaxCPU: 8},
		}, nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		tgt := target
		captured.Store(&tgt)
		return "UPID", nil
	}

	svc, _, database := newTestService(t, fake)
	// Override the default seed: drop alpha's templates, give bravo the
	// one we need. With only bravo eligible, the scorer must pick it.
	if err := database.Where("node = ?", "alpha").Delete(&db.NodeTemplate{}).Error; err != nil {
		t.Fatalf("clear alpha templates: %v", err)
	}
	if err := database.Create(&db.NodeTemplate{
		Node: "bravo", OS: "ubuntu-24.04", VMID: 9100,
	}).Error; err != nil {
		t.Fatalf("seed bravo template: %v", err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "tpl-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := captured.Load()
	if got == nil || *got != "bravo" {
		t.Errorf("CloneVM target = %v, want bravo", got)
	}
}

func TestProvision_UnknownTier(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "x",
		Tier:       "bogus",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestProvision_UnknownOSTemplate(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "x",
		Tier:       "small",
		OSTemplate: "windows-95",
		SSHPubKey:  realPubKey(t),
	}, nil)
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestProvision_GenerateKey_StoresAndRetrievesFromVault(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:    "vault-vm",
		Tier:        "small",
		OSTemplate:  "ubuntu-24.04",
		GenerateKey: true,
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var vm db.VM
	if err := database.First(&vm, "vmid = ?", res.VMID).Error; err != nil {
		t.Fatalf("load vm: %v", err)
	}
	if vm.SSHKeyID == nil {
		t.Fatal("expected VM.SSHKeyID to point at the generated key")
	}

	// The encrypted blob now lives on the linked ssh_keys row.
	var key db.SSHKey
	if err := database.First(&key, *vm.SSHKeyID).Error; err != nil {
		t.Fatalf("load ssh_key: %v", err)
	}
	if !key.HasPrivateKey() {
		t.Fatal("expected encrypted private key on ssh_keys row")
	}
	if strings.Contains(string(key.PrivKeyCT), "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatal("private key was stored as plaintext")
	}
	if key.Source != sshkeys.SourceGenerated {
		t.Errorf("Source = %q, want generated", key.Source)
	}

	// GetPrivateKey decrypts and returns the same value the user got at
	// provision time.
	keyName, priv, err := svc.GetPrivateKey(context.Background(), vm.ID, nil)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if keyName != "nimbus-vault-vm" {
		t.Errorf("KeyName = %q, want nimbus-vault-vm", keyName)
	}
	if priv != res.SSHPrivateKey {
		t.Errorf("vault returned different private key than the API response")
	}
}

func TestProvision_BYO_PubKeyOnly_NoVaultEntry(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "byo-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var vm db.VM
	if err := database.First(&vm, "hostname = ?", "byo-vm").Error; err != nil {
		t.Fatal(err)
	}
	if vm.SSHKeyID == nil {
		t.Fatal("expected SSHKeyID to be set even for public-only BYO")
	}

	// The linked ssh_keys row should hold the public key but no encrypted
	// private half — the user opted not to vault it.
	var key db.SSHKey
	if err := database.First(&key, *vm.SSHKeyID).Error; err != nil {
		t.Fatalf("load ssh_key: %v", err)
	}
	if key.HasPrivateKey() {
		t.Error("BYO without privkey should not populate the vault columns")
	}

	_, _, err := svc.GetPrivateKey(context.Background(), vm.ID, nil)
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFound from GetPrivateKey, got %v", err)
	}
}

func TestProvision_BYO_MismatchedKeypair_Rejected(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))

	// Generate one keypair, then keep the private half from a *different*
	// keypair so the two don't match.
	pubA, _, err := provision.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	_, privB, err := provision.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Provision(context.Background(), provision.Request{
		Hostname:   "mismatch-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  pubA,
		SSHPrivKey: privB,
	}, nil)
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError on mismatched keypair, got %v", err)
	}
	if ve.Field != "ssh_privkey" {
		t.Errorf("ValidationError.Field = %q, want ssh_privkey", ve.Field)
	}
}

func TestProvision_BYO_MatchingKeypair_StoredInVault(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	pub, priv, err := provision.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "byo-vault-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  pub,
		SSHPrivKey: priv,
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var row db.VM
	if err := database.First(&row, "hostname = ?", "byo-vault-vm").Error; err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.GetPrivateKey(context.Background(), row.ID, nil)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if got != "nimbus-byo-vault-vm" {
		t.Errorf("KeyName = %q, want nimbus-byo-vault-vm", got)
	}
}

func TestProvision_UseSavedKey_LinksWithoutDuplicating(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	cipher, _ := secrets.New(make([]byte, secrets.KeyLen))
	keys := sshkeys.New(database.DB, cipher)
	key, err := keys.Create(context.Background(), sshkeys.CreateRequest{
		Name: "saved", Generate: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "saved-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHKeyID:   &key.ID,
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var vm db.VM
	if err := database.First(&vm, "hostname = ?", "saved-vm").Error; err != nil {
		t.Fatal(err)
	}
	if vm.SSHKeyID == nil || *vm.SSHKeyID != key.ID {
		t.Errorf("VM.SSHKeyID = %v, want %d", vm.SSHKeyID, key.ID)
	}
	if vm.KeyName != "saved" {
		t.Errorf("VM.KeyName = %q, want saved", vm.KeyName)
	}

	// We must NOT have created a duplicate ssh_keys row for this VM.
	var n int64
	if err := database.Model(&db.SSHKey{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("ssh_keys count = %d, want 1 (no duplicate row created)", n)
	}
}

func TestProvision_NoSSHInputUsesDefaultKey(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	cipher, _ := secrets.New(make([]byte, secrets.KeyLen))
	keys := sshkeys.New(database.DB, cipher)
	def, err := keys.Create(context.Background(), sshkeys.CreateRequest{
		Name: "default", Generate: true, SetDefault: true,
	})
	if err != nil {
		t.Fatalf("Create default: %v", err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "default-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var vm db.VM
	if err := database.First(&vm, "hostname = ?", "default-vm").Error; err != nil {
		t.Fatal(err)
	}
	if vm.SSHKeyID == nil || *vm.SSHKeyID != def.ID {
		t.Errorf("VM.SSHKeyID = %v, want %d (default)", vm.SSHKeyID, def.ID)
	}
}

func TestProvision_NoSSHInputAndNoDefault_ValidationError(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "x",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
	}, nil)
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

// fakeVerifier is a tiny stand-in for ippool.Reconciler. The verify field
// drives behavior per-call; calls counts how many times VerifyFree was invoked.
type fakeVerifier struct {
	verify func(ctx context.Context, ip string) (bool, *int, error)
	calls  atomic.Int32
}

func (f *fakeVerifier) VerifyFree(ctx context.Context, ip string) (bool, *int, error) {
	f.calls.Add(1)
	return f.verify(ctx, ip)
}

// TestProvision_VerifyHappyPath confirms a successful single-attempt verify
// does not allocate a different IP than Reserve picked.
func TestProvision_VerifyHappyPath(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	v := &fakeVerifier{verify: func(_ context.Context, _ string) (bool, *int, error) {
		return true, nil, nil
	}}
	svc.SetIPVerifier(v)

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "ok-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.IP != "10.0.0.1" {
		t.Errorf("IP = %s, want 10.0.0.1 (first free)", res.IP)
	}
	if got := v.calls.Load(); got != 1 {
		t.Errorf("verifier called %d times, want 1", got)
	}
}

// TestProvision_VerifyRaceLost_RetriesNextIP exercises the cross-instance race
// scenario: the verifier rejects the first IP (claims it's held by another
// VM), accepts the second. Provision should release the first, reserve the
// second, and complete successfully.
func TestProvision_VerifyRaceLost_RetriesNextIP(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// The leapfrog will land on 10.0.0.2; have the agent report that.
	fake.getAgentIfaces = func(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
		return []proxmox.NetworkInterface{
			{Name: "ens18", IPAddresses: []proxmox.IPAddress{{IPAddress: "10.0.0.2", IPAddressType: "ipv4"}}},
		}, nil
	}
	svc, pool, _ := newTestService(t, fake)

	// First call returns "held"; subsequent calls return "free".
	heldVMID := 999
	v := &fakeVerifier{}
	v.verify = func(_ context.Context, ip string) (bool, *int, error) {
		if ip == "10.0.0.1" {
			return false, &heldVMID, nil
		}
		return true, nil, nil
	}
	svc.SetIPVerifier(v)

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "race-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.IP != "10.0.0.2" {
		t.Errorf("IP = %s, want 10.0.0.2 (second free after race-loss)", res.IP)
	}
	if got := v.calls.Load(); got != 2 {
		t.Errorf("verifier called %d times, want 2 (one rejection, one success)", got)
	}

	// 10.0.0.1 must have been released back to free, since we lost the race.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.IP == "10.0.0.1" && r.Status != ippool.StatusFree {
			t.Errorf("10.0.0.1 status = %s after race-loss, want free", r.Status)
		}
		if r.IP == "10.0.0.2" && r.Status != ippool.StatusAllocated {
			t.Errorf("10.0.0.2 status = %s, want allocated", r.Status)
		}
	}
}

// TestProvision_VerifyExhaustsRetries asserts that after maxVerifyAttempts
// rejections, Provision returns a ConflictError and the last IP is released.
func TestProvision_VerifyExhaustsRetries(t *testing.T) {
	t.Parallel()
	svc, pool, _ := newTestService(t, happyFakePVE(t))

	heldVMID := 888
	v := &fakeVerifier{verify: func(_ context.Context, _ string) (bool, *int, error) {
		return false, &heldVMID, nil
	}}
	svc.SetIPVerifier(v)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "doomed-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}

	// All IPs we touched must be free again — three attempts means we
	// reserved three different IPs; all must be released.
	rows, _ := pool.List(context.Background())
	for _, r := range rows {
		if r.Status != ippool.StatusFree {
			t.Errorf("IP %s status = %s after exhausted retries, want all free", r.IP, r.Status)
		}
	}
}

// TestProvision_VerifyErrorTreatedAsUnsafe exercises the path where the
// verifier returns an error: the IP must be released and re-reserved (treated
// as if the verify had said "held").
func TestProvision_VerifyErrorTreatedAsUnsafe(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getAgentIfaces = func(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
		return []proxmox.NetworkInterface{
			{Name: "ens18", IPAddresses: []proxmox.IPAddress{{IPAddress: "10.0.0.2", IPAddressType: "ipv4"}}},
		}, nil
	}
	svc, _, _ := newTestService(t, fake)

	var firstCall atomic.Bool
	v := &fakeVerifier{verify: func(_ context.Context, _ string) (bool, *int, error) {
		if !firstCall.Swap(true) {
			return false, nil, errors.New("proxmox unreachable")
		}
		return true, nil, nil
	}}
	svc.SetIPVerifier(v)

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "transient-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Should have advanced to 10.0.0.2 since the first verify "failed unsafe".
	if res.IP != "10.0.0.2" {
		t.Errorf("IP = %s, want 10.0.0.2 after transient verify error", res.IP)
	}
}

// pickNodeFakePVE is a fake wired only for pickNode-style tests. The
// orchestrated Provision path is exercised elsewhere; these tests focus on
// the cluster-snapshot + scoring contract.
func pickNodeFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := happyFakePVE(t)
	// Two-node cluster, both online and capacious.
	f.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30, MaxCPU: 8, CPU: 0.1},
			{Name: "bravo", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30, MaxCPU: 8, CPU: 0.1},
		}, nil
	}
	return f
}

func newTestServiceWithCfg(t *testing.T, fake *fakePVE, cfg provision.Config) (*provision.Service, *ippool.Pool, *db.DB) {
	t.Helper()
	_, pool, database := newTestService(t, fake)
	// Add bravo's templates with unique VMIDs so both nodes can host the OS.
	bravoVMID := 9100
	for _, os := range []string{"ubuntu-24.04", "ubuntu-22.04", "debian-12", "debian-11"} {
		if err := database.Create(&db.NodeTemplate{Node: "bravo", OS: os, VMID: bravoVMID}).Error; err != nil {
			t.Fatalf("seed bravo template: %v", err)
		}
		bravoVMID++
	}
	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	keysSvc := sshkeys.New(database.DB, cipher)
	return provision.New(fake, pool, database.DB, cipher, keysSvc, cfg), pool, database
}

func TestPickNode_DiskGateRejectsFullPool(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	// alpha's local-lvm is full; bravo has plenty of room. Medium tier needs 30 GiB.
	fake.getClusterStorage = func(_ context.Context) ([]proxmox.ClusterStorage, error) {
		return []proxmox.ClusterStorage{
			{Storage: "local-lvm", Node: "alpha", Total: 100 << 30, Used: 99 << 30, Shared: 0},
			{Storage: "local-lvm", Node: "bravo", Total: 500 << 30, Used: 100 << 30, Shared: 0},
		}, nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		t := target
		captured.Store(&t)
		return "UPID", nil
	}

	svc, _, _ := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
		VMDiskStorage:    "local-lvm",
	})

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "disk-test-vm",
		Tier:       "medium",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := captured.Load()
	if got == nil || *got != "bravo" {
		t.Errorf("CloneVM target = %v, want bravo (alpha's disk too full)", got)
	}
}

// TestPickNode_CordonedNodeSkipped covers the spec acceptance criterion:
// "Cordon flips state and the scheduler skips the node within one provision
// attempt." Without the lockStatesByNode lookup in pickNode, this test would
// fail — the cordoned alpha would still get scored normally and (being the
// scoring tie-break winner) get picked.
func TestPickNode_CordonedNodeSkipped(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	fake.getClusterStorage = func(_ context.Context) ([]proxmox.ClusterStorage, error) {
		return []proxmox.ClusterStorage{
			{Storage: "local-lvm", Node: "alpha", Total: 500 << 30, Used: 100 << 30, Shared: 0},
			{Storage: "local-lvm", Node: "bravo", Total: 500 << 30, Used: 100 << 30, Shared: 0},
		}, nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		t := target
		captured.Store(&t)
		return "UPID", nil
	}

	svc, _, database := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
		VMDiskStorage:    "local-lvm",
	})

	// Seed both node rows; cordon alpha. The scheduler should pick bravo.
	for _, name := range []string{"alpha", "bravo"} {
		if err := database.WithContext(context.Background()).Create(&db.Node{
			Name: name, LockState: "none", LastSeenAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatalf("seed node %s: %v", name, err)
		}
	}
	if err := database.WithContext(context.Background()).Model(&db.Node{}).
		Where("name = ?", "alpha").
		Update("lock_state", "cordoned").Error; err != nil {
		t.Fatalf("cordon alpha: %v", err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "cordon-test-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := captured.Load()
	if got == nil || *got != "bravo" {
		t.Errorf("CloneVM target = %v, want bravo (alpha cordoned)", got)
	}
}

// TestPickNode_RequiredTagsFiltersHostAggregate is the headline test for
// the host-aggregate placement model: a VM that requires `fast-cpu`
// must only land on a node that carries that tag, even when an untagged
// node has more headroom. Mirrors OpenStack flavor extra-specs / K8s
// nodeSelector.
func TestPickNode_RequiredTagsFiltersHostAggregate(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	// alpha: capacious but no tags. bravo: smaller but tagged fast-cpu.
	fake.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxCPU: 16, MaxMem: 32 << 30, Mem: 1 << 30, CPU: 0.1},
			{Name: "bravo", Status: "online", MaxCPU: 8, MaxMem: 16 << 30, Mem: 1 << 30, CPU: 0.1},
		}, nil
	}
	fake.getClusterStorage = func(_ context.Context) ([]proxmox.ClusterStorage, error) {
		return []proxmox.ClusterStorage{
			{Storage: "local-lvm", Node: "alpha", Total: 500 << 30, Used: 100 << 30, Shared: 0},
			{Storage: "local-lvm", Node: "bravo", Total: 500 << 30, Used: 100 << 30, Shared: 0},
		}, nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		t := target
		captured.Store(&t)
		return "UPID", nil
	}

	svc, _, database := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
		VMDiskStorage:    "local-lvm",
	})

	// Seed both node rows; tag bravo as fast-cpu. The required-tag gate
	// should reject alpha (no fast-cpu tag) so bravo wins despite
	// having less capacity.
	for _, name := range []string{"alpha", "bravo"} {
		if err := database.WithContext(context.Background()).Create(&db.Node{
			Name: name, LockState: "none", LastSeenAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatalf("seed node %s: %v", name, err)
		}
	}
	if err := database.WithContext(context.Background()).Model(&db.Node{}).
		Where("name = ?", "bravo").
		Update("tags", "fast-cpu").Error; err != nil {
		t.Fatalf("tag bravo: %v", err)
	}

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:     "fast-vm",
		Tier:         "medium",
		RequiredTags: "fast-cpu",
		OSTemplate:   "ubuntu-24.04",
		SSHPubKey:    realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := captured.Load()
	if got == nil || *got != "bravo" {
		t.Errorf("CloneVM target = %v, want bravo (only fast-cpu-tagged node)", got)
	}
}

func TestPickNode_SharedStorageDedupes(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	// Shared Ceph pool: appears once per node with identical capacity. The
	// scorer must dedupe and stamp the same StorageInfo onto every node — and
	// the medium tier (30 GiB) fits comfortably in the 200 GiB free pool.
	fake.getClusterStorage = func(_ context.Context) ([]proxmox.ClusterStorage, error) {
		return []proxmox.ClusterStorage{
			{Storage: "ceph-pool", Node: "alpha", Total: 1 << 40, Used: 824 << 30, Shared: 1},
			{Storage: "ceph-pool", Node: "bravo", Total: 1 << 40, Used: 824 << 30, Shared: 1},
		}, nil
	}

	svc, _, _ := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
		VMDiskStorage:    "ceph-pool",
	})

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "shared-vm",
		Tier:       "medium",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
}

func TestPickNode_StoppedVMsBlockPlacement(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	// alpha looks idle live, but is hosting two stopped 7 GiB VMs (14 GiB
	// committed). bravo is empty. Asking for medium (2 GiB) on a 16 GiB node
	// would fit by live RAM but not by commitment — alpha should be rejected
	// and bravo chosen.
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{
			{VMID: 100, Node: "alpha", Status: "stopped", MaxMem: 7 << 30},
			{VMID: 101, Node: "alpha", Status: "stopped", MaxMem: 7 << 30},
		}, nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		tt := target
		captured.Store(&tt)
		return "UPID", nil
	}

	svc, _, _ := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
	})

	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "committed-vm",
		Tier:       "large",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got := captured.Load()
	if got == nil || *got != "bravo" {
		t.Errorf("CloneVM target = %v, want bravo (alpha overcommitted by stopped VMs)", got)
	}
}

func TestPickNode_RejectionMessageListsAllNodes(t *testing.T) {
	t.Parallel()
	fake := pickNodeFakePVE(t)
	// alpha excluded; bravo offline.
	fake.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30, MaxCPU: 8},
			{Name: "bravo", Status: "offline", MaxMem: 16 << 30, MaxCPU: 8},
		}, nil
	}

	svc, _, _ := newTestServiceWithCfg(t, fake, provision.Config{
		TemplateBaseVMID: 9000,
		ExcludedNodes:    []string{"alpha"},
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
	})

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "noplace-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil)
	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}
	if !strings.Contains(conflict.Error(), "alpha=excluded") {
		t.Errorf("expected 'alpha=excluded' in error, got: %s", conflict.Error())
	}
	if !strings.Contains(conflict.Error(), "bravo=offline") {
		t.Errorf("expected 'bravo=offline' in error, got: %s", conflict.Error())
	}
}

// ---- Gopher tunnel integration -------------------------------------------

func newGopherStub(t *testing.T, handler http.HandlerFunc) (*tunnel.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := tunnel.New(srv.URL, "test-key", 2*time.Second)
	if err != nil {
		t.Fatalf("tunnel.New: %v", err)
	}
	return c, &calls
}

func TestProvision_TunnelDisabled_IgnoresFlag(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	// Note: SetTunnelClient never called — service has nil tunnels.

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:     "no-tunnel-vm",
		Tier:         "small",
		OSTemplate:   "ubuntu-24.04",
		SSHPubKey:    realPubKey(t),
		PublicTunnel: true,
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.TunnelURL != "" || res.TunnelError != "" {
		t.Errorf("expected tunnel fields blank when client unset, got url=%q err=%q",
			res.TunnelURL, res.TunnelError)
	}
}

// 5xx from Gopher must NOT fail the VM provision. The VM comes back with
// tunnel_error populated.
func TestProvision_TunnelInfraError_VMStillSucceeds(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	tc, _ := newGopherStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	svc.SetTunnelClient(tc)

	pub, priv, err := provision.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:     "infra-fail",
		Tier:         "small",
		OSTemplate:   "ubuntu-24.04",
		SSHPubKey:    pub,
		SSHPrivKey:   priv,
		PublicTunnel: true,
	}, nil)
	if err != nil {
		t.Fatalf("Provision should still succeed despite Gopher 5xx, got %v", err)
	}
	if res.TunnelURL != "" {
		t.Errorf("TunnelURL = %q, want empty on infra error", res.TunnelURL)
	}
	if res.TunnelError == "" {
		t.Errorf("TunnelError should be populated on Gopher infra failure")
	}
	// VM row carries the same fields.
	var vm db.VM
	if err := database.First(&vm, "vmid = ?", res.VMID).Error; err != nil {
		t.Fatal(err)
	}
	if vm.TunnelError == "" {
		t.Errorf("VM.TunnelError should be persisted, got empty")
	}
}

// When WaitForIP soft-fails (cluster LAN unreachable), tunnel is registered
// but the bootstrap is skipped — surface manual-fix instructions in
// tunnel_error so the user knows how to finish.
func TestProvision_TunnelSoftSuccess_BootstrapSkipped(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getAgentIfaces = func(_ context.Context, _ string, _ int) ([]proxmox.NetworkInterface, error) {
		return []proxmox.NetworkInterface{}, nil // never reports the IP
	}
	svc, _, _ := newTestService(t, fake)

	var seenDelete atomic.Bool
	tc, _ := newGopherStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// Real Gopher shape: POST /machines returns the new machine in
			// the envelope. The bootstrap URL lives on the machine.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"m-soft","status":"pending","public_ssh":true,"bootstrap_url":"https://gopher.example.com/bootstrap/abc"}}`))
		case http.MethodDelete:
			seenDelete.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":null}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	svc.SetTunnelClient(tc)

	pub, priv, err := provision.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:     "soft",
		Tier:         "small",
		OSTemplate:   "ubuntu-24.04",
		SSHPubKey:    pub,
		SSHPrivKey:   priv,
		PublicTunnel: true,
	}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Warning == "" {
		t.Errorf("expected reachability warning")
	}
	if res.TunnelURL != "" {
		t.Errorf("TunnelURL should be empty when bootstrap is skipped, got %q", res.TunnelURL)
	}
	if !strings.Contains(res.TunnelError, "bootstrap_url") &&
		!strings.Contains(res.TunnelError, "manually") {
		t.Errorf("TunnelError should describe the manual recovery path: %q", res.TunnelError)
	}
	if seenDelete.Load() {
		t.Errorf("tunnel should NOT be deleted on soft-success — user can finish manually")
	}
}

func TestResultString_RedactsPrivateKey(t *testing.T) {
	t.Parallel()
	r := &provision.Result{
		VMID: 1, Hostname: "h", IP: "1.1.1.1", Tier: "small", OS: "ubuntu",
		SSHPrivateKey: "BEGIN... super-secret ...END",
	}
	s := r.String()
	if strings.Contains(s, "super-secret") {
		t.Errorf("private key leaked into String(): %s", s)
	}
	if !strings.Contains(s, "<REDACTED>") {
		t.Errorf("expected <REDACTED> marker, got %s", s)
	}
}

// recordReporter captures every ProgressEvent for order assertions.
func recordReporter() (provision.ProgressReporter, *[]string) {
	var seen []string
	rep := func(evt provision.ProgressEvent) {
		seen = append(seen, evt.Step)
	}
	return rep, &seen
}

func TestProvision_Progress_HappyPath_EmitsAllStepsInOrder(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))

	rep, seen := recordReporter()
	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "progress-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, rep); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	want := []string{
		provision.StepReserveIP,
		provision.StepCloneTpl,
		provision.StepConfigure,
		provision.StepStartVM,
		provision.StepWaitAgent,
	}
	if len(*seen) != len(want) {
		t.Fatalf("got %d events %v, want %d %v", len(*seen), *seen, len(want), want)
	}
	for i, step := range want {
		if (*seen)[i] != step {
			t.Errorf("event[%d] = %q, want %q", i, (*seen)[i], step)
		}
	}
}

func TestProvision_Progress_FailureStopsAtFailingPhase(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Hard-fail the clone *task* — clone() succeeds (returns a UPID) but
	// WaitForTask reports failure, so we expect the reserve_ip event to
	// have fired and *no* later event.
	fake.waitForTask = func(_ context.Context, _, _ string, _ time.Duration) error {
		return errors.New("clone failed: out of disk")
	}
	svc, _, _ := newTestService(t, fake)

	rep, seen := recordReporter()
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "broken-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, rep)
	if err == nil {
		t.Fatalf("expected clone failure, got nil")
	}

	want := []string{provision.StepReserveIP}
	if len(*seen) != len(want) || (*seen)[0] != want[0] {
		t.Fatalf("got events %v, want exactly %v", *seen, want)
	}
}

func TestProvision_Progress_NilReporterIsAllowed(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "noprog-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
	}, nil); err != nil {
		t.Fatalf("Provision with nil reporter: %v", err)
	}
}

func TestBackfillOwnership(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	// newTestService doesn't migrate Users — bring it up here. Create the
	// schema and seed the principals: an admin (id will be 1, lowest), a
	// non-admin, and a second admin (verifies "lowest ID" deterministic
	// selection).
	if err := database.AutoMigrate(&db.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	admin := db.User{Name: "Brendan", Email: "a@x", IsAdmin: true}
	if err := database.Create(&admin).Error; err != nil {
		t.Fatalf("create admin: %v", err)
	}
	member := db.User{Name: "Member", Email: "m@x", IsAdmin: false}
	if err := database.Create(&member).Error; err != nil {
		t.Fatalf("create member: %v", err)
	}
	admin2 := db.User{Name: "OtherAdmin", Email: "b@x", IsAdmin: true}
	if err := database.Create(&admin2).Error; err != nil {
		t.Fatalf("create admin2: %v", err)
	}

	// Two legacy VMs (owner_id NULL) and one already-owned VM. The owned
	// row pins to the member to verify backfill leaves it alone.
	legacy1 := db.VM{Hostname: "legacy1", VMID: 100, IP: "10.0.0.10", Node: "alpha"}
	legacy2 := db.VM{Hostname: "legacy2", VMID: 101, IP: "10.0.0.11", Node: "alpha"}
	memberID := member.ID
	owned := db.VM{Hostname: "owned", VMID: 102, IP: "10.0.0.12", Node: "alpha", OwnerID: &memberID}
	for _, vm := range []*db.VM{&legacy1, &legacy2, &owned} {
		if err := database.Create(vm).Error; err != nil {
			t.Fatalf("seed vm: %v", err)
		}
	}

	n, err := svc.BackfillOwnership(context.Background())
	if err != nil {
		t.Fatalf("BackfillOwnership: %v", err)
	}
	if n != 2 {
		t.Errorf("first run rows affected: got %d, want 2", n)
	}

	// Verify legacy rows now point to the lower-id admin (admin, not admin2).
	var refreshedLegacy1, refreshedLegacy2, refreshedOwned db.VM
	if err := database.First(&refreshedLegacy1, legacy1.ID).Error; err != nil {
		t.Fatalf("reload legacy1: %v", err)
	}
	if refreshedLegacy1.OwnerID == nil || *refreshedLegacy1.OwnerID != admin.ID {
		t.Errorf("legacy1 owner: got %v, want %d", refreshedLegacy1.OwnerID, admin.ID)
	}
	if err := database.First(&refreshedLegacy2, legacy2.ID).Error; err != nil {
		t.Fatalf("reload legacy2: %v", err)
	}
	if refreshedLegacy2.OwnerID == nil || *refreshedLegacy2.OwnerID != admin.ID {
		t.Errorf("legacy2 owner: got %v, want %d", refreshedLegacy2.OwnerID, admin.ID)
	}
	if err := database.First(&refreshedOwned, owned.ID).Error; err != nil {
		t.Fatalf("reload owned: %v", err)
	}
	if refreshedOwned.OwnerID == nil || *refreshedOwned.OwnerID != memberID {
		t.Errorf("owned row clobbered: got %v, want %d", refreshedOwned.OwnerID, memberID)
	}

	// Idempotency: re-running with no remaining NULLs should touch zero rows.
	n2, err := svc.BackfillOwnership(context.Background())
	if err != nil {
		t.Fatalf("BackfillOwnership second run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second run rows affected: got %d, want 0", n2)
	}
}

func TestBackfillOwnership_NoAdminYet(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))

	if err := database.AutoMigrate(&db.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	// No users at all — common during the setup wizard.
	legacy := db.VM{Hostname: "legacy", VMID: 200, IP: "10.0.0.20", Node: "alpha"}
	if err := database.Create(&legacy).Error; err != nil {
		t.Fatalf("seed legacy vm: %v", err)
	}

	n, err := svc.BackfillOwnership(context.Background())
	if err != nil {
		t.Fatalf("BackfillOwnership: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected without admin: got %d, want 0", n)
	}

	// Legacy row stays NULL so the next startup (post-setup) can finish the job.
	var refreshed db.VM
	if err := database.First(&refreshed, legacy.ID).Error; err != nil {
		t.Fatalf("reload legacy: %v", err)
	}
	if refreshed.OwnerID != nil {
		t.Errorf("legacy row mutated despite missing admin: owner_id=%v", refreshed.OwnerID)
	}
}

// TestAdminDelete verifies that AdminDelete bypasses the owner check Delete
// enforces — an admin call succeeds against a VM owned by a different user.
// Also confirms the destroy / IP-release / row-delete sequence runs.
func TestAdminDelete(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	var destroyCalls atomic.Int32
	fake.destroyVM = func(_ context.Context, _ string, _ int) (string, error) {
		destroyCalls.Add(1)
		return "task:destroy", nil
	}
	svc, pool, database := newTestService(t, fake)

	// Reserve the IP that the seeded VM "holds" so the admin delete has
	// something to release.
	ip, err := pool.Reserve(context.Background(), "victim")
	if err != nil {
		t.Fatalf("reserve ip: %v", err)
	}

	// Seed a VM owned by user A (id=42 — newTestService doesn't migrate Users
	// and no User row is required because AdminDelete doesn't load owners).
	ownerA := uint(42)
	vm := db.VM{Hostname: "victim", VMID: 100, IP: ip, Node: "alpha", Status: "stopped", OwnerID: &ownerA}
	if err := database.Create(&vm).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	if err := svc.AdminDelete(context.Background(), vm.ID); err != nil {
		t.Fatalf("AdminDelete: %v", err)
	}
	if destroyCalls.Load() != 1 {
		t.Errorf("destroy calls: got %d, want 1", destroyCalls.Load())
	}

	var ghost db.VM
	if err := database.Unscoped().First(&ghost, vm.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("vm row should be hard-deleted, got err=%v", err)
	}

	// IP should be free again — Reserve on the same hostname returns the same IP.
	regrabbed, err := pool.Reserve(context.Background(), "victim2")
	if err != nil {
		t.Fatalf("reserve after delete: %v", err)
	}
	if regrabbed != ip {
		t.Errorf("ip not released: regrabbed %s, want %s", regrabbed, ip)
	}
}

// TestAdminDelete_NotFound confirms AdminDelete returns NotFound for a row
// that doesn't exist, matching the user-scoped Delete behavior.
func TestAdminDelete_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))

	err := svc.AdminDelete(context.Background(), 9999)
	if err == nil {
		t.Fatalf("AdminDelete on missing row: want error, got nil")
	}
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("AdminDelete error: got %T (%v), want *NotFoundError", err, err)
	}
}
