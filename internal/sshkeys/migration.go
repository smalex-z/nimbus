package sshkeys

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"nimbus/internal/db"
)

// MigrateLegacyVMKeys promotes pre-existing per-VM vault entries (the old
// ssh_privkey_ct/nonce columns on the vms table) into rows in the ssh_keys
// table. Idempotent: VMs that already have ssh_key_id set, or that lack a
// vaulted key, are skipped.
//
// Returns the number of rows migrated.
//
// Migration strategy: for each VM with ciphertext set and ssh_key_id NULL,
// create an SSHKey row tagged Source=vm-auto, transfer the encrypted blob
// directly (no decrypt/re-encrypt — the master key hasn't changed), set
// vm.ssh_key_id, and zero the legacy columns.
func MigrateLegacyVMKeys(ctx context.Context, database *gorm.DB) (int, error) {
	var legacy []db.VM
	err := database.WithContext(ctx).
		Where("ssh_key_id IS NULL AND key_name != '' AND length(ssh_privkey_ct) > 0").
		Find(&legacy).Error
	if err != nil {
		return 0, fmt.Errorf("scan legacy vault entries: %w", err)
	}
	if len(legacy) == 0 {
		return 0, nil
	}

	migrated := 0
	for i := range legacy {
		vm := &legacy[i]
		err := database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// If a key with this name already exists (e.g. partial prior run),
			// reuse it rather than creating a duplicate.
			var existing db.SSHKey
			err := tx.Where("name = ?", vm.KeyName).First(&existing).Error
			var keyID uint
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				row := &db.SSHKey{
					Name:         vm.KeyName,
					PublicKey:    vm.SSHPubKey,
					PrivKeyCT:    vm.SSHPrivKeyCT,
					PrivKeyNonce: vm.SSHPrivKeyNonce,
					OwnerID:      vm.OwnerID,
					Source:       SourceVMAuto,
				}
				if vm.SSHPubKey != "" {
					if fp, err := fingerprint(vm.SSHPubKey); err == nil {
						row.Fingerprint = fp
					}
				}
				if err := tx.Create(row).Error; err != nil {
					return fmt.Errorf("create ssh_keys row for vm %d: %w", vm.ID, err)
				}
				keyID = row.ID
			case err != nil:
				return fmt.Errorf("lookup existing key %q: %w", vm.KeyName, err)
			default:
				keyID = existing.ID
			}

			// Link the VM and zero the legacy columns. Use Updates with a map
			// so GORM emits the NULL/zero-out we want regardless of zero-value
			// elision rules on the struct path.
			if err := tx.Model(&db.VM{}).Where("id = ?", vm.ID).Updates(map[string]any{
				"ssh_key_id":        keyID,
				"ssh_privkey_ct":    nil,
				"ssh_privkey_nonce": nil,
			}).Error; err != nil {
				return fmt.Errorf("link vm %d to ssh_keys: %w", vm.ID, err)
			}
			migrated++
			return nil
		})
		if err != nil {
			return migrated, err
		}
	}
	return migrated, nil
}
