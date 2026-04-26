package s3storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// BucketStat is the lightweight per-bucket summary the UI renders.
type BucketStat struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	ObjectCount int64     `json:"object_count"`
	TotalSize   int64     `json:"total_size_bytes"`
}

// BucketsClient is a thin wrapper around minio-go that exposes only the
// operations the Nimbus API needs. Constructed lazily by Service.Buckets.
//
// All methods accept a context and translate minio-go's typed errors into
// Go-side sentinels where the caller cares (e.g. ErrBucketNotEmpty).
type BucketsClient struct {
	mc *minio.Client
}

// ErrBucketNotEmpty is returned by DeleteBucket when MinIO refuses to
// delete a non-empty bucket. The handler converts this to a 409 Conflict.
var ErrBucketNotEmpty = errors.New("bucket is not empty")

// ErrBucketAlreadyExists mirrors MinIO's "BucketAlreadyOwnedByYou" /
// "BucketAlreadyExists" responses. Handler returns 409 Conflict.
var ErrBucketAlreadyExists = errors.New("bucket already exists")

// newBucketsClient builds a MinIO client from the storage row's endpoint
// and root credentials. The endpoint is expected to look like
// "http://10.0.0.42:9000" (matches what Service.MarkReady persists).
func newBucketsClient(endpoint, accessKey, secretKey string) (*BucketsClient, error) {
	host, secure, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("minio.New: %w", err)
	}
	return &BucketsClient{mc: mc}, nil
}

// splitEndpoint reduces "http://host:port" / "https://host:port" / "host:port"
// to the (host:port, secure) pair minio-go wants. Internal-network deploys
// run over plain HTTP per the design; HTTPS is left as a future-proof.
func splitEndpoint(endpoint string) (string, bool, error) {
	if endpoint == "" {
		return "", false, errors.New("empty endpoint")
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, false, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return host, u.Scheme == "https", nil
}

// ListBuckets returns one BucketStat per bucket. Stats (object count,
// total size) are computed by walking ListObjects with a per-bucket cap
// to avoid pinning the request on a huge bucket — the UI shows ~ for
// truncated counts.
//
// Cap is intentionally generous (10k objects, 1 GiB summed) so small
// buckets always render real numbers; only outliers get the "approx"
// truncation. The minio-go ListObjects channel is closed early when the
// cap is reached.
func (c *BucketsClient) ListBuckets(ctx context.Context) ([]BucketStat, error) {
	bs, err := c.mc.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	out := make([]BucketStat, 0, len(bs))
	for _, b := range bs {
		stat := BucketStat{Name: b.Name, CreatedAt: b.CreationDate}
		count, size := c.bucketStats(ctx, b.Name)
		stat.ObjectCount = count
		stat.TotalSize = size
		out = append(out, stat)
	}
	return out, nil
}

// bucketStats walks the bucket up to a cap. Errors (e.g. permission, network)
// are swallowed — the UI just renders zero/approximation for that bucket
// rather than failing the whole list.
func (c *BucketsClient) bucketStats(ctx context.Context, bucket string) (int64, int64) {
	const maxObjects = 10000
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var (
		count int64
		size  int64
	)
	for obj := range c.mc.ListObjects(listCtx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			break
		}
		count++
		size += obj.Size
		if count >= maxObjects {
			break
		}
	}
	return count, size
}

// CreateBucket creates a bucket with default region. Surface AlreadyExists
// as ErrBucketAlreadyExists so the handler can return 409 instead of 500.
func (c *BucketsClient) CreateBucket(ctx context.Context, name string) error {
	if err := c.mc.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
		errResp := minio.ToErrorResponse(err)
		switch errResp.Code {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return ErrBucketAlreadyExists
		}
		return fmt.Errorf("make bucket %q: %w", name, err)
	}
	return nil
}

// DeleteBucket removes a bucket. Returns ErrBucketNotEmpty when MinIO
// refuses because the bucket still has objects (or versions). Caller
// must empty the bucket first — Nimbus does not silently force-delete.
func (c *BucketsClient) DeleteBucket(ctx context.Context, name string) error {
	if err := c.mc.RemoveBucket(ctx, name); err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "BucketNotEmpty" {
			return ErrBucketNotEmpty
		}
		return fmt.Errorf("remove bucket %q: %w", name, err)
	}
	return nil
}

// HealthCheck pings the MinIO server's basic readiness — used by the
// deploy orchestrator's poll loop. Returns nil on first 200 from
// ListBuckets (the cheapest authenticated request).
func (c *BucketsClient) HealthCheck(ctx context.Context) error {
	_, err := c.mc.ListBuckets(ctx)
	return err
}
