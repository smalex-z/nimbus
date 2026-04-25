package provision_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
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
	getNodes       func(context.Context) ([]proxmox.Node, error)
	listVMs        func(context.Context, string) ([]proxmox.VMStatus, error)
	templateExists func(context.Context, string, int) (bool, error)
	nextVMID       func(context.Context) (int, error)
	cloneVM        func(context.Context, string, string, int, int, string) (string, error)
	waitForTask    func(context.Context, string, string, time.Duration) error
	setCloudInit   func(context.Context, string, int, proxmox.CloudInitConfig) error
	resizeDisk     func(context.Context, string, int, string, string) error
	startVM        func(context.Context, string, int) (string, error)
	getAgentIfaces func(context.Context, string, int) ([]proxmox.NetworkInterface, error)

	cloneCalls     atomic.Int32
	cloudInitCalls atomic.Int32
	cloudInitArgs  atomic.Pointer[proxmox.CloudInitConfig]
}

func (f *fakePVE) GetNodes(ctx context.Context) ([]proxmox.Node, error) {
	return f.getNodes(ctx)
}
func (f *fakePVE) ListVMs(ctx context.Context, n string) ([]proxmox.VMStatus, error) {
	return f.listVMs(ctx, n)
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
func (f *fakePVE) ResizeDisk(ctx context.Context, n string, vmid int, d, s string) error {
	return f.resizeDisk(ctx, n, vmid, d, s)
}
func (f *fakePVE) StartVM(ctx context.Context, n string, vmid int) (string, error) {
	return f.startVM(ctx, n, vmid)
}
func (f *fakePVE) GetAgentInterfaces(ctx context.Context, n string, vmid int) ([]proxmox.NetworkInterface, error) {
	return f.getAgentIfaces(ctx, n, vmid)
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
		listVMs:        func(_ context.Context, _ string) ([]proxmox.VMStatus, error) { return nil, nil },
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
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, ippool.Model(), &db.VM{}, &db.NodeTemplate{}, &db.SSHKey{})
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

	svc := provision.New(fake, pool, database.DB, cipher, keysSvc, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
	})
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
	})
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
	})
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
	})
	if err != nil {
		t.Fatalf("expected soft success, got error: %v", err)
	}
	if res.Warning == "" {
		t.Errorf("expected non-empty Warning on soft success")
	}
	if !strings.Contains(res.Warning, "could not confirm reachability") {
		t.Errorf("warning text doesn't explain reachability issue: %q", res.Warning)
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
	})
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
	})
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
	})
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
			{Name: "alpha", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30},
			{Name: "bravo", Status: "online", MaxMem: 16 << 30, Mem: 1 << 30},
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
	}); err != nil {
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
	})
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
	})
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
	})
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
	keyName, priv, err := svc.GetPrivateKey(context.Background(), vm.ID)
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
	}); err != nil {
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

	_, _, err := svc.GetPrivateKey(context.Background(), vm.ID)
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
	})
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
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	var row db.VM
	if err := database.First(&row, "hostname = ?", "byo-vault-vm").Error; err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.GetPrivateKey(context.Background(), row.ID)
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
	}); err != nil {
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
	}); err != nil {
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
	})
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
	})
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
	})
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
	})
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
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Should have advanced to 10.0.0.2 since the first verify "failed unsafe".
	if res.IP != "10.0.0.2" {
		t.Errorf("IP = %s, want 10.0.0.2 after transient verify error", res.IP)
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
