package s3storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"

	"gorm.io/gorm"

	"nimbus/internal/db"
	"nimbus/internal/secrets"
)

// userPartRE constrains the user-typed half of a bucket name. Stricter than
// the bucket-name regex on the admin path because the prefix half eats some
// of MinIO's 63-char budget; capping userPart at 30 chars keeps the composed
// `<prefix>-<userPart>` comfortably below 63 even for 21-char prefixes.
//
// Composed-name length is also enforced at runtime in Create.
var userPartMin = 3
var userPartMax = 30

// ErrBucketNameInvalid is returned by Create when the user-provided name
// half fails validation. The handler returns 400.
var ErrBucketNameInvalid = errors.New("bucket name invalid: 3-30 chars, lowercase letters/digits/hyphens, no leading or trailing hyphen")

// ErrBucketNotOwned is returned by Delete when the bucket exists in MinIO
// but is not registered to the calling user. The handler returns 404 — we
// do not disclose existence across ownership boundaries.
var ErrBucketNotOwned = errors.New("bucket not owned by caller")

// UserBucketService is the per-user surface on top of the singleton MinIO
// host. Owns the s3_buckets and s3_service_accounts tables; delegates
// MinIO-side state (buckets, service accounts) to BucketsClient + AdminClient
// constructed from the storage row's root credentials.
type UserBucketService struct {
	db      *gorm.DB
	cipher  *secrets.Cipher
	storage *Service
}

// NewUserBucketService constructs the service. cipher must be the same
// AES-256-GCM cipher used elsewhere (e.g. SSH private keys) so a key
// rotation rotates this and that together.
func NewUserBucketService(database *gorm.DB, cipher *secrets.Cipher, storage *Service) *UserBucketService {
	return &UserBucketService{db: database, cipher: cipher, storage: storage}
}

// EnsureServiceAccount returns the calling user's MinIO service account,
// minting one on first call. Returns the plaintext secret so callers
// (Credentials handler, on-demand display in the UI) don't need to round-
// trip back to the cipher themselves.
//
// Idempotent and race-safe: SQLite's single-writer + the uniqueIndex on
// owner_id serializes concurrent first-creates; the loser re-SELECTs the
// winner's row.
func (u *UserBucketService) EnsureServiceAccount(ctx context.Context, userID uint, userName string) (*db.S3ServiceAccount, string, error) {
	if userID == 0 {
		return nil, "", errors.New("userID is zero")
	}

	// Hot path: row already exists.
	if row, plain, err := u.loadServiceAccount(userID); err == nil {
		return row, plain, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", err
	}

	// Cold path: mint. We need the storage VM ready to talk to MinIO.
	storageRow, err := u.storage.Get()
	if err != nil {
		return nil, "", err
	}
	if storageRow.Status != StatusReady || storageRow.Endpoint == "" {
		return nil, "", ErrNotReady
	}

	prefix := computePrefix(userName, userID)
	accessKey := fmt.Sprintf("nimbus_u%d", userID)
	// MinIO caps service-account secret keys at 40 chars (root passwords are
	// uncapped, but per-SA creds via the admin API enforce 8–40). 20 bytes →
	// 40 hex chars hits the max while staying inside the bound.
	secretBytes, err := randomHexBytes(20)
	if err != nil {
		return nil, "", fmt.Errorf("generate secret: %w", err)
	}
	plaintextSecret := string(secretBytes)

	admin, err := newAdminClient(storageRow.Endpoint, storageRow.RootUser, storageRow.RootPassword)
	if err != nil {
		return nil, "", err
	}
	policy := BuildPolicyJSON(prefix)
	if err := admin.MintServiceAccount(ctx, accessKey, plaintextSecret, policy); err != nil {
		return nil, "", err
	}

	ct, nonce, err := u.cipher.Encrypt([]byte(plaintextSecret))
	if err != nil {
		return nil, "", fmt.Errorf("encrypt secret: %w", err)
	}

	row := &db.S3ServiceAccount{
		OwnerID:     userID,
		Prefix:      prefix,
		AccessKey:   accessKey,
		SecretCT:    ct,
		SecretNonce: nonce,
	}
	if err := u.db.Create(row).Error; err != nil {
		if isUniqueViolation(err) {
			// Lost the race to another goroutine; reload the winner's row.
			// The orphaned MinIO secret we minted above is overwritten by
			// MintServiceAccount's idempotent Update path on next mint.
			return u.loadServiceAccount(userID)
		}
		return nil, "", fmt.Errorf("insert service account row: %w", err)
	}
	return row, plaintextSecret, nil
}

// Create composes `<prefix>-<userPart>`, validates it, asks MinIO to create
// the bucket via root credentials, and inserts the s3_buckets row.
//
// Bucket creation goes through root, not the user's service account — the
// SA's policy deliberately omits s3:CreateBucket so the prefix invariant
// is enforced server-side, not by hoping the SA's policy is correct.
func (u *UserBucketService) Create(ctx context.Context, userID uint, userName, userPart string) (*db.S3Bucket, error) {
	if !validUserPart(userPart) {
		return nil, ErrBucketNameInvalid
	}
	sa, _, err := u.EnsureServiceAccount(ctx, userID, userName)
	if err != nil {
		return nil, err
	}

	name := sa.Prefix + "-" + userPart
	if !validComposedName(name) {
		// Belt-and-suspenders: prefix + userPart shouldn't exceed 63 chars
		// since prefix ≤ 21 and userPart ≤ 30 with a 1-char hyphen, but the
		// regex check below MinIO's wire format is the safety net.
		return nil, ErrBucketNameInvalid
	}

	bc, err := u.storage.Buckets()
	if err != nil {
		return nil, err
	}
	if err := bc.CreateBucket(ctx, name); err != nil {
		return nil, err
	}

	row := &db.S3Bucket{OwnerID: userID, Name: name}
	if err := u.db.Create(row).Error; err != nil {
		// MinIO already has the bucket; the row insert lost. Most likely a
		// stale row exists from a prior aborted teardown — surface as
		// already-exists so the user can pick a different name.
		if isUniqueViolation(err) {
			return nil, ErrBucketAlreadyExists
		}
		// MinIO succeeded but DB failed. Best-effort rollback of the bucket
		// so we don't leave an orphan the user can't manage.
		if delErr := bc.DeleteBucket(ctx, name); delErr != nil {
			log.Printf("s3: rollback failed for orphan bucket %s: %v", name, delErr)
		}
		return nil, fmt.Errorf("insert bucket row: %w", err)
	}
	return row, nil
}

// List returns the calling user's buckets with object-count + total-size
// stats. Stats are pulled via the existing bucketStats walker — same cap
// (~10k objects) and same swallow-errors behavior.
func (u *UserBucketService) List(ctx context.Context, userID uint) ([]BucketStat, error) {
	bc, err := u.storage.Buckets()
	if err != nil {
		return nil, err
	}
	var rows []db.S3Bucket
	if err := u.db.Where("owner_id = ?", userID).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list bucket rows: %w", err)
	}
	out := make([]BucketStat, 0, len(rows))
	for _, b := range rows {
		stat := BucketStat{Name: b.Name, CreatedAt: b.CreatedAt}
		count, size := bc.bucketStats(ctx, b.Name)
		stat.ObjectCount = count
		stat.TotalSize = size
		out = append(out, stat)
	}
	return out, nil
}

// Delete removes the named bucket if it belongs to the calling user. Returns
// ErrBucketNotOwned (handler 404) if the user doesn't own a row with this
// name, ErrBucketNotEmpty (handler 409) if MinIO refuses because the bucket
// still has objects.
func (u *UserBucketService) Delete(ctx context.Context, userID uint, name string) error {
	var row db.S3Bucket
	if err := u.db.Where("owner_id = ? AND name = ?", userID, name).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBucketNotOwned
		}
		return fmt.Errorf("load bucket row: %w", err)
	}
	bc, err := u.storage.Buckets()
	if err != nil {
		return err
	}
	if err := bc.DeleteBucket(ctx, name); err != nil {
		return err
	}
	if err := u.db.Unscoped().Delete(&row).Error; err != nil {
		return fmt.Errorf("delete bucket row: %w", err)
	}
	return nil
}

// CredentialsView is the shape the Credentials handler returns to the SPA.
type CredentialsView struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Prefix    string
}

// Credentials returns everything a webapp needs to authenticate against the
// user's scoped MinIO surface. Auto-mints the service account on first
// call so the UI can show creds before any bucket exists.
func (u *UserBucketService) Credentials(ctx context.Context, userID uint, userName string) (*CredentialsView, error) {
	storageRow, err := u.storage.Get()
	if err != nil {
		return nil, err
	}
	if storageRow.Status != StatusReady || storageRow.Endpoint == "" {
		return nil, ErrNotReady
	}
	sa, plain, err := u.EnsureServiceAccount(ctx, userID, userName)
	if err != nil {
		return nil, err
	}
	return &CredentialsView{
		Endpoint:  storageRow.Endpoint,
		AccessKey: sa.AccessKey,
		SecretKey: plain,
		Prefix:    sa.Prefix,
	}, nil
}

// PurgeForUser tears down a user's MinIO state when their Nimbus account is
// deleted. Best-effort: MinIO errors are logged but don't block the cleanup
// of subsequent resources, so a hung MinIO doesn't wedge user deletion.
func (u *UserBucketService) PurgeForUser(ctx context.Context, userID uint) error {
	storageRow, err := u.storage.Get()
	if errors.Is(err, ErrNotDeployed) {
		// No MinIO host at all; just clear our DB rows.
		return u.purgeRows(userID)
	}
	if err != nil {
		return err
	}

	// Best-effort MinIO-side cleanup. We try buckets first, then SA, so a
	// dead bucket leaves the SA reachable for manual recovery.
	if storageRow.Status == StatusReady && storageRow.Endpoint != "" {
		if bc, bcErr := u.storage.Buckets(); bcErr == nil {
			var rows []db.S3Bucket
			if err := u.db.Where("owner_id = ?", userID).Find(&rows).Error; err != nil {
				log.Printf("s3 purge: load bucket rows for user %d: %v", userID, err)
			}
			for _, b := range rows {
				if err := bc.DeleteBucket(ctx, b.Name); err != nil {
					log.Printf("s3 purge: remove bucket %s: %v", b.Name, err)
				}
			}
		}
		var sa db.S3ServiceAccount
		if err := u.db.Where("owner_id = ?", userID).First(&sa).Error; err == nil {
			if admin, adminErr := newAdminClient(storageRow.Endpoint, storageRow.RootUser, storageRow.RootPassword); adminErr == nil {
				if err := admin.DeleteServiceAccount(ctx, sa.AccessKey); err != nil {
					log.Printf("s3 purge: delete SA %s: %v", sa.AccessKey, err)
				}
			}
		}
	}

	return u.purgeRows(userID)
}

func (u *UserBucketService) purgeRows(userID uint) error {
	return u.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("owner_id = ?", userID).Delete(&db.S3Bucket{}).Error; err != nil {
			return fmt.Errorf("purge bucket rows: %w", err)
		}
		if err := tx.Unscoped().Where("owner_id = ?", userID).Delete(&db.S3ServiceAccount{}).Error; err != nil {
			return fmt.Errorf("purge service account row: %w", err)
		}
		return nil
	})
}

func (u *UserBucketService) loadServiceAccount(userID uint) (*db.S3ServiceAccount, string, error) {
	var row db.S3ServiceAccount
	if err := u.db.Where("owner_id = ?", userID).First(&row).Error; err != nil {
		return nil, "", err
	}
	plain, err := u.cipher.Decrypt(row.SecretCT, row.SecretNonce)
	if err != nil {
		return nil, "", fmt.Errorf("decrypt service account secret: %w", err)
	}
	return &row, string(plain), nil
}

// computePrefix derives a stable, bucket-safe prefix from a user's display
// name and ID. The `-u<id>` suffix guarantees uniqueness across users with
// the same display name (or empty / non-ASCII names that sanitize to "").
//
// Caps the alphanumeric body at 12 chars so the full prefix stays under
// ~21 chars, leaving room for a reasonable userPart inside MinIO's 63-char
// bucket-name limit.
func computePrefix(userName string, userID uint) string {
	body := sanitizeNameBody(userName, 12)
	if body == "" {
		return fmt.Sprintf("u%d", userID)
	}
	return fmt.Sprintf("%s-u%d", body, userID)
}

// sanitizeNameBody lowercases ASCII letters/digits, drops everything else,
// and uses hyphens as separators when they appear in the original. Returns
// at most maxLen chars from the result.
func sanitizeNameBody(s string, maxLen int) string {
	var b strings.Builder
	prevHyphen := true // drop leading hyphens
	for _, r := range s {
		// Multi-byte runes deliberately drop out (CJK, emoji, etc.) — the
		// `-u<id>` suffix carries the uniqueness guarantee.
		if r >= 128 {
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
			continue
		}
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
			prevHyphen = false
		case unicode.IsDigit(r):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
		if b.Len() >= maxLen {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

func validUserPart(s string) bool {
	if len(s) < userPartMin || len(s) > userPartMax {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// validComposedName re-runs MinIO's bucket-name rules on the full composed
// name — defensive check so we never call MakeBucket with something that'd
// be rejected at the wire.
func validComposedName(s string) bool {
	if len(s) < 3 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

func randomHexBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	out := make([]byte, hex.EncodedLen(n))
	hex.Encode(out, buf)
	return out, nil
}

// isUniqueViolation matches the same heuristic sshkeys.isUniqueViolation
// uses — gorm.ErrDuplicatedKey is unreliable across drivers, so we fall
// back to substring match on the message.
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
