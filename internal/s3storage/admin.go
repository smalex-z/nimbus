package s3storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/db"
)

// AdminBucketStat extends BucketStat with owner identity. Returned from
// Service.ListAllBuckets so the admin /s3 page can render a who-owns-what
// table across the whole cluster.
//
// Owner fields are derived from a LEFT JOIN against users; if a row is
// orphaned (cascade hiccup, concurrent user-delete), OwnerName/OwnerEmail
// come back as empty strings — the UI renders these as "<unknown>".
type AdminBucketStat struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	ObjectCount int64     `json:"object_count"`
	TotalSize   int64     `json:"total_size_bytes"`
	OwnerID     uint      `json:"owner_id"`
	OwnerName   string    `json:"owner_name"`
	OwnerEmail  string    `json:"owner_email"`
}

// ListAllBuckets returns every active bucket joined with owner identity.
// Object counts + sizes are pulled live via the same bucketStats walker
// the user-side list uses (10k object cap; truncated buckets show their
// partial total).
//
// Returns ErrNotDeployed when no storage row exists, ErrNotReady while
// the storage VM is still coming up.
func (s *Service) ListAllBuckets(ctx context.Context) ([]AdminBucketStat, error) {
	bc, err := s.Buckets()
	if err != nil {
		return nil, err
	}
	type row struct {
		Name       string
		OwnerID    uint
		OwnerName  string
		OwnerEmail string
		CreatedAt  time.Time
	}
	var rows []row
	if err := s.db.Table("s3_buckets").
		Select(`s3_buckets.name        AS name,
		        s3_buckets.owner_id    AS owner_id,
		        users.name             AS owner_name,
		        users.email            AS owner_email,
		        s3_buckets.created_at  AS created_at`).
		Joins("LEFT JOIN users ON users.id = s3_buckets.owner_id AND users.deleted_at IS NULL").
		Where("s3_buckets.deleted_at IS NULL").
		Order("users.name ASC, s3_buckets.created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list bucket rows: %w", err)
	}
	out := make([]AdminBucketStat, 0, len(rows))
	for _, r := range rows {
		count, size := bc.bucketStats(ctx, r.Name)
		out = append(out, AdminBucketStat{
			Name:        r.Name,
			CreatedAt:   r.CreatedAt,
			ObjectCount: count,
			TotalSize:   size,
			OwnerID:     r.OwnerID,
			OwnerName:   r.OwnerName,
			OwnerEmail:  r.OwnerEmail,
		})
	}
	return out, nil
}

// AdminDeleteBucket force-empties a bucket on MinIO then removes it,
// regardless of who owns it. Cascades to the corresponding s3_buckets
// DB row so the owner stops seeing it in their list. Idempotent on the
// row deletion (Unscoped hard-delete).
//
// Returns gorm.ErrRecordNotFound when no DB row matches the name —
// admins shouldn't have a way to nuke buckets that aren't tracked here
// (that'd be a sign of MinIO/DB drift; surface as 404 instead of
// silently force-deleting).
func (s *Service) AdminDeleteBucket(ctx context.Context, name string) error {
	var row db.S3Bucket
	if err := s.db.Where("name = ?", name).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return fmt.Errorf("load bucket row: %w", err)
	}
	bc, err := s.Buckets()
	if err != nil {
		return err
	}
	if err := bc.ForceDeleteBucket(ctx, name); err != nil {
		return err
	}
	if err := s.db.Unscoped().Delete(&row).Error; err != nil {
		return fmt.Errorf("delete bucket row: %w", err)
	}
	return nil
}
