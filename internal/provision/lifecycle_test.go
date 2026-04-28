package provision_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// seedOwnedVM seeds a vms row owned by ownerID at the given (vmid, node, ip).
// Returns the new row id so tests can call lifecycle ops against it.
func seedOwnedVM(t *testing.T, database *db.DB, ownerID uint, hostname string, vmid int, node string) uint {
	t.Helper()
	owner := ownerID
	row := &db.VM{
		VMID:       vmid,
		Hostname:   hostname,
		IP:         "10.0.0.99",
		Node:       node,
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		Status:     "running",
		OwnerID:    &owner,
	}
	if err := database.Create(row).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	return row.ID
}

func TestLifecycleOp_RoutesEachOpAndUpdatesStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op         provision.VMLifecycleOp
		wantStatus string
	}{
		{provision.VMOpStart, "running"},
		{provision.VMOpShutdown, "stopped"},
		{provision.VMOpStop, "stopped"},
		{provision.VMOpReboot, "running"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			fake := happyFakePVE(t)
			svc, _, database := newTestService(t, fake)
			id := seedOwnedVM(t, database, 42, "vm-a", 200, "alpha")

			if err := svc.LifecycleOp(context.Background(), id, 42, tc.op); err != nil {
				t.Fatalf("LifecycleOp(%s): %v", tc.op, err)
			}
			var got db.VM
			if err := database.First(&got, id).Error; err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status after %s = %q, want %q", tc.op, got.Status, tc.wantStatus)
			}
		})
	}
}

func TestLifecycleOp_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "vm-a", 200, "alpha")

	err := svc.LifecycleOp(context.Background(), id, 99, provision.VMOpStart)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("err = %v, want NotFoundError (existence must not be disclosed)", err)
	}
}

func TestLifecycleOp_RejectsUnknownOp(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "vm-a", 200, "alpha")

	err := svc.LifecycleOp(context.Background(), id, 42, provision.VMLifecycleOp("nuke"))
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("err = %v, want ValidationError", err)
	}
}

func TestAdminLifecycleByVMID_WorksWithoutLocalRow(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, _ := newTestService(t, fake)

	// No local row at all — emulates a foreign / external VM. The op must
	// still go through (proxmox call succeeds), without erroring.
	if err := svc.AdminLifecycleByVMID(context.Background(), "alpha", 9999, provision.VMOpStart); err != nil {
		t.Fatalf("AdminLifecycleByVMID on absent local row: %v", err)
	}
}

func TestAdminLifecycleByVMID_StampsStatusOnLocalRow(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	svc, _, database := newTestService(t, fake)
	id := seedOwnedVM(t, database, 42, "vm-a", 200, "alpha")

	if err := svc.AdminLifecycleByVMID(context.Background(), "alpha", 200, provision.VMOpShutdown); err != nil {
		t.Fatalf("AdminLifecycleByVMID: %v", err)
	}
	var got db.VM
	if err := database.First(&got, id).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status != "stopped" {
		t.Errorf("local status after admin shutdown = %q, want stopped", got.Status)
	}
}

func TestListWithLiveStatus_OverwritesWhenProxmoxDisagrees(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{{VMID: 200, Node: "alpha", Status: "stopped"}}, nil
	}
	svc, _, database := newTestService(t, fake)
	owner := uint(42)
	seedOwnedVM(t, database, owner, "vm-a", 200, "alpha")

	got, err := svc.ListWithLiveStatus(context.Background(), &owner)
	if err != nil {
		t.Fatalf("ListWithLiveStatus: %v", err)
	}
	if len(got) != 1 || got[0].Status != "stopped" {
		t.Fatalf("status = %+v, want one row with status=stopped", got)
	}
}

func TestListWithLiveStatus_KeepsDBStatusWhenProxmoxSilent(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return nil, nil // VM not yet visible to Proxmox (e.g. mid-provision)
	}
	svc, _, database := newTestService(t, fake)
	owner := uint(42)
	row := &db.VM{
		VMID: 200, Hostname: "vm-a", Node: "alpha",
		Tier: "small", OSTemplate: "ubuntu-24.04",
		Status: "provisioning", OwnerID: &owner,
	}
	if err := database.Create(row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := svc.ListWithLiveStatus(context.Background(), &owner)
	if err != nil {
		t.Fatalf("ListWithLiveStatus: %v", err)
	}
	if len(got) != 1 || got[0].Status != "provisioning" {
		t.Fatalf("status = %+v, want one row with status=provisioning", got)
	}
}

func TestListWithLiveStatus_FallsBackOnProxmoxError(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return nil, errors.New("proxmox boom")
	}
	svc, _, database := newTestService(t, fake)
	owner := uint(42)
	seedOwnedVM(t, database, owner, "vm-a", 200, "alpha")

	got, err := svc.ListWithLiveStatus(context.Background(), &owner)
	if err != nil {
		t.Fatalf("ListWithLiveStatus: %v", err)
	}
	if len(got) != 1 || got[0].Status != "running" {
		t.Fatalf("status = %+v, want DB-side running on Proxmox failure", got)
	}
}

func TestListWithLiveStatus_ScopedToOwner(t *testing.T) {
	t.Parallel()
	fake := happyFakePVE(t)
	fake.getClusterVMs = func(_ context.Context) ([]proxmox.ClusterVM, error) {
		return []proxmox.ClusterVM{
			{VMID: 200, Node: "alpha", Status: "stopped"},
			{VMID: 201, Node: "alpha", Status: "stopped"},
		}, nil
	}
	svc, _, database := newTestService(t, fake)
	mine := uint(42)
	other := uint(99)
	seedOwnedVM(t, database, mine, "mine", 200, "alpha")
	seedOwnedVM(t, database, other, "theirs", 201, "alpha")

	got, err := svc.ListWithLiveStatus(context.Background(), &mine)
	if err != nil {
		t.Fatalf("ListWithLiveStatus: %v", err)
	}
	if len(got) != 1 || got[0].VMID != 200 {
		t.Fatalf("got = %+v, want only owner=42's row", got)
	}
}

// TestLifecycleOp_IdempotentWhenAlreadyInTargetState exercises the user-clicks-
// Start-on-already-running flow that surfaced as a 500 in production. Proxmox
// returns a UPID and then the qmstart task fails with "VM N already running"
// from qmeventd. The dispatcher should swallow that specific failure and
// still write the local status, so the UI sees the click as a no-op success
// instead of a confusing internal-server-error toast.
func TestLifecycleOp_IdempotentWhenAlreadyInTargetState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op     provision.VMLifecycleOp
		taskID string // matches what RebootVM/StartVM/etc. return
		errMsg string // simulates a WaitForTask failure
	}{
		{provision.VMOpStart, "task:start", "task UPID:n:00:00:69:qmstart:200: failed: VM 200 already running"},
		{provision.VMOpShutdown, "task:shutdown", "task UPID:n:00:00:69:qmshutdown:200: failed: VM 200 not running"},
		{provision.VMOpStop, "task:stop", "task UPID:n:00:00:69:qmstop:200: failed: VM 200 already stopped"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			fake := happyFakePVE(t)
			// Make the relevant op return its task ID, then make WaitForTask
			// return the simulated qmeventd failure.
			fake.startVM = func(_ context.Context, _ string, _ int) (string, error) { return tc.taskID, nil }
			fake.stopVM = func(_ context.Context, _ string, _ int) (string, error) { return tc.taskID, nil }
			fake.shutdownVM = func(_ context.Context, _ string, _ int) (string, error) { return tc.taskID, nil }
			fake.waitForTask = func(_ context.Context, _, taskID string, _ time.Duration) error {
				if taskID == tc.taskID {
					return fmt.Errorf("%s", tc.errMsg)
				}
				return nil
			}
			svc, _, database := newTestService(t, fake)
			id := seedOwnedVM(t, database, 42, "vm-a", 200, "alpha")

			if err := svc.LifecycleOp(context.Background(), id, 42, tc.op); err != nil {
				t.Fatalf("LifecycleOp(%s) on already-in-target-state VM should be a no-op: %v", tc.op, err)
			}
		})
	}
}
