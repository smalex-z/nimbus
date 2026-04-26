package sshkeys_test

import (
	"context"
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/secrets"
	"nimbus/internal/sshkeys"
)

// Seed a VM the way the legacy code did — KeyName + SSHPrivKeyCT/Nonce on the
// row, no SSHKeyID — and confirm MigrateLegacyVMKeys promotes it into an
// SSHKey row, links the VM, and zeros out the legacy columns.
func TestMigrateLegacyVMKeys_PromotesAndLinks(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.SSHKey{}, &db.VM{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := sshkeys.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := cipher.Encrypt([]byte(priv))
	if err != nil {
		t.Fatal(err)
	}

	legacy := &db.VM{
		VMID: 100, Hostname: "legacy", IP: "10.0.0.1", Node: "alpha",
		Tier: "small", OSTemplate: "ubuntu-24.04", Status: "running",
		KeyName:         "nimbus-legacy",
		SSHPubKey:       pub,
		SSHPrivKeyCT:    ct,
		SSHPrivKeyNonce: nonce,
	}
	if err := database.Create(legacy).Error; err != nil {
		t.Fatalf("seed legacy vm: %v", err)
	}

	n, err := sshkeys.MigrateLegacyVMKeys(context.Background(), database.DB)
	if err != nil {
		t.Fatalf("MigrateLegacyVMKeys: %v", err)
	}
	if n != 1 {
		t.Errorf("migrated = %d, want 1", n)
	}

	// Idempotent: a second run should be a no-op.
	again, err := sshkeys.MigrateLegacyVMKeys(context.Background(), database.DB)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second run migrated = %d, want 0", again)
	}

	// VM is now linked to the new ssh_keys row, with legacy columns zeroed.
	var loaded db.VM
	if err := database.First(&loaded, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if loaded.SSHKeyID == nil {
		t.Fatal("VM.SSHKeyID still NULL after migration")
	}
	if len(loaded.SSHPrivKeyCT) != 0 || len(loaded.SSHPrivKeyNonce) != 0 {
		t.Error("legacy columns should be cleared after migration")
	}

	// The new ssh_keys row preserves the encrypted blob and is tagged vm-auto.
	var key db.SSHKey
	if err := database.First(&key, *loaded.SSHKeyID).Error; err != nil {
		t.Fatal(err)
	}
	if key.Source != sshkeys.SourceVMAuto {
		t.Errorf("Source = %q, want vm-auto", key.Source)
	}
	if string(key.PrivKeyCT) != string(ct) {
		t.Error("ciphertext was modified during migration")
	}

	// And it's decryptable through the keys service — round-trip back to plaintext.
	svc := sshkeys.New(database.DB, cipher)
	_, plain, err := svc.GetPrivateKey(context.Background(), key.ID, nil)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if plain != priv {
		t.Error("decrypted private key doesn't match original")
	}
}
