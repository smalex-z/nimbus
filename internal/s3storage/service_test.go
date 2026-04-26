package s3storage_test

import (
	"errors"
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/s3storage"
)

// newTestService returns a Service backed by a fresh on-disk SQLite file.
// Mirrors the ippool tests' pattern — file-backed (not :memory:) so the
// single-connection pool from db.New behaves like production.
func newTestService(t *testing.T) *s3storage.Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.S3Storage{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return s3storage.New(database.DB)
}

func TestService_GetEmpty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	_, err := svc.Get()
	if !errors.Is(err, s3storage.ErrNotDeployed) {
		t.Fatalf("Get on empty: got %v, want ErrNotDeployed", err)
	}
}

func TestService_CreateThenGet(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	row, err := svc.Create(s3storage.CreateParams{
		VMID:         101,
		Node:         "alpha",
		DiskGB:       50,
		RootUser:     "u",
		RootPassword: "p",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != s3storage.StatusDeploying {
		t.Errorf("status = %s, want %s", row.Status, s3storage.StatusDeploying)
	}
	got, err := svc.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.VMID != 101 || got.Node != "alpha" || got.DiskGB != 50 {
		t.Errorf("got %+v, want vmid=101 node=alpha disk=50", got)
	}
}

func TestService_DoubleCreateRejected(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	if _, err := svc.Create(s3storage.CreateParams{VMID: 1, Node: "n", DiskGB: 30}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(s3storage.CreateParams{VMID: 2, Node: "n", DiskGB: 30})
	if !errors.Is(err, s3storage.ErrAlreadyDeployed) {
		t.Fatalf("second Create: got %v, want ErrAlreadyDeployed", err)
	}
}

// TestService_StateTransitions covers the deploying → ready → error path
// the orchestrator walks. Each Mark call must update both Status and the
// associated metadata column without losing other fields.
func TestService_StateTransitions(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	if _, err := svc.Create(s3storage.CreateParams{VMID: 1, Node: "n", DiskGB: 30}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.MarkReady("http://10.0.0.42:9000"); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	row, _ := svc.Get()
	if row.Status != s3storage.StatusReady || row.Endpoint != "http://10.0.0.42:9000" {
		t.Errorf("after MarkReady: %+v", row)
	}
	if row.ErrorMsg != "" {
		t.Errorf("error_msg should be cleared by MarkReady, got %q", row.ErrorMsg)
	}

	if err := svc.MarkError("docker pull failed"); err != nil {
		t.Fatalf("MarkError: %v", err)
	}
	row, _ = svc.Get()
	if row.Status != s3storage.StatusError || row.ErrorMsg != "docker pull failed" {
		t.Errorf("after MarkError: %+v", row)
	}
	// The pre-existing endpoint must persist — operator inspection wants
	// to know what endpoint MinIO was supposed to be at.
	if row.Endpoint != "http://10.0.0.42:9000" {
		t.Errorf("endpoint clobbered by MarkError: %q", row.Endpoint)
	}
}

func TestService_Delete(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	if _, err := svc.Create(s3storage.CreateParams{VMID: 1, Node: "n", DiskGB: 30}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(); !errors.Is(err, s3storage.ErrNotDeployed) {
		t.Errorf("after Delete, Get returned %v, want ErrNotDeployed", err)
	}
	// Delete is idempotent — calling on an empty table must not error.
	if err := svc.Delete(); err != nil {
		t.Errorf("idempotent Delete: %v", err)
	}
}

func TestService_BucketsRequiresReady(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	// No row at all → ErrNotDeployed
	if _, err := svc.Buckets(); !errors.Is(err, s3storage.ErrNotDeployed) {
		t.Fatalf("Buckets on empty: got %v, want ErrNotDeployed", err)
	}
	if _, err := svc.Create(s3storage.CreateParams{VMID: 1, Node: "n", DiskGB: 30}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Status=deploying → ErrNotReady
	if _, err := svc.Buckets(); !errors.Is(err, s3storage.ErrNotReady) {
		t.Fatalf("Buckets on deploying: got %v, want ErrNotReady", err)
	}
	if err := svc.MarkReady("http://10.0.0.42:9000"); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	// Now valid — we just check no error; we don't actually dial MinIO.
	bc, err := svc.Buckets()
	if err != nil {
		t.Fatalf("Buckets after MarkReady: %v", err)
	}
	if bc == nil {
		t.Fatal("BucketsClient is nil")
	}
}

func TestGenerateRootCredentials(t *testing.T) {
	t.Parallel()
	u1, p1, err := s3storage.GenerateRootCredentials()
	if err != nil {
		t.Fatalf("GenerateRootCredentials: %v", err)
	}
	u2, p2, err := s3storage.GenerateRootCredentials()
	if err != nil {
		t.Fatalf("GenerateRootCredentials: %v", err)
	}
	if u1 == u2 || p1 == p2 {
		t.Errorf("creds should be unique across calls: u1=%q u2=%q p1=%q p2=%q", u1, u2, p1, p2)
	}
	if len(p1) < 32 {
		t.Errorf("password too short: %d", len(p1))
	}
}
