package bootstrap_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/bootstrap"
	"nimbus/internal/db"
	"nimbus/internal/proxmox"
)

// fakePX is a hand-rolled mock of the bootstrap.ProxmoxClient interface. Each
// method is overridable via a function field; defaults are happy-path.
type fakePX struct {
	getNodes             func(context.Context) ([]proxmox.Node, error)
	templateExists       func(context.Context, string, int) (bool, error)
	listVMs              func(context.Context, string) ([]proxmox.VMStatus, error)
	nextVMIDFrom         func(context.Context, int) (int, error)
	ensureStorageContent func(context.Context, string, string) error
	storageHasFile       func(context.Context, string, string, string, string) (bool, error)
	downloadStorageURL   func(context.Context, string, string, string, string, string) (string, error)
	waitForTask          func(context.Context, string, string, time.Duration) error
	createVMWithImport   func(context.Context, string, int, proxmox.CreateVMOpts) (string, error)
	convertToTemplate    func(context.Context, string, int) error

	// Bake ceremony surface — added with the D-boot rewrite.
	uploadFile          func(context.Context, string, string, string, string, []byte) error
	attachCDROM         func(context.Context, string, int, string, string) error
	detachDrive         func(context.Context, string, int, string) error
	deleteStorageVolume func(context.Context, string, string) error
	startVM             func(context.Context, string, int) (string, error)
	stopVM              func(context.Context, string, int) (string, error)
	destroyVM           func(context.Context, string, int) (string, error)
	getVMState          func(context.Context, string, int) (string, error)
	setVMTags           func(context.Context, string, int, []string) error
	agentRun            func(context.Context, string, int, []string, string, time.Duration) (*proxmox.AgentExecStatus, error)

	nextVMIDSeq   atomic.Int32 // for default sequential VMID assignment
	createCalls   atomic.Int32
	downloadCalls atomic.Int32
	convertCalls  atomic.Int32
	bakeCalls     atomic.Int32 // counts AgentRun invocations during bake
	tagCalls      atomic.Int32
}

func (f *fakePX) GetNodes(ctx context.Context) ([]proxmox.Node, error) {
	return f.getNodes(ctx)
}
func (f *fakePX) TemplateExists(ctx context.Context, n string, vmid int) (bool, error) {
	return f.templateExists(ctx, n, vmid)
}
func (f *fakePX) ListVMs(ctx context.Context, n string) ([]proxmox.VMStatus, error) {
	return f.listVMs(ctx, n)
}
func (f *fakePX) NextVMIDFrom(ctx context.Context, minVMID int) (int, error) {
	return f.nextVMIDFrom(ctx, minVMID)
}
func (f *fakePX) EnsureStorageContent(ctx context.Context, s, ct string) error {
	return f.ensureStorageContent(ctx, s, ct)
}
func (f *fakePX) StorageHasFile(ctx context.Context, n, s, ct, fn string) (bool, error) {
	return f.storageHasFile(ctx, n, s, ct, fn)
}
func (f *fakePX) DownloadStorageURL(ctx context.Context, n, s, ct, u, fn string) (string, error) {
	f.downloadCalls.Add(1)
	return f.downloadStorageURL(ctx, n, s, ct, u, fn)
}
func (f *fakePX) WaitForTask(ctx context.Context, n, t string, i time.Duration) error {
	return f.waitForTask(ctx, n, t, i)
}
func (f *fakePX) CreateVMWithImport(ctx context.Context, n string, vmid int, opts proxmox.CreateVMOpts) (string, error) {
	f.createCalls.Add(1)
	return f.createVMWithImport(ctx, n, vmid, opts)
}
func (f *fakePX) ConvertToTemplate(ctx context.Context, n string, vmid int) error {
	f.convertCalls.Add(1)
	return f.convertToTemplate(ctx, n, vmid)
}
func (f *fakePX) UploadFile(ctx context.Context, n, s, ct, fn string, c []byte) error {
	return f.uploadFile(ctx, n, s, ct, fn, c)
}
func (f *fakePX) AttachCDROM(ctx context.Context, n string, vmid int, slot, volid string) error {
	return f.attachCDROM(ctx, n, vmid, slot, volid)
}
func (f *fakePX) DetachDrive(ctx context.Context, n string, vmid int, slot string) error {
	return f.detachDrive(ctx, n, vmid, slot)
}
func (f *fakePX) DeleteStorageVolume(ctx context.Context, n, volid string) error {
	return f.deleteStorageVolume(ctx, n, volid)
}
func (f *fakePX) StartVM(ctx context.Context, n string, vmid int) (string, error) {
	return f.startVM(ctx, n, vmid)
}
func (f *fakePX) StopVM(ctx context.Context, n string, vmid int) (string, error) {
	return f.stopVM(ctx, n, vmid)
}
func (f *fakePX) DestroyVM(ctx context.Context, n string, vmid int) (string, error) {
	return f.destroyVM(ctx, n, vmid)
}
func (f *fakePX) GetVMState(ctx context.Context, n string, vmid int) (string, error) {
	return f.getVMState(ctx, n, vmid)
}
func (f *fakePX) SetVMTags(ctx context.Context, n string, vmid int, tags []string) error {
	f.tagCalls.Add(1)
	return f.setVMTags(ctx, n, vmid, tags)
}
func (f *fakePX) AgentRun(ctx context.Context, n string, vmid int, cmd []string, in string, p time.Duration) (*proxmox.AgentExecStatus, error) {
	f.bakeCalls.Add(1)
	return f.agentRun(ctx, n, vmid, cmd, in, p)
}

// happyPX returns a fully-mocked client where everything succeeds.
// NextVMIDFrom returns sequential IDs starting at the requested floor so each
// test gets distinct values inside the configured range.
func happyPX() *fakePX {
	f := &fakePX{
		getNodes: func(_ context.Context) ([]proxmox.Node, error) {
			return []proxmox.Node{
				{Name: "alpha", Status: "online"},
				{Name: "bravo", Status: "online"},
			}, nil
		},
		templateExists:       func(_ context.Context, _ string, _ int) (bool, error) { return false, nil },
		listVMs:              func(_ context.Context, _ string) ([]proxmox.VMStatus, error) { return nil, nil },
		ensureStorageContent: func(_ context.Context, _, _ string) error { return nil },
		storageHasFile:       func(_ context.Context, _, _, _, _ string) (bool, error) { return false, nil },
		downloadStorageURL: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return "UPID:download", nil
		},
		waitForTask: func(_ context.Context, _, _ string, _ time.Duration) error { return nil },
		createVMWithImport: func(_ context.Context, _ string, _ int, _ proxmox.CreateVMOpts) (string, error) {
			return "UPID:create", nil
		},
		convertToTemplate: func(_ context.Context, _ string, _ int) error { return nil },

		// Bake ceremony defaults — every step succeeds, GetVMState returns
		// "stopped" so waitForStopped exits on first poll, AgentRun
		// returns exit 0 so every cloud-init / poweroff probe is happy.
		uploadFile:          func(_ context.Context, _, _, _, _ string, _ []byte) error { return nil },
		attachCDROM:         func(_ context.Context, _ string, _ int, _, _ string) error { return nil },
		detachDrive:         func(_ context.Context, _ string, _ int, _ string) error { return nil },
		deleteStorageVolume: func(_ context.Context, _, _ string) error { return nil },
		startVM:             func(_ context.Context, _ string, _ int) (string, error) { return "UPID:start", nil },
		stopVM:              func(_ context.Context, _ string, _ int) (string, error) { return "UPID:stop", nil },
		destroyVM:           func(_ context.Context, _ string, _ int) (string, error) { return "UPID:destroy", nil },
		getVMState:          func(_ context.Context, _ string, _ int) (string, error) { return "stopped", nil },
		setVMTags:           func(_ context.Context, _ string, _ int, _ []string) error { return nil },
		agentRun: func(_ context.Context, _ string, _ int, _ []string, _ string, _ time.Duration) (*proxmox.AgentExecStatus, error) {
			return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0}, nil
		},
	}
	f.nextVMIDFrom = func(_ context.Context, min int) (int, error) {
		return min + int(f.nextVMIDSeq.Add(1)) - 1, nil
	}
	return f
}

// newSvc constructs a bootstrap.Service backed by a fresh on-disk SQLite DB
// scoped to the test's temp dir. Returns the underlying *db.DB so tests can
// pre-seed node_templates rows or assert post-conditions.
func newSvc(t *testing.T, px bootstrap.ProxmoxClient) (*bootstrap.Service, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.NodeTemplate{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return bootstrap.New(px, database.DB, bootstrap.Config{}), database
}

func TestBootstrap_HappyPath_AllOSesAllNodes(t *testing.T) {
	t.Parallel()
	px := happyPX()
	svc, database := newSvc(t, px)

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// 4 OSes × 2 nodes = 8 created
	if got := len(res.Created); got != 8 {
		t.Errorf("created = %d, want 8 (4 OSes × 2 nodes)", got)
	}
	if got := len(res.Failed); got != 0 {
		t.Errorf("failed = %d, want 0", got)
	}
	if got := len(res.Skipped); got != 0 {
		t.Errorf("skipped = %d, want 0", got)
	}
	if got := px.createCalls.Load(); got != 8 {
		t.Errorf("CreateVMWithImport called %d times, want 8", got)
	}
	if got := px.convertCalls.Load(); got != 8 {
		t.Errorf("ConvertToTemplate called %d times, want 8", got)
	}

	// VMIDs come from the mocked NextVMIDFrom (sequential from the configured
	// floor). Every VMID must be inside the template range AND cluster-wide
	// unique (matches Proxmox's actual constraint).
	seen := map[int]bool{}
	for _, o := range res.Created {
		if o.VMID < bootstrap.DefaultTemplateBase {
			t.Errorf("VMID %d below template floor %d (outcome %+v)",
				o.VMID, bootstrap.DefaultTemplateBase, o)
		}
		if seen[o.VMID] {
			t.Errorf("duplicate VMID across outcomes: %d", o.VMID)
		}
		seen[o.VMID] = true
	}
	if len(seen) != 8 {
		t.Errorf("got %d unique VMIDs, want 8", len(seen))
	}

	// Each (node, OS) → VMID mapping should also be persisted in the DB.
	var rows []db.NodeTemplate
	if err := database.Find(&rows).Error; err != nil {
		t.Fatalf("DB find: %v", err)
	}
	if len(rows) != 8 {
		t.Errorf("node_templates rows = %d, want 8", len(rows))
	}
	_ = sort.Strings // ensure sort import stays used by other tests
}

func TestBootstrap_Idempotent_SkipsExisting(t *testing.T) {
	t.Parallel()
	px := happyPX()
	// Templates already exist on Proxmox side AND in our DB.
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) { return true, nil }
	svc, database := newSvc(t, px)

	// Pre-seed the DB with rows for all (node, OS) pairs the test will iterate
	// — without these the DB-first idempotency check finds nothing and falls
	// through to the create flow.
	vmid := 9000
	for _, node := range []string{"alpha", "bravo"} {
		for _, entry := range bootstrap.Catalog {
			if err := database.Create(&db.NodeTemplate{
				Node: node, OS: entry.OS, VMID: vmid,
			}).Error; err != nil {
				t.Fatalf("seed: %v", err)
			}
			vmid++
		}
	}

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := len(res.Skipped); got != 8 {
		t.Errorf("skipped = %d, want 8", got)
	}
	if got := len(res.Created); got != 0 {
		t.Errorf("created = %d, want 0 (everything skipped)", got)
	}
	if got := px.createCalls.Load(); got != 0 {
		t.Errorf("CreateVMWithImport called %d times when all skipped", got)
	}
	if got := px.downloadCalls.Load(); got != 0 {
		t.Errorf("DownloadStorageURL called %d times when all skipped", got)
	}
	// The Skipped Outcomes shouldn't surface the internal sentinel string
	for _, o := range res.Skipped {
		if o.Error != "" {
			t.Errorf("skipped outcome leaked Error=%q", o.Error)
		}
	}
}

func TestBootstrap_Force_RecreatesEvenIfExists(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) { return true, nil }
	svc, database := newSvc(t, px)

	// Pre-seed: pretend everything's already bootstrapped. With Force=false
	// the next call would skip them all; Force=true should ignore the rows.
	vmid := 9000
	for _, node := range []string{"alpha", "bravo"} {
		for _, entry := range bootstrap.Catalog {
			_ = database.Create(&db.NodeTemplate{Node: node, OS: entry.OS, VMID: vmid}).Error
			vmid++
		}
	}

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{Force: true})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := len(res.Created); got != 8 {
		t.Errorf("force should recreate all 8, got %d created (skipped=%d, failed=%d)",
			got, len(res.Skipped), len(res.Failed))
	}
}

func TestBootstrap_PartialFailure_ReportsPerTemplate(t *testing.T) {
	t.Parallel()
	px := happyPX()
	// Make ubuntu-22.04 (offset 1, vmid 9001) downloads fail, others succeed.
	px.downloadStorageURL = func(_ context.Context, n, _, _, _, fn string) (string, error) {
		if fn == "ubuntu-22.04-server-cloudimg-amd64.qcow2" {
			return "", errors.New("network unreachable")
		}
		return "UPID:download", nil
	}
	svc, database := newSvc(t, px)
	_ = database

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// 4 OSes × 2 nodes = 8 attempts, 2 should fail (ubuntu-22.04 on each node)
	if got := len(res.Failed); got != 2 {
		t.Errorf("failed = %d, want 2", got)
	}
	if got := len(res.Created); got != 6 {
		t.Errorf("created = %d, want 6", got)
	}
	for _, f := range res.Failed {
		if f.OS != "ubuntu-22.04" {
			t.Errorf("unexpected failed OS: %s", f.OS)
		}
		if f.Error == "" {
			t.Errorf("failed outcome missing error message")
		}
	}
}

func TestBootstrap_SubsetOSesAndNodes(t *testing.T) {
	t.Parallel()
	px := happyPX()
	svc, database := newSvc(t, px)
	_ = database

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		Nodes: []string{"alpha"},
		OS:    []string{"debian-12"},
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := len(res.Created); got != 1 {
		t.Errorf("created = %d, want 1", got)
	}
	// VMID is now allocated via NextVMIDFrom(TemplateBaseVMID), not derived
	// from base+offset. Assert it landed inside the configured range.
	if res.Created[0].VMID < bootstrap.DefaultTemplateBase {
		t.Errorf("VMID = %d, want >= %d (template range)",
			res.Created[0].VMID, bootstrap.DefaultTemplateBase)
	}
	if res.Created[0].Node != "alpha" {
		t.Errorf("node = %s, want alpha", res.Created[0].Node)
	}
	if res.Created[0].OS != "debian-12" {
		t.Errorf("os = %s, want debian-12", res.Created[0].OS)
	}
}

func TestBootstrap_OfflineNodesSkipped(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "online"},
			{Name: "bravo", Status: "offline"},
		}, nil
	}
	svc, database := newSvc(t, px)
	_ = database

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"ubuntu-24.04"},
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := len(res.Created); got != 1 {
		t.Errorf("created = %d, want 1 (only alpha is online)", got)
	}
	if res.Created[0].Node != "alpha" {
		t.Errorf("got node %s, want alpha", res.Created[0].Node)
	}
}

func TestBootstrap_NoOnlineNodes_ReturnsError(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{
			{Name: "alpha", Status: "offline"},
		}, nil
	}
	svc, database := newSvc(t, px)
	_ = database

	_, err := svc.Bootstrap(context.Background(), bootstrap.Request{})
	if err == nil {
		t.Fatalf("expected error when no nodes online")
	}
}

func TestBootstrap_UnknownOS_ReturnsError(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t, happyPX())
	_, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"freebsd"},
	})
	if err == nil {
		t.Fatalf("expected error for unknown OS")
	}
}

func TestBootstrap_SkipsDownloadWhenImageCached(t *testing.T) {
	t.Parallel()
	px := happyPX()
	// Image is already on the storage.
	px.storageHasFile = func(_ context.Context, _, _, _, _ string) (bool, error) { return true, nil }
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	svc, database := newSvc(t, px)
	_ = database

	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"ubuntu-24.04"},
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := len(res.Created); got != 1 {
		t.Errorf("created = %d, want 1", got)
	}
	// Crucial: download must NOT have been called when the file already existed.
	if got := px.downloadCalls.Load(); got != 0 {
		t.Errorf("DownloadStorageURL called %d times when image was cached, want 0", got)
	}
	// But the rest of the flow ran.
	if got := px.createCalls.Load(); got != 1 {
		t.Errorf("CreateVMWithImport called %d times, want 1", got)
	}
}

func TestBootstrap_StepsCalledInOrder(t *testing.T) {
	t.Parallel()
	var sequence []string
	var mu sync.Mutex

	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	// templateExists isn't called when the DB has no row for (node, os) — the
	// flow short-circuits before the Proxmox check. We don't include it in
	// the expected sequence for this fresh-cluster scenario.
	px.storageHasFile = func(_ context.Context, _, _, _, _ string) (bool, error) {
		mu.Lock()
		sequence = append(sequence, "has-file")
		mu.Unlock()
		return false, nil
	}
	px.downloadStorageURL = func(_ context.Context, _, _, _, _, _ string) (string, error) {
		mu.Lock()
		sequence = append(sequence, "download")
		mu.Unlock()
		return "UPID:dl", nil
	}
	px.nextVMIDFrom = func(_ context.Context, _ int) (int, error) {
		mu.Lock()
		sequence = append(sequence, "nextid")
		mu.Unlock()
		return 9000, nil
	}
	px.createVMWithImport = func(_ context.Context, _ string, _ int, _ proxmox.CreateVMOpts) (string, error) {
		mu.Lock()
		sequence = append(sequence, "create")
		mu.Unlock()
		return "UPID:create", nil
	}
	px.uploadFile = func(_ context.Context, _, _, _, _ string, _ []byte) error {
		mu.Lock()
		sequence = append(sequence, "bake")
		mu.Unlock()
		return nil
	}
	px.convertToTemplate = func(_ context.Context, _ string, _ int) error {
		mu.Lock()
		sequence = append(sequence, "template")
		mu.Unlock()
		return nil
	}
	px.setVMTags = func(_ context.Context, _ string, _ int, _ []string) error {
		mu.Lock()
		sequence = append(sequence, "tag")
		mu.Unlock()
		return nil
	}

	svc, database := newSvc(t, px)
	_ = database
	_, _ = svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"ubuntu-24.04"},
	})

	// Post-D-boot ordering: bake (one-time configure-then-template
	// ceremony) runs between create and convert-to-template, and the
	// nimbus-baked tag is stamped right after convert. The exact bake
	// substeps are exercised by other tests; here we only assert the
	// high-level invariant — bake is in the right place.
	want := []string{"has-file", "download", "nextid", "create", "bake", "template", "tag"}
	if len(sequence) != len(want) {
		t.Fatalf("got %d steps, want %d: %v", len(sequence), len(want), sequence)
	}
	for i, step := range want {
		if sequence[i] != step {
			t.Errorf("step %d = %q, want %q (full: %v)", i, sequence[i], step, sequence)
		}
	}
}

func TestBootstrap_BakeStampsNimbusBakedTag(t *testing.T) {
	t.Parallel()
	px := happyPX()
	// Single OS, single node — keeps assertions readable.
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}
	var (
		mu      sync.Mutex
		gotTags []string
	)
	px.setVMTags = func(_ context.Context, _ string, _ int, tags []string) error {
		mu.Lock()
		gotTags = append(gotTags, tags...)
		mu.Unlock()
		return nil
	}

	svc, _ := newSvc(t, px)
	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"ubuntu-24.04"},
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", res.Failed)
	}
	if len(gotTags) != 1 || gotTags[0] != bootstrap.NimbusBakedTag {
		t.Errorf("tags set = %v, want exactly [%q]", gotTags, bootstrap.NimbusBakedTag)
	}
	if got := px.tagCalls.Load(); got != 1 {
		t.Errorf("SetVMTags called %d times, want 1", got)
	}
}

func TestBootstrap_BakeFailure_ForceStopsAndAbortsConvert(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.getNodes = func(_ context.Context) ([]proxmox.Node, error) {
		return []proxmox.Node{{Name: "alpha", Status: "online"}}, nil
	}

	// QGA probe (the `true` command) responds OK so waitForQGA exits.
	// `cloud-init status --wait` then returns a non-zero exit, which
	// surfaces as a bake error — bake must force-stop the VM and abort
	// before ConvertToTemplate runs.
	bakeErr := errors.New("simulated cloud-init failure")
	px.agentRun = func(_ context.Context, _ string, _ int, cmd []string, _ string, _ time.Duration) (*proxmox.AgentExecStatus, error) {
		if len(cmd) > 0 && cmd[0] == "cloud-init" {
			return nil, bakeErr
		}
		return &proxmox.AgentExecStatus{Exited: 1, ExitCode: 0}, nil
	}

	var stopCalls atomic.Int32
	px.stopVM = func(_ context.Context, _ string, _ int) (string, error) {
		stopCalls.Add(1)
		return "UPID:stop", nil
	}

	svc, _ := newSvc(t, px)
	res, err := svc.Bootstrap(context.Background(), bootstrap.Request{
		OS: []string{"ubuntu-24.04"},
	})
	if err != nil {
		t.Fatalf("Bootstrap (outer): %v", err)
	}

	// Single OS × single node → exactly one Failed outcome whose error
	// message carries the bake context. ConvertToTemplate must NOT have
	// been called.
	if len(res.Failed) != 1 {
		t.Fatalf("failed = %d, want 1: %+v", len(res.Failed), res.Failed)
	}
	if !strings.Contains(res.Failed[0].Error, "bake") {
		t.Errorf("failure message %q does not mention bake", res.Failed[0].Error)
	}
	if got := px.convertCalls.Load(); got != 0 {
		t.Errorf("ConvertToTemplate called %d times during a bake failure, want 0", got)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Errorf("StopVM called %d times after bake failure, want 1 (force-stop in deferred cleanup)", got)
	}
}

// CheckTemplatesStatus must report a row as baked WITHOUT scanning the
// node when the DB pointer is still good — both to stay cheap and to
// avoid stamping false positives.
func TestCheckTemplatesStatus_RowBaked_NoNodeScan(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) { return true, nil }

	var listCalls atomic.Int32
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		listCalls.Add(1)
		return nil, nil
	}

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9000}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	st, err := svc.CheckTemplatesStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckTemplatesStatus: %v", err)
	}
	if st.Total != 1 || st.Baked != 1 || st.Unbaked != 0 {
		t.Errorf("status = %+v, want Total=1 Baked=1 Unbaked=0", st)
	}
	if got := listCalls.Load(); got != 0 {
		t.Errorf("ListVMs called %d times for a good row, want 0", got)
	}
}

// The reconciliation case: another Nimbus instance rebuilt the templates
// on a shared cluster, leaving this instance's DB pointer dangling. The
// status check should discover the live baked sibling, rewrite the row,
// and report baked.
func TestCheckTemplatesStatus_AdoptsBakedSiblingOnStaleRow(t *testing.T) {
	t.Parallel()
	px := happyPX()
	const stale, fresh = 9000, 9100
	px.templateExists = func(_ context.Context, _ string, vmid int) (bool, error) {
		switch vmid {
		case stale:
			return false, nil // old VMID is gone / unbaked
		case fresh:
			return true, nil // sibling Nimbus baked this one
		}
		return false, nil
	}
	px.listVMs = func(_ context.Context, n string) ([]proxmox.VMStatus, error) {
		if n != "alpha" {
			return nil, nil
		}
		return []proxmox.VMStatus{
			{VMID: fresh, Name: "ubuntu-24.04-template", Template: 1},
			{VMID: 100, Name: "some-user-vm", Template: 0},
		}, nil
	}

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: stale}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	st, err := svc.CheckTemplatesStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckTemplatesStatus: %v", err)
	}
	if st.Total != 1 || st.Baked != 1 || st.Unbaked != 0 {
		t.Errorf("status = %+v, want Total=1 Baked=1 Unbaked=0 after adoption", st)
	}
	if st.Details[0].VMID != fresh {
		t.Errorf("Details[0].VMID = %d, want %d (adopted sibling)", st.Details[0].VMID, fresh)
	}

	// DB row must be rewritten so provision picks up the live template.
	var got db.NodeTemplate
	if err := database.DB.Where("node = ? AND os = ?", "alpha", "ubuntu-24.04").First(&got).Error; err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if got.VMID != fresh {
		t.Errorf("row vmid = %d, want %d", got.VMID, fresh)
	}
}

// If the node has no baked sibling, reconciliation must leave the row
// alone and report unbaked — that's the genuine "rebuild needed" path.
func TestCheckTemplatesStatus_NoSibling_StaysUnbaked(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) { return false, nil }
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		// Templates on the node, but none named ubuntu-template.
		return []proxmox.VMStatus{
			{VMID: 9100, Name: "debian-template", Template: 1},
		}, nil
	}

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9000}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	st, err := svc.CheckTemplatesStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckTemplatesStatus: %v", err)
	}
	if st.Total != 1 || st.Baked != 0 || st.Unbaked != 1 {
		t.Errorf("status = %+v, want Total=1 Baked=0 Unbaked=1", st)
	}
	var got db.NodeTemplate
	if err := database.DB.Where("node = ? AND os = ?", "alpha", "ubuntu-24.04").First(&got).Error; err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if got.VMID != 9000 {
		t.Errorf("row vmid = %d, want 9000 (untouched when no sibling)", got.VMID)
	}
}

// A discovered sibling whose tag check fails must NOT be adopted — we
// only heal toward a confirmed-baked template.
func TestCheckTemplatesStatus_SiblingNotBaked_NoAdopt(t *testing.T) {
	t.Parallel()
	px := happyPX()
	px.templateExists = func(_ context.Context, _ string, _ int) (bool, error) { return false, nil }
	px.listVMs = func(_ context.Context, _ string) ([]proxmox.VMStatus, error) {
		return []proxmox.VMStatus{
			{VMID: 9100, Name: "ubuntu-24.04-template", Template: 1}, // present but unbaked
		}, nil
	}

	svc, database := newSvc(t, px)
	if err := database.DB.Create(&db.NodeTemplate{Node: "alpha", OS: "ubuntu-24.04", VMID: 9000}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	st, err := svc.CheckTemplatesStatus(context.Background())
	if err != nil {
		t.Fatalf("CheckTemplatesStatus: %v", err)
	}
	if st.Unbaked != 1 {
		t.Errorf("status = %+v, want Unbaked=1 (sibling lacks baked tag)", st)
	}
	var got db.NodeTemplate
	if err := database.DB.Where("node = ? AND os = ?", "alpha", "ubuntu-24.04").First(&got).Error; err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if got.VMID != 9000 {
		t.Errorf("row vmid = %d, want 9000 (untouched)", got.VMID)
	}
}
