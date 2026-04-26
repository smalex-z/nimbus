package sshkeys_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
)

func newTestService(t *testing.T) (*sshkeys.Service, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.SSHKey{}, &db.VM{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return sshkeys.New(database.DB, cipher), database
}

func TestCreate_GenerateKey_StoresEncryptedPrivate(t *testing.T) {
	t.Parallel()
	svc, database := newTestService(t)

	row, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:     "my-laptop",
		Generate: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(row.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public key prefix unexpected: %q", row.PublicKey)
	}
	if row.Source != sshkeys.SourceGenerated {
		t.Errorf("Source = %q, want generated", row.Source)
	}

	// The persisted row holds ciphertext, not plaintext.
	var disk db.SSHKey
	if err := database.First(&disk, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !disk.HasPrivateKey() {
		t.Fatal("expected vaulted private key")
	}
	if strings.Contains(string(disk.PrivKeyCT), "BEGIN OPENSSH") {
		t.Fatal("private key was stored as plaintext")
	}

	// GetPrivateKey round-trips back to plaintext.
	name, plain, err := svc.GetPrivateKey(context.Background(), row.ID, nil)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if name != "my-laptop" {
		t.Errorf("name = %q, want my-laptop", name)
	}
	if !strings.Contains(plain, "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("expected OpenSSH PEM, got %q", plain)
	}
}

func TestCreate_PublicOnly_NoVaultEntry(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pub, _, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:      "office-key",
		PublicKey: pub,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.HasPrivateKey() {
		t.Fatal("expected no vault entry for public-only import")
	}

	_, _, err = svc.GetPrivateKey(context.Background(), row.ID, nil)
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestCreate_MismatchedKeypair_Rejected(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pubA, _, _ := sshkeys.GenerateEd25519()
	_, privB, _ := sshkeys.GenerateEd25519()

	_, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:       "mismatched",
		PublicKey:  pubA,
		PrivateKey: privB,
	})
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Field != "private_key" {
		t.Errorf("ValidationError.Field = %q, want private_key", ve.Field)
	}
}

func TestCreate_DuplicateName_Conflict(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pub, _, _ := sshkeys.GenerateEd25519()
	if _, err := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "dup", PublicKey: pub}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "dup", PublicKey: pub})
	var ce *internalerrors.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
}

func TestCreate_InvalidName_Rejected(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	for _, name := range []string{"", "Name With Spaces", "UPPER", "trailing-", "trailing_", ".leading-dot", "_leading-underscore", "has/slash"} {
		_, err := svc.Create(context.Background(), sshkeys.CreateRequest{Name: name, Generate: true})
		var ve *internalerrors.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("name %q: expected ValidationError, got %v", name, err)
		}
	}
}

// Common SSH key filenames must round-trip without renaming, since users
// often want their stored key to match the on-disk filename they imported.
func TestCreate_CommonSSHFilenames_Accepted(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	for _, name := range []string{"id_rsa", "id_ed25519", "id_rsa.pub", "my.laptop", "prod-server", "a"} {
		_, err := svc.Create(context.Background(), sshkeys.CreateRequest{Name: name, Generate: true})
		if err != nil {
			t.Errorf("name %q: expected accept, got %v", name, err)
		}
	}
}

func TestAttachPrivateKey_RoundTrips(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pub, priv, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:      "pub-only",
		PublicKey: pub,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if key.HasPrivateKey() {
		t.Fatal("expected pub-only key on create")
	}

	if err := svc.AttachPrivateKey(context.Background(), key.ID, priv, nil); err != nil {
		t.Fatalf("AttachPrivateKey: %v", err)
	}
	_, got, err := svc.GetPrivateKey(context.Background(), key.ID, nil)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if got != priv {
		t.Errorf("vault returned different private key than was attached")
	}
}

func TestAttachPrivateKey_AlreadyVaulted_Conflict(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	key, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name: "has-priv", Generate: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, priv, _ := sshkeys.GenerateEd25519()

	err = svc.AttachPrivateKey(context.Background(), key.ID, priv, nil)
	var ce *internalerrors.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
}

func TestAttachPrivateKey_Mismatched_Rejected(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pubA, _, _ := sshkeys.GenerateEd25519()
	_, privB, _ := sshkeys.GenerateEd25519()
	key, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:      "pub-a",
		PublicKey: pubA,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = svc.AttachPrivateKey(context.Background(), key.ID, privB, nil)
	var ve *internalerrors.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Field != "private_key" {
		t.Errorf("Field = %q, want private_key", ve.Field)
	}
}

func TestAttachPrivateKey_UnknownID_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, priv, _ := sshkeys.GenerateEd25519()
	err := svc.AttachPrivateKey(context.Background(), 999, priv, nil)
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestSetDefault_OnlyOneAtATime(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	a, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "a", Generate: true, SetDefault: true})
	b, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "b", Generate: true})

	if !a.IsDefault {
		t.Fatal("a should be default after Create(SetDefault=true)")
	}

	if err := svc.SetDefault(context.Background(), b.ID, nil); err != nil {
		t.Fatalf("SetDefault(b): %v", err)
	}

	got, err := svc.GetDefault(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetDefault: %v", err)
	}
	if got.ID != b.ID {
		t.Errorf("default ID = %d, want %d (b)", got.ID, b.ID)
	}

	// a should no longer be default.
	keys, _ := svc.List(context.Background(), nil)
	for _, k := range keys {
		if k.ID == a.ID && k.IsDefault {
			t.Error("a is still marked default after b was promoted")
		}
	}
}

func TestGetDefault_NoneSet_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.GetDefault(context.Background(), nil)
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
}

func TestDelete_ClearsVMReferences(t *testing.T) {
	t.Parallel()
	svc, database := newTestService(t)

	key, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "tmp", Generate: true})

	// VM rows pointing at this key need their FK nulled out on delete.
	keyID := key.ID
	vm := &db.VM{
		VMID: 100, Hostname: "vm-1", IP: "10.0.0.1", Node: "alpha",
		Tier: "small", OSTemplate: "ubuntu-24.04", Status: "running",
		SSHKeyID: &keyID,
	}
	if err := database.Create(vm).Error; err != nil {
		t.Fatalf("seed vm: %v", err)
	}

	if err := svc.Delete(context.Background(), key.ID, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var loaded db.VM
	if err := database.First(&loaded, vm.ID).Error; err != nil {
		t.Fatal(err)
	}
	if loaded.SSHKeyID != nil {
		t.Errorf("VM.SSHKeyID = %v, want nil after key delete", *loaded.SSHKeyID)
	}

	_, err := svc.Get(context.Background(), key.ID, nil)
	var nf *internalerrors.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("expected key to be gone, got err=%v", err)
	}
}

func TestList_DefaultsFirst(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, _ = svc.Create(context.Background(), sshkeys.CreateRequest{Name: "alpha", Generate: true})
	def, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "beta", Generate: true, SetDefault: true})

	keys, err := svc.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	if keys[0].ID != def.ID {
		t.Errorf("default key not listed first; got order [%d, %d]", keys[0].ID, keys[1].ID)
	}
}

func TestCreate_FingerprintIsStable(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pub, _, _ := sshkeys.GenerateEd25519()
	a, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "a", PublicKey: pub})
	b, _ := svc.Create(context.Background(), sshkeys.CreateRequest{Name: "b", PublicKey: pub})

	if a.Fingerprint == "" || b.Fingerprint == "" {
		t.Fatal("expected fingerprints to be populated")
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("same public key produced different fingerprints: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
	if !strings.HasPrefix(a.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint missing SHA256: prefix: %q", a.Fingerprint)
	}
}

// newTestServiceWithUsers extends newTestService by also migrating the User
// schema, used by the ownership tests below.
func newTestServiceWithUsers(t *testing.T) (*sshkeys.Service, *db.DB) {
	t.Helper()
	svc, database := newTestService(t)
	if err := database.AutoMigrate(&db.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	return svc, database
}

func TestBackfillOwnership(t *testing.T) {
	t.Parallel()
	svc, database := newTestServiceWithUsers(t)

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

	pub, _, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatalf("generate pubkey: %v", err)
	}

	memberID := member.ID
	legacy1 := db.SSHKey{Name: "legacy1", PublicKey: pub, Source: "imported"}
	legacy2 := db.SSHKey{Name: "legacy2", PublicKey: pub, Source: "imported"}
	owned := db.SSHKey{Name: "owned", PublicKey: pub, Source: "imported", OwnerID: &memberID}
	for _, k := range []*db.SSHKey{&legacy1, &legacy2, &owned} {
		if err := database.Create(k).Error; err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	n, err := svc.BackfillOwnership(context.Background())
	if err != nil {
		t.Fatalf("BackfillOwnership: %v", err)
	}
	if n != 2 {
		t.Errorf("first run rows affected: got %d, want 2", n)
	}

	var l1, l2, o db.SSHKey
	database.First(&l1, legacy1.ID)
	database.First(&l2, legacy2.ID)
	database.First(&o, owned.ID)
	if l1.OwnerID == nil || *l1.OwnerID != admin.ID {
		t.Errorf("legacy1 owner: got %v, want %d", l1.OwnerID, admin.ID)
	}
	if l2.OwnerID == nil || *l2.OwnerID != admin.ID {
		t.Errorf("legacy2 owner: got %v, want %d", l2.OwnerID, admin.ID)
	}
	if o.OwnerID == nil || *o.OwnerID != memberID {
		t.Errorf("owned key clobbered: got %v, want %d", o.OwnerID, memberID)
	}

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
	svc, database := newTestServiceWithUsers(t)
	pub, _, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatalf("generate pubkey: %v", err)
	}
	legacy := db.SSHKey{Name: "legacy", PublicKey: pub, Source: "imported"}
	if err := database.Create(&legacy).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}

	n, err := svc.BackfillOwnership(context.Background())
	if err != nil {
		t.Fatalf("BackfillOwnership: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected without admin: got %d, want 0", n)
	}
}

// TestServiceOwnerGate covers the cross-user 404 on every gated method. The
// nil-requesterID bypass is already exercised by the rest of this file (every
// other test passes nil), so we focus on the non-nil-mismatch branch here.
func TestServiceOwnerGate(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	pub, priv, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	ownerA := uint(7)
	row, err := svc.Create(context.Background(), sshkeys.CreateRequest{
		Name:      "owned-by-a",
		PublicKey: pub,
		OwnerID:   &ownerA,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	otherID := uint(99)
	expectNotFound := func(t *testing.T, name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: expected NotFound, got nil", name)
			return
		}
		var nf *internalerrors.NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("%s: expected NotFoundError, got %T (%v)", name, err, err)
		}
	}

	_, err = svc.Get(context.Background(), row.ID, &otherID)
	expectNotFound(t, "Get", err)
	_, _, err = svc.GetPrivateKey(context.Background(), row.ID, &otherID)
	expectNotFound(t, "GetPrivateKey", err)
	err = svc.SetDefault(context.Background(), row.ID, &otherID)
	expectNotFound(t, "SetDefault", err)
	err = svc.AttachPrivateKey(context.Background(), row.ID, priv, &otherID)
	expectNotFound(t, "AttachPrivateKey", err)
	err = svc.Delete(context.Background(), row.ID, &otherID)
	expectNotFound(t, "Delete", err)
}
