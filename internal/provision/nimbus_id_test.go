package provision_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
)

// newBackfillService is a focused harness: just enough infra to run
// BackfillNimbusMetadata against a seeded DB. The full happyFakePVE has
// the entire provision flow wired up; here we only need GetVMConfig +
// SetVMTags + SetVMDescription.
func newBackfillService(t *testing.T, fake *fakePVE) (*provision.Service, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backfill.db")
	database, err := db.New(path, ippool.Model(), &db.VM{}, &db.NodeTemplate{}, &db.SSHKey{}, &db.Node{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	pool := ippool.New(database.DB)
	keysSvc := sshkeys.New(database.DB, cipher)
	svc := provision.New(fake, pool, database.DB, cipher, keysSvc, provision.Config{})
	return svc, database
}

// TestBackfillNimbusMetadata_MintsIDForLegacyRow: a row with no nimbus_id
// and a PVE side that has no marker gets a freshly-minted UUIDv7, the tag
// pair stamped, and the description marker written. The DB row picks up
// nimbus_id + smbios_id in the same pass.
func TestBackfillNimbusMetadata_MintsIDForLegacyRow(t *testing.T) {
	t.Parallel()
	const wantSMBIOS = "11111111-2222-3333-4444-555566667777"
	var (
		mu          sync.Mutex
		setTagsArgs []string
		setDescArg  string
		tagsCalls   int
		descCalls   int
	)
	fake := &fakePVE{
		getVMConfig: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags":        "user-tag",
				"description": "Owned by data team.",
				"smbios1":     "uuid=" + wantSMBIOS + ",family=acme",
			}, nil
		},
		setVMTags: func(_ context.Context, _ string, _ int, tags []string) error {
			mu.Lock()
			defer mu.Unlock()
			tagsCalls++
			setTagsArgs = append([]string(nil), tags...)
			return nil
		},
		setVMDescription: func(_ context.Context, _ string, _ int, desc string) error {
			mu.Lock()
			defer mu.Unlock()
			descCalls++
			setDescArg = desc
			return nil
		},
	}
	svc, database := newBackfillService(t, fake)
	row := &db.VM{
		VMID:       142,
		Hostname:   "legacy-1",
		IP:         "10.0.0.42",
		Node:       "alpha",
		Tier:       "small",
		OSTemplate: "ubuntu-22.04",
		Status:     "running",
	}
	if err := database.Create(row).Error; err != nil {
		t.Fatalf("seed legacy vm: %v", err)
	}

	n, err := svc.BackfillNimbusMetadata(context.Background())
	if err != nil {
		t.Fatalf("BackfillNimbusMetadata: %v", err)
	}
	if n != 1 {
		t.Errorf("updated count = %d, want 1", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if tagsCalls != 1 || descCalls != 1 {
		t.Errorf("expected exactly 1 SetVMTags + 1 SetVMDescription, got tags=%d desc=%d", tagsCalls, descCalls)
	}

	hasMarker := false
	hasShortID := false
	for _, tag := range setTagsArgs {
		if tag == proxmox.NimbusMarkerTag {
			hasMarker = true
		}
		if strings.HasPrefix(tag, proxmox.NimbusIDTagPrefix) {
			hasShortID = true
		}
	}
	if !hasMarker || !hasShortID {
		t.Errorf("SetVMTags args = %v; want both nimbus marker and nimbus-id-* tag", setTagsArgs)
	}

	// Re-read the row — the backfill should have persisted a UUID-shaped id
	// and the smbios uuid we returned from getVMConfig.
	var refreshed db.VM
	if err := database.DB.First(&refreshed, row.ID).Error; err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if refreshed.NimbusID == "" || len(strings.Split(refreshed.NimbusID, "-")) != 5 {
		t.Errorf("nimbus_id = %q, want a UUIDv7-shaped string", refreshed.NimbusID)
	}
	if refreshed.SMBIOSID != wantSMBIOS {
		t.Errorf("smbios_id = %q, want %q", refreshed.SMBIOSID, wantSMBIOS)
	}
	if !strings.Contains(setDescArg, "id="+refreshed.NimbusID) {
		t.Errorf("description %q does not contain id=%s", setDescArg, refreshed.NimbusID)
	}
}

// TestBackfillNimbusMetadata_AdoptsIDFromPVEDescription: when the PVE-side
// description already carries an id (e.g. row was provisioned by a peer
// instance, or this row's NimbusID column was wiped by an op), the backfill
// must adopt that id rather than mint a new one — otherwise two instances
// would each pick a different uuid for the same VM.
func TestBackfillNimbusMetadata_AdoptsIDFromPVEDescription(t *testing.T) {
	t.Parallel()
	const existingID = "0193abcd-4321-7000-89ab-cdef01234567"
	existingDesc := proxmox.EncodeNimbusDescription("small", "ubuntu-22.04", existingID)
	fake := &fakePVE{
		getVMConfig: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags":        "nimbus;nimbus-id-" + proxmox.ShortNimbusID(existingID),
				"description": existingDesc,
			}, nil
		},
	}
	svc, database := newBackfillService(t, fake)
	row := &db.VM{
		VMID:       143,
		Hostname:   "legacy-2",
		IP:         "10.0.0.43",
		Node:       "alpha",
		Tier:       "small",
		OSTemplate: "ubuntu-22.04",
		Status:     "running",
	}
	if err := database.Create(row).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	if _, err := svc.BackfillNimbusMetadata(context.Background()); err != nil {
		t.Fatalf("BackfillNimbusMetadata: %v", err)
	}

	var refreshed db.VM
	if err := database.DB.First(&refreshed, row.ID).Error; err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if refreshed.NimbusID != existingID {
		t.Errorf("nimbus_id = %q, want %q (must adopt PVE-side id, not mint a new one)", refreshed.NimbusID, existingID)
	}
}

// TestBackfillNimbusMetadata_NoopWhenAlreadyConverged: a row + PVE state
// that already carry matching id, tags, and description shouldn't trigger
// any writes. Guards against unnecessary PVE API churn on every startup.
func TestBackfillNimbusMetadata_NoopWhenAlreadyConverged(t *testing.T) {
	t.Parallel()
	const settledID = "0193abcd-4321-7000-89ab-cdef01234567"
	var setTagsCalls, setDescCalls int
	var mu sync.Mutex
	fake := &fakePVE{
		getVMConfig: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags":        "nimbus;nimbus-id-" + proxmox.ShortNimbusID(settledID),
				"description": proxmox.EncodeNimbusDescription("small", "ubuntu-22.04", settledID),
			}, nil
		},
		setVMTags: func(_ context.Context, _ string, _ int, _ []string) error {
			mu.Lock()
			setTagsCalls++
			mu.Unlock()
			return nil
		},
		setVMDescription: func(_ context.Context, _ string, _ int, _ string) error {
			mu.Lock()
			setDescCalls++
			mu.Unlock()
			return nil
		},
	}
	svc, database := newBackfillService(t, fake)
	if err := database.Create(&db.VM{
		VMID:       144,
		Hostname:   "settled-1",
		IP:         "10.0.0.44",
		Node:       "alpha",
		Tier:       "small",
		OSTemplate: "ubuntu-22.04",
		Status:     "running",
		NimbusID:   settledID,
	}).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}

	n, err := svc.BackfillNimbusMetadata(context.Background())
	if err != nil {
		t.Fatalf("BackfillNimbusMetadata: %v", err)
	}
	if n != 0 {
		t.Errorf("updated count = %d, want 0 (idempotent)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if setTagsCalls != 0 || setDescCalls != 0 {
		t.Errorf("expected no PVE writes when already converged, got tags=%d desc=%d", setTagsCalls, setDescCalls)
	}
}

// TestBackfillNimbusMetadata_ContinuesAfterPerRowFailure: a PVE write
// failure on one row must not stall the rest of the walk. Per-VM errors
// log + continue; aggregate counter only reflects successes.
func TestBackfillNimbusMetadata_ContinuesAfterPerRowFailure(t *testing.T) {
	t.Parallel()
	fake := &fakePVE{
		getVMConfig: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 999 {
				return nil, errors.New("simulated read failure")
			}
			return map[string]any{
				"tags":        "user-tag",
				"description": "",
			}, nil
		},
	}
	svc, database := newBackfillService(t, fake)
	rows := []db.VM{
		{VMID: 999, Hostname: "broken", IP: "10.0.0.99", Node: "alpha", Tier: "small", OSTemplate: "ubuntu-22.04", Status: "running"},
		{VMID: 200, Hostname: "ok", IP: "10.0.0.200", Node: "alpha", Tier: "small", OSTemplate: "ubuntu-22.04", Status: "running"},
	}
	for i := range rows {
		if err := database.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	n, err := svc.BackfillNimbusMetadata(context.Background())
	if err != nil {
		t.Fatalf("BackfillNimbusMetadata: %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1 (one row failed, one succeeded)", n)
	}
}
