package s3storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// AdminClient wraps madmin-go's AdminClient for the small subset Nimbus uses
// (service-account create / update / delete). Constructed with the storage
// row's root credentials — same auth used by BucketsClient.
type AdminClient struct {
	mc *madmin.AdminClient
}

// newAdminClient builds an admin client against the storage row's endpoint
// and root creds. Reuses splitEndpoint (minio.go) so the parsing rules match
// the bucket client's exactly.
func newAdminClient(endpoint, accessKey, secretKey string) (*AdminClient, error) {
	host, secure, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	mc, err := madmin.NewWithOptions(host, &madmin.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("madmin.New: %w", err)
	}
	return &AdminClient{mc: mc}, nil
}

// MintServiceAccount creates a service account with the given access/secret
// pair and inline policy. Idempotent: if MinIO already has an account with
// the same access key, the call falls back to UpdateServiceAccount so the
// policy + secret converge to the requested values.
func (c *AdminClient) MintServiceAccount(ctx context.Context, accessKey, secretKey, policyJSON string) error {
	_, err := c.mc.AddServiceAccount(ctx, madmin.AddServiceAccountReq{
		AccessKey: accessKey,
		SecretKey: secretKey,
		Policy:    json.RawMessage(policyJSON),
	})
	if err == nil {
		return nil
	}
	// Already-exists: replay as Update so a re-mint converges policy + secret
	// rather than wedging on the duplicate.
	if isAccountAlreadyExists(err) {
		secretCopy := secretKey
		policyCopy := json.RawMessage(policyJSON)
		updErr := c.mc.UpdateServiceAccount(ctx, accessKey, madmin.UpdateServiceAccountReq{
			NewSecretKey: secretCopy,
			NewPolicy:    policyCopy,
		})
		if updErr != nil {
			return fmt.Errorf("update existing service account %s: %w", accessKey, updErr)
		}
		return nil
	}
	return fmt.Errorf("add service account %s: %w", accessKey, err)
}

// DeleteServiceAccount removes a service account. 404-ish responses (account
// already gone) are treated as success so cascade cleanup is idempotent.
func (c *AdminClient) DeleteServiceAccount(ctx context.Context, accessKey string) error {
	if err := c.mc.DeleteServiceAccount(ctx, accessKey); err != nil {
		if isAccountNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete service account %s: %w", accessKey, err)
	}
	return nil
}

// BuildPolicyJSON returns an IAM-style policy document scoping a service
// account to bucket names matching `<prefix>-*`. The doc grants object-level
// CRUD plus the two listing actions; CreateBucket/DeleteBucket are
// deliberately omitted so the prefix invariant is enforced server-side
// (Nimbus calls MakeBucket with root creds, never the SA's).
//
// Two resource patterns are required:
//   - arn:aws:s3:::<prefix>-*   for bucket-scoped actions (ListBucket)
//   - arn:aws:s3:::<prefix>-*\/* for object-scoped actions (GetObject etc.)
//
// ListAllMyBuckets is granted with resource "*" because the action itself
// is account-wide; MinIO will still filter the returned list to buckets
// the policy can otherwise touch.
func BuildPolicyJSON(prefix string) string {
	type stmt struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	}
	doc := struct {
		Version   string `json:"Version"`
		Statement []stmt `json:"Statement"`
	}{
		Version: "2012-10-17",
		Statement: []stmt{
			{
				Effect:   "Allow",
				Action:   []string{"s3:ListAllMyBuckets"},
				Resource: []string{"arn:aws:s3:::*"},
			},
			{
				Effect: "Allow",
				Action: []string{
					"s3:ListBucket",
					"s3:GetBucketLocation",
				},
				Resource: []string{
					fmt.Sprintf("arn:aws:s3:::%s-*", prefix),
				},
			},
			{
				Effect: "Allow",
				Action: []string{
					"s3:GetObject",
					"s3:PutObject",
					"s3:DeleteObject",
				},
				Resource: []string{
					fmt.Sprintf("arn:aws:s3:::%s-*/*", prefix),
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		// json.Marshal of a fixed struct cannot fail under normal circumstances.
		// Returning a syntactically-valid empty policy keeps the caller from
		// crashing if Go's stdlib ever does the impossible.
		return `{"Version":"2012-10-17","Statement":[]}`
	}
	return string(b)
}

// isAccountAlreadyExists detects MinIO's "service account already exists"
// response, which surfaces as a typed madmin.ErrorResponse with a code
// starting with "XMinioAdminServiceAccountAlready" — wording has shifted
// across MinIO versions, so we match by prefix.
func isAccountAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	var er madmin.ErrorResponse
	if errors.As(err, &er) {
		if strings.Contains(er.Code, "ServiceAccountAlready") || strings.Contains(er.Code, "AlreadyExists") {
			return true
		}
	}
	// Fallback: some MinIO builds return the error string only.
	msg := err.Error()
	return strings.Contains(msg, "service account already exists") ||
		strings.Contains(msg, "ServiceAccountAlreadyExists") ||
		strings.Contains(msg, "already exists")
}

// isAccountNotFound detects responses indicating the service account no
// longer exists — used to make DeleteServiceAccount idempotent.
func isAccountNotFound(err error) bool {
	if err == nil {
		return false
	}
	var er madmin.ErrorResponse
	if errors.As(err, &er) {
		if strings.Contains(er.Code, "NoSuchServiceAccount") || strings.Contains(er.Code, "NoSuchUser") {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "service account does not exist") ||
		strings.Contains(msg, "specified service account does not exist") ||
		strings.Contains(msg, "no such")
}
