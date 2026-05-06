// Package s3storage manages the cluster's shared MinIO host.
//
// The shared MinIO lives on a dedicated VM that Nimbus provisions on demand
// (the "Deploy Storage" action on the S3 page). Only one such VM exists at a
// time — the s3_storage table is treated as a singleton.
//
// This package owns the lifecycle (deploy, status, delete) and the
// in-process MinIO client used to mediate bucket CRUD on behalf of the UI.
package s3storage

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"gorm.io/gorm"

	"nimbus/internal/db"
)

// Status values written to S3Storage.Status.
const (
	StatusDeploying = "deploying"
	StatusReady     = "ready"
	StatusError     = "error"
	StatusDeleting  = "deleting"
)

// ErrNotDeployed is returned by Get when no storage row exists.
var ErrNotDeployed = errors.New("s3 storage not deployed")

// ErrAlreadyDeployed is returned by Create when a storage row already exists.
var ErrAlreadyDeployed = errors.New("s3 storage already deployed")

// ErrNotReady is returned by Buckets when the storage VM is not in the
// "ready" state — the MinIO client is only safe to use after deploy
// finishes.
var ErrNotReady = errors.New("s3 storage not ready")

// Service manages the singleton S3 storage row and the MinIO client that
// talks to it.
type Service struct {
	db *gorm.DB

	mu      sync.Mutex
	buckets *BucketsClient // cached; rebuilt when the underlying row changes
}

// New constructs a Service.
func New(database *gorm.DB) *Service {
	return &Service{db: database}
}

// Get returns the current storage row, or ErrNotDeployed if none exists.
func (s *Service) Get() (*db.S3Storage, error) {
	var row db.S3Storage
	if err := s.db.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotDeployed
		}
		return nil, fmt.Errorf("load s3 storage: %w", err)
	}
	return &row, nil
}

// CreateParams captures the inputs to a deploy. The deploy orchestrator
// (deploy.go) is responsible for picking the VM via the provision flow
// before calling Create — this method only owns the database row.
type CreateParams struct {
	VMID         int
	Node         string
	DiskGB       int
	RootUser     string
	RootPassword string
}

// Create inserts a new storage row in the "deploying" state. Returns
// ErrAlreadyDeployed if a row already exists.
func (s *Service) Create(p CreateParams) (*db.S3Storage, error) {
	if _, err := s.Get(); err == nil {
		return nil, ErrAlreadyDeployed
	} else if !errors.Is(err, ErrNotDeployed) {
		return nil, err
	}

	row := db.S3Storage{
		VMID:         p.VMID,
		Node:         p.Node,
		DiskGB:       p.DiskGB,
		Status:       StatusDeploying,
		RootUser:     p.RootUser,
		RootPassword: p.RootPassword,
	}
	if err := s.db.Create(&row).Error; err != nil {
		return nil, fmt.Errorf("insert s3 storage: %w", err)
	}
	return &row, nil
}

// MarkReady transitions the row to "ready" and persists the resolved
// public endpoint (e.g. "http://10.0.0.42:9000"). Clears any prior
// error message and invalidates the cached MinIO client so the next
// Buckets() call rebuilds it against the new endpoint.
func (s *Service) MarkReady(endpoint string) error {
	row, err := s.Get()
	if err != nil {
		return err
	}
	updates := map[string]any{
		"status":    StatusReady,
		"endpoint":  endpoint,
		"error_msg": "",
	}
	if err := s.db.Model(&db.S3Storage{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	s.invalidateBuckets()
	return nil
}

// MarkError flips the row to "error" with the given message. The row is
// kept so the user can hit Delete to retry.
func (s *Service) MarkError(msg string) error {
	row, err := s.Get()
	if err != nil {
		return err
	}
	if err := s.db.Model(&db.S3Storage{}).Where("id = ?", row.ID).Updates(map[string]any{
		"status":    StatusError,
		"error_msg": msg,
	}).Error; err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	s.invalidateBuckets()
	return nil
}

// MarkDeleting flips the row to "deleting" so the UI can show progress
// while the underlying VM is destroyed. Idempotent — safe to call when
// the row is already deleting.
func (s *Service) MarkDeleting() error {
	row, err := s.Get()
	if err != nil {
		return err
	}
	if err := s.db.Model(&db.S3Storage{}).Where("id = ?", row.ID).
		Update("status", StatusDeleting).Error; err != nil {
		return fmt.Errorf("mark deleting: %w", err)
	}
	s.invalidateBuckets()
	return nil
}

// Delete removes the storage row and cascades to user-side bucket and
// service-account rows in the same SQLite transaction. The caller is
// responsible for tearing down the underlying Proxmox VM (and releasing its
// IP) before or after calling this — the row is just the database
// bookkeeping.
//
// The cascade is hard-delete (Unscoped) so a redeploy starts from a clean
// slate. Bucket data on the destroyed VM's disk is gone with the VM, so
// retaining the rows would lie to the UI.
//
// Also cascades the auto-generated SSH key (s3_storage.ssh_key_id) so
// failed/successful redeploys don't leak nimbus-nimbus-s3-* entries
// into the user's vault. Best-effort — a missing key row doesn't fail
// the cascade.
func (s *Service) Delete() error {
	// Capture the SSH key id before we delete the storage row inside the
	// transaction; the row body is gone after the txn commits.
	var sshKeyID *uint
	if row, err := s.Get(); err == nil {
		sshKeyID = row.SSHKeyID
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("1 = 1").Delete(&db.S3Bucket{}).Error; err != nil {
			return fmt.Errorf("cascade delete s3 buckets: %w", err)
		}
		if err := tx.Unscoped().Where("1 = 1").Delete(&db.S3ServiceAccount{}).Error; err != nil {
			return fmt.Errorf("cascade delete s3 service accounts: %w", err)
		}
		if err := tx.Unscoped().Where("1 = 1").Delete(&db.S3Storage{}).Error; err != nil {
			return fmt.Errorf("delete s3 storage: %w", err)
		}
		if sshKeyID != nil {
			if err := tx.Unscoped().Delete(&db.SSHKey{}, *sshKeyID).Error; err != nil {
				log.Printf("s3 delete: cascade ssh key %d: %v", *sshKeyID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidateBuckets()
	return nil
}

// Buckets returns a MinIO bucket-API client bound to the current storage
// row. Returns ErrNotDeployed when no row exists, ErrNotReady when the
// row is not yet "ready" (e.g. still deploying or in an error state).
//
// The client is cached and reused across calls; invalidated on any state
// change (MarkReady, MarkError, MarkDeleting, Delete).
func (s *Service) Buckets() (*BucketsClient, error) {
	row, err := s.Get()
	if err != nil {
		return nil, err
	}
	if row.Status != StatusReady {
		return nil, ErrNotReady
	}
	if row.Endpoint == "" {
		return nil, ErrNotReady
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets != nil {
		return s.buckets, nil
	}
	bc, err := newBucketsClient(row.Endpoint, row.RootUser, row.RootPassword)
	if err != nil {
		return nil, fmt.Errorf("init minio client: %w", err)
	}
	s.buckets = bc
	return s.buckets, nil
}

func (s *Service) invalidateBuckets() {
	s.mu.Lock()
	s.buckets = nil
	s.mu.Unlock()
}
