// Package sshkeys is the user-managed SSH key store.
//
// Keys are first-class objects: users add them on the Keys page, optionally
// mark one as the default for new VMs, and download the private half later
// from anywhere. Provisioning consumes keys from this store via key ID; the
// legacy per-VM vault columns are migrated into this store on startup and
// then ignored.
package sshkeys

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"

	"nimbus/internal/db"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/secrets"
)

// nameRE constrains key names to a filesystem-safe identifier so they're safe
// to use as file names ("ssh -i ~/.ssh/{name}") and as part of URLs. We allow
// '_' and '.' in the middle so common SSH key filenames (id_rsa, id_ed25519,
// id_rsa.pub, my.laptop) round-trip without renaming.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// Source values for SSHKey.Source.
const (
	SourceImported  = "imported"
	SourceGenerated = "generated"
	SourceVMAuto    = "vm-auto"
)

// Service manages the SSH key store.
type Service struct {
	db     *gorm.DB
	cipher *secrets.Cipher
}

// New constructs a Service.
func New(database *gorm.DB, cipher *secrets.Cipher) *Service {
	return &Service{db: database, cipher: cipher}
}

// CreateRequest captures everything the API/handler can pass through.
//
// Exactly one of (Generate=true) or (PublicKey set) must hold. PrivateKey is
// optional and only meaningful with PublicKey; when Generate is true the
// service mints both halves.
type CreateRequest struct {
	Name       string
	Label      string
	PublicKey  string
	PrivateKey string
	Generate   bool
	SetDefault bool
	OwnerID    *uint
}

// Create stores a new key.
//
// On generate: mints a fresh Ed25519 keypair and stores the encrypted private
// half. On import: stores the public key, plus the private half (encrypted)
// when the caller supplied one and it matches the public key.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*db.SSHKey, error) {
	if !nameRE.MatchString(req.Name) {
		return nil, &internalerrors.ValidationError{
			Field:   "name",
			Message: "lowercase letters, digits, '.', '_', or '-'; must start and end with a letter or digit; 1-64 chars",
		}
	}
	if req.Generate && req.PublicKey != "" {
		return nil, &internalerrors.ValidationError{
			Field:   "key",
			Message: "exactly one of generate or public_key must be provided",
		}
	}
	if !req.Generate && req.PublicKey == "" {
		return nil, &internalerrors.ValidationError{
			Field:   "key",
			Message: "exactly one of generate or public_key must be provided",
		}
	}

	pub := strings.TrimSpace(req.PublicKey)
	priv := req.PrivateKey
	source := SourceImported

	if req.Generate {
		var err error
		pub, priv, err = GenerateEd25519()
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		source = SourceGenerated
	} else if priv != "" {
		if err := VerifyKeyPair(pub, priv); err != nil {
			return nil, &internalerrors.ValidationError{
				Field:   "private_key",
				Message: "private key does not match public key: " + err.Error(),
			}
		}
	}

	fp, err := fingerprint(pub)
	if err != nil {
		return nil, &internalerrors.ValidationError{
			Field:   "public_key",
			Message: "could not parse public key: " + err.Error(),
		}
	}

	row := &db.SSHKey{
		Name:        req.Name,
		Label:       req.Label,
		PublicKey:   pub,
		Fingerprint: fp,
		OwnerID:     req.OwnerID,
		Source:      source,
	}
	if priv != "" {
		ct, nonce, err := s.cipher.Encrypt([]byte(priv))
		if err != nil {
			return nil, fmt.Errorf("encrypt private key: %w", err)
		}
		row.PrivKeyCT = ct
		row.PrivKeyNonce = nonce
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(row).Error; err != nil {
			if isUniqueViolation(err) {
				return &internalerrors.ConflictError{Message: fmt.Sprintf("key name %q already exists", req.Name)}
			}
			return fmt.Errorf("create ssh key: %w", err)
		}
		if req.SetDefault {
			if err := setDefaultTx(tx, row.ID, req.OwnerID); err != nil {
				return err
			}
			row.IsDefault = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

// List returns all stored keys for the given owner. ownerID==nil returns
// every key (Phase 1 default — no auth).
func (s *Service) List(ctx context.Context, ownerID *uint) ([]db.SSHKey, error) {
	var keys []db.SSHKey
	q := s.db.WithContext(ctx).Order("is_default DESC, created_at DESC")
	if ownerID != nil {
		q = q.Where("owner_id = ?", *ownerID)
	}
	if err := q.Find(&keys).Error; err != nil {
		return nil, fmt.Errorf("list ssh keys: %w", err)
	}
	return keys, nil
}

// Get returns one key by ID.
func (s *Service) Get(ctx context.Context, id uint) (*db.SSHKey, error) {
	var row db.SSHKey
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "ssh_key", ID: fmt.Sprintf("%d", id)}
		}
		return nil, fmt.Errorf("get ssh key %d: %w", id, err)
	}
	return &row, nil
}

// GetPrivateKey returns the decrypted private half of a key.
func (s *Service) GetPrivateKey(ctx context.Context, id uint) (name, privateKey string, err error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	if !row.HasPrivateKey() {
		return "", "", &internalerrors.NotFoundError{
			Resource: "private_key",
			ID:       fmt.Sprintf("ssh_key:%d", id),
		}
	}
	pt, err := s.cipher.Decrypt(row.PrivKeyCT, row.PrivKeyNonce)
	if err != nil {
		return "", "", fmt.Errorf("decrypt ssh key %d: %w", id, err)
	}
	return row.Name, string(pt), nil
}

// Delete removes the key. The associated VM rows have their ssh_key_id set to
// NULL via a manual update (we don't rely on GORM's cascade since SQLite +
// soft-deletes complicate things).
func (s *Service) Delete(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row db.SSHKey
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &internalerrors.NotFoundError{Resource: "ssh_key", ID: fmt.Sprintf("%d", id)}
			}
			return fmt.Errorf("get ssh key %d: %w", id, err)
		}
		if err := tx.Model(&db.VM{}).Where("ssh_key_id = ?", id).Update("ssh_key_id", nil).Error; err != nil {
			return fmt.Errorf("clear ssh_key_id refs: %w", err)
		}
		if err := tx.Delete(&row).Error; err != nil {
			return fmt.Errorf("delete ssh key %d: %w", id, err)
		}
		return nil
	})
}

// AttachPrivateKey vaults a private half on a public-only key. Rejects when a
// private half already exists or the keypair doesn't match — the user must
// delete and re-add to replace.
func (s *Service) AttachPrivateKey(ctx context.Context, id uint, privateKey string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row db.SSHKey
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &internalerrors.NotFoundError{Resource: "ssh_key", ID: fmt.Sprintf("%d", id)}
			}
			return fmt.Errorf("get ssh key %d: %w", id, err)
		}
		if row.HasPrivateKey() {
			return &internalerrors.ConflictError{
				Message: "this key already has a private half stored — delete and re-add to replace",
			}
		}
		if err := VerifyKeyPair(row.PublicKey, privateKey); err != nil {
			return &internalerrors.ValidationError{
				Field:   "private_key",
				Message: "private key does not match the stored public key: " + err.Error(),
			}
		}
		ct, nonce, err := s.cipher.Encrypt([]byte(privateKey))
		if err != nil {
			return fmt.Errorf("encrypt private key: %w", err)
		}
		if err := tx.Model(&db.SSHKey{}).Where("id = ?", id).Updates(map[string]any{
			"priv_key_ct":    ct,
			"priv_key_nonce": nonce,
		}).Error; err != nil {
			return fmt.Errorf("update ssh key %d: %w", id, err)
		}
		return nil
	})
}

// SetDefault marks the named key as default for its owner, atomically clearing
// any prior default.
func (s *Service) SetDefault(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row db.SSHKey
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &internalerrors.NotFoundError{Resource: "ssh_key", ID: fmt.Sprintf("%d", id)}
			}
			return fmt.Errorf("get ssh key %d: %w", id, err)
		}
		return setDefaultTx(tx, id, row.OwnerID)
	})
}

// GetDefault returns the default key for the given owner, or NotFoundError
// when none is set.
func (s *Service) GetDefault(ctx context.Context, ownerID *uint) (*db.SSHKey, error) {
	var row db.SSHKey
	q := s.db.WithContext(ctx).Where("is_default = ?", true)
	if ownerID == nil {
		q = q.Where("owner_id IS NULL")
	} else {
		q = q.Where("owner_id = ?", *ownerID)
	}
	if err := q.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &internalerrors.NotFoundError{Resource: "default_ssh_key", ID: ""}
		}
		return nil, fmt.Errorf("get default ssh key: %w", err)
	}
	return &row, nil
}

// setDefaultTx is the in-transaction body shared by Create(SetDefault=true)
// and SetDefault. Clears any prior default within the same owner scope, then
// sets the requested key.
func setDefaultTx(tx *gorm.DB, id uint, ownerID *uint) error {
	clear := tx.Model(&db.SSHKey{}).Where("is_default = ? AND id != ?", true, id)
	if ownerID == nil {
		clear = clear.Where("owner_id IS NULL")
	} else {
		clear = clear.Where("owner_id = ?", *ownerID)
	}
	if err := clear.Update("is_default", false).Error; err != nil {
		return fmt.Errorf("clear prior default: %w", err)
	}
	if err := tx.Model(&db.SSHKey{}).Where("id = ?", id).Update("is_default", true).Error; err != nil {
		return fmt.Errorf("set default: %w", err)
	}
	return nil
}

// fingerprint returns the OpenSSH SHA256 fingerprint ("SHA256:base64...") of
// the supplied authorized_keys line.
func fingerprint(authorizedKeyLine string) (string, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKeyLine))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(pk.Marshal())
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "="), nil
}

// isUniqueViolation reports whether err looks like a unique-constraint failure
// from SQLite/Postgres. We pattern-match on the message text — gorm.ErrDuplicatedKey
// is recent and unreliable across drivers.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}
