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
	name, plain, err := svc.GetPrivateKey(context.Background(), row.ID)
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

	_, _, err = svc.GetPrivateKey(context.Background(), row.ID)
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

	for _, name := range []string{"", "Name With Spaces", "UPPER", "with_underscore", "trailing-"} {
		_, err := svc.Create(context.Background(), sshkeys.CreateRequest{Name: name, Generate: true})
		var ve *internalerrors.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("name %q: expected ValidationError, got %v", name, err)
		}
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

	if err := svc.SetDefault(context.Background(), b.ID); err != nil {
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

	if err := svc.Delete(context.Background(), key.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var loaded db.VM
	if err := database.First(&loaded, vm.ID).Error; err != nil {
		t.Fatal(err)
	}
	if loaded.SSHKeyID != nil {
		t.Errorf("VM.SSHKeyID = %v, want nil after key delete", *loaded.SSHKeyID)
	}

	_, err := svc.Get(context.Background(), key.ID)
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
