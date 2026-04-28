package provision_test

import (
	"context"
	"errors"
	"testing"

	internalerrors "nimbus/internal/errors"
	"nimbus/internal/provision"
)

func TestGet_OwnerSeesVM(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	owner := uint(42)
	vm, err := svc.Get(context.Background(), id, &owner)
	if err != nil {
		t.Fatalf("Get(owner): %v", err)
	}
	if vm.ID != id {
		t.Errorf("Get returned wrong VM: got id=%d, want id=%d", vm.ID, id)
	}
}

func TestGet_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	other := uint(99)
	_, err := svc.Get(context.Background(), id, &other)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("Get(non-owner): err = %v, want NotFoundError (existence must not be disclosed)", err)
	}
}

func TestGet_NilRequesterBypassesOwnership(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	vm, err := svc.Get(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("Get(nil): %v", err)
	}
	if vm.ID != id {
		t.Errorf("Get(nil) returned wrong VM: got id=%d, want id=%d", vm.ID, id)
	}
}

func TestListVMTunnels_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	other := uint(99)
	_, err := svc.ListVMTunnels(context.Background(), id, &other)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("ListVMTunnels(non-owner): err = %v, want NotFoundError", err)
	}
}

func TestCreateVMTunnel_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	other := uint(99)
	_, err := svc.CreateVMTunnel(context.Background(), id, provision.VMTunnelRequest{
		TargetPort: 8080,
		Transport:  "tcp",
	}, &other)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("CreateVMTunnel(non-owner): err = %v, want NotFoundError (existence must not be disclosed; non-owner must not learn deployment-config state either)", err)
	}
}

func TestDeleteVMTunnel_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	id := seedOwnedVM(t, database, 42, "owned-vm", 200, "alpha")

	other := uint(99)
	err := svc.DeleteVMTunnel(context.Background(), id, "tunnel-abc", &other)
	var notFound *internalerrors.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("DeleteVMTunnel(non-owner): err = %v, want NotFoundError", err)
	}
}
