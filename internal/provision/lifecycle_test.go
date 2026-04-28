package provision_test

import (
	"context"
	"errors"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/provision"
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
