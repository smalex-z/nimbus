package provision_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/provision"
)

// seedManyOwnedVMs creates n VMs owned by ownerID with unique
// vmid/hostname/ip values so the unique-index constraints don't trip.
func seedManyOwnedVMs(t *testing.T, database *db.DB, ownerID uint, n int) {
	t.Helper()
	owner := ownerID
	for i := 0; i < n; i++ {
		row := &db.VM{
			VMID:       9000 + i,
			Hostname:   "owned-" + strconv.Itoa(i),
			IP:         "10.0.0." + strconv.Itoa(100+i),
			Node:       "alpha",
			Tier:       "small",
			OSTemplate: "ubuntu-24.04",
			Status:     "running",
			OwnerID:    &owner,
		}
		if err := database.Create(row).Error; err != nil {
			t.Fatalf("seed vm %d: %v", i, err)
		}
	}
}

func TestProvision_MemberAtCapRejected(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	owner := uint(42)
	seedManyOwnedVMs(t, database, owner, provision.MemberMaxVMs)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "over-cap",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
		OwnerID:    &owner,
	}, nil)

	var conflict *internalerrors.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Provision over cap: err = %v, want ConflictError", err)
	}
}

func TestProvision_AdminBypassesCap(t *testing.T) {
	t.Parallel()
	svc, _, database := newTestService(t, happyFakePVE(t))
	owner := uint(42)
	seedManyOwnedVMs(t, database, owner, provision.MemberMaxVMs)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:         "admin-bypass",
		Tier:             "small",
		OSTemplate:       "ubuntu-24.04",
		SSHPubKey:        realPubKey(t),
		OwnerID:          &owner,
		RequesterIsAdmin: true,
	}, nil)
	if err != nil {
		t.Fatalf("admin should bypass member cap: %v", err)
	}
}

func TestProvision_NilOwnerBypassesCap(t *testing.T) {
	t.Parallel()
	// Legacy / test paths without OwnerID must not be quota-gated. The
	// Phase-1 Provision() callers (and the existing test corpus) all pass
	// nil owner.
	svc, _, database := newTestService(t, happyFakePVE(t))
	owner := uint(42)
	seedManyOwnedVMs(t, database, owner, provision.MemberMaxVMs)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "no-owner",
		Tier:       "small",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
		OwnerID:    nil,
	}, nil)
	if err != nil {
		t.Fatalf("nil-owner provision must not be quota-gated: %v", err)
	}
}

func TestProvision_MemberDisallowedTier(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	owner := uint(42)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:   "xl-as-member",
		Tier:       "xl",
		OSTemplate: "ubuntu-24.04",
		SSHPubKey:  realPubKey(t),
		OwnerID:    &owner,
	}, nil)

	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Provision(xl as member): err = %v, want ValidationError", err)
	}
	if ve.Field != "tier" {
		t.Errorf("ValidationError.Field = %q, want tier", ve.Field)
	}
}

func TestProvision_AdminMayUseDisallowedTier(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t, happyFakePVE(t))
	owner := uint(42)

	_, err := svc.Provision(context.Background(), provision.Request{
		Hostname:         "xl-as-admin",
		Tier:             "xl",
		OSTemplate:       "ubuntu-24.04",
		SSHPubKey:        realPubKey(t),
		OwnerID:          &owner,
		RequesterIsAdmin: true,
	}, nil)
	// Admins clear the allowlist; they may still hit unrelated downstream
	// errors (e.g. tier xl unknown to nodescore.Tiers). The contract this
	// test enforces is: the *member-allowlist* gate did not fire. Failing
	// later for unrelated reasons is fine, but we should not see a
	// ValidationError on the "tier" field with the admin-only message.
	if err == nil {
		return // happy path
	}
	var ve *internalerrors.ValidationError
	if errors.As(err, &ve) && ve.Field == "tier" && ve.Message == `tier "xl" is admin-only` {
		t.Fatalf("admin should bypass member tier allowlist, got %v", err)
	}
}
