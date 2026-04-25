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
)

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

func newTestService(t *testing.T, fake *fakePVE) (*provision.Service, *ippool.Pool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, ippool.Model(), &db.VM{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), "10.0.0.1", "10.0.0.5"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	svc := provision.New(fake, pool, database.DB, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
	})
	return svc, pool
}

func TestProvision_HappyPath_BringYourOwnKey(t *testing.T) {
	t.Parallel()
	svc, pool := newTestService(t, happyFakePVE(t))

	res, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "test-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  "ssh-ed25519 AAAA test@laptop",
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
	svc, _ := newTestService(t, fake)

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
}

func TestProvision_FailureReleasesIP(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	// Inject a failure in SetCloudInit (after IP is reserved).
	fake.setCloudInit = func(_ context.Context, _ string, _ int, _ proxmox.CloudInitConfig) error {
		return errors.New("boom")
	}
	svc, pool := newTestService(t, fake)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "boom-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  "ssh-ed25519 AAAA",
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
	svc, pool := newTestService(t, fake)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "lonely-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  "ssh-ed25519 AAAA",
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
	// Only bravo has the template.
	fake.templateExists = func(_ context.Context, n string, _ int) (bool, error) {
		return n == "bravo", nil
	}
	captured := atomic.Pointer[string]{}
	fake.cloneVM = func(_ context.Context, _, target string, _, _ int, _ string) (string, error) {
		t := target
		captured.Store(&t)
		return "UPID", nil
	}

	svc, _ := newTestService(t, fake)
	if _, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "tpl-vm",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  "ssh-ed25519 AAAA",
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
	svc, _ := newTestService(t, happyFakePVE(t))
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "x",
		Tier:       "bogus",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  "ssh-ed25519 AAAA",
	})
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestProvision_UnknownOSTemplate(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t, happyFakePVE(t))
	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "x",
		Tier:       "small",
		OSTemplate: "windows-95",
		SSHPubKey:  "ssh-ed25519 AAAA",
	})
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
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
