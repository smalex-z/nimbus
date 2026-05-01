package service_test

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/service"
)

// newTokenAuthService is a helper specific to LoginToken tests; the
// shared helper in auth_test.go doesn't AutoMigrate the LoginToken
// table because it predates this feature. Adding LoginToken to that
// helper would also work, but a focused helper here keeps the blast
// radius narrow and lets these tests evolve their own fixtures
// independently.
func newTokenAuthService(t *testing.T) *service.AuthService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.db")
	database, err := db.New(path, &db.User{}, &db.LoginToken{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if err := database.Create(&db.User{Name: "alice", Email: "a@x"}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return service.NewAuthService(database)
}

func TestMintAndConsumeLoginToken_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	tok, err := svc.MintLoginToken(1, "magic_link", time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(tok) != 32 {
		t.Fatalf("token length = %d, want 32 hex chars (16 bytes)", len(tok))
	}

	uid, err := svc.ConsumeLoginToken(tok, "magic_link")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if uid != 1 {
		t.Errorf("uid = %d, want 1", uid)
	}
}

func TestConsumeLoginToken_RejectsSecondUse(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	tok, err := svc.MintLoginToken(1, "magic_link", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConsumeLoginToken(tok, "magic_link"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	_, err = svc.ConsumeLoginToken(tok, "magic_link")
	if !errors.Is(err, service.ErrLoginTokenUsed) {
		t.Fatalf("second Consume: err = %v, want ErrLoginTokenUsed", err)
	}
}

func TestConsumeLoginToken_RejectsExpired(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	// Mint with an absurdly short TTL, then sleep past it. Avoids
	// reaching into the DB to backdate expires_at, which would
	// require unexported test seams.
	tok, err := svc.MintLoginToken(1, "magic_link", 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	_, err = svc.ConsumeLoginToken(tok, "magic_link")
	if !errors.Is(err, service.ErrLoginTokenExpired) {
		t.Fatalf("Consume after expiry: err = %v, want ErrLoginTokenExpired", err)
	}
}

func TestConsumeLoginToken_RejectsWrongPurpose(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	tok, err := svc.MintLoginToken(1, "magic_link", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ConsumeLoginToken(tok, "password_reset")
	if !errors.Is(err, service.ErrLoginTokenInvalid) {
		t.Fatalf("Consume with wrong purpose: err = %v, want ErrLoginTokenInvalid", err)
	}
}

func TestConsumeLoginToken_RejectsUnknown(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	_, err := svc.ConsumeLoginToken("0000000000000000", "magic_link")
	if !errors.Is(err, service.ErrLoginTokenInvalid) {
		t.Fatalf("Consume with unknown token: err = %v, want ErrLoginTokenInvalid", err)
	}
}

// Single-use under concurrency: 50 goroutines race to redeem the same
// token; exactly one wins, the rest get ErrLoginTokenUsed (or
// ErrLoginTokenInvalid via the WHERE used_at IS NULL guard, depending
// on which path the loser takes). Verifies the atomic UPDATE actually
// is atomic on top of SQLite's single-writer setup.
func TestConsumeLoginToken_SingleUseUnderConcurrency(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	tok, err := svc.MintLoginToken(1, "magic_link", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	var winners atomic.Int32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.ConsumeLoginToken(tok, "magic_link"); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("winners = %d, want exactly 1", got)
	}
}

func TestPurgeExpiredLoginTokens(t *testing.T) {
	t.Parallel()
	svc := newTokenAuthService(t)

	// Live: ttl=10s. Should NOT be purged.
	live, _ := svc.MintLoginToken(1, "magic_link", 10*time.Second)
	// Expiring: ttl=5ms; sleep past it before purge.
	dead, _ := svc.MintLoginToken(1, "magic_link", 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	removed, err := svc.PurgeExpiredLoginTokens()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	// Live token still consumable.
	if _, err := svc.ConsumeLoginToken(live, "magic_link"); err != nil {
		t.Errorf("live token rejected after purge: %v", err)
	}
	// Dead token gone.
	if _, err := svc.ConsumeLoginToken(dead, "magic_link"); !errors.Is(err, service.ErrLoginTokenInvalid) {
		t.Errorf("dead token after purge: err = %v, want ErrLoginTokenInvalid", err)
	}
}
