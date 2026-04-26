package service_test

import (
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/service"
)

func newAuthService(t *testing.T) (*service.AuthService, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.User{}, &db.Session{}, &db.OAuthSettings{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return service.NewAuthService(database), database
}

// Backfill should be a no-op on an empty DB — there's nothing to promote
// and no admin missing in the first place.
func TestPromoteFirstUserIfNoAdmin_NoUsers_NoOp(t *testing.T) {
	t.Parallel()
	svc, _ := newAuthService(t)

	promoted, err := svc.PromoteFirstUserIfNoAdmin()
	if err != nil {
		t.Fatalf("PromoteFirstUserIfNoAdmin: %v", err)
	}
	if promoted {
		t.Error("expected no promotion on empty DB")
	}
}

// The realistic homelab case: one pre-`is_admin` user with default false.
// First call promotes them; second call is a no-op (idempotent).
func TestPromoteFirstUserIfNoAdmin_SingleUser_Promoted(t *testing.T) {
	t.Parallel()
	svc, database := newAuthService(t)

	if err := database.Create(&db.User{
		Name: "alice", Email: "a@x", PasswordHash: "x", IsAdmin: false,
	}).Error; err != nil {
		t.Fatal(err)
	}

	promoted, err := svc.PromoteFirstUserIfNoAdmin()
	if err != nil {
		t.Fatalf("PromoteFirstUserIfNoAdmin: %v", err)
	}
	if !promoted {
		t.Fatal("expected promotion of single non-admin user")
	}

	var loaded db.User
	if err := database.First(&loaded, "email = ?", "a@x").Error; err != nil {
		t.Fatal(err)
	}
	if !loaded.IsAdmin {
		t.Errorf("user.IsAdmin = false, want true after backfill")
	}

	// Idempotent — second call is a no-op.
	again, err := svc.PromoteFirstUserIfNoAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("second call should be no-op")
	}
}

// Multi-user case: only the oldest (lowest ID) gets promoted; others stay
// members.
func TestPromoteFirstUserIfNoAdmin_MultiUser_OldestOnly(t *testing.T) {
	t.Parallel()
	svc, database := newAuthService(t)

	for _, email := range []string{"first@x", "second@x", "third@x"} {
		if err := database.Create(&db.User{
			Name: email, Email: email, PasswordHash: "x", IsAdmin: false,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	promoted, err := svc.PromoteFirstUserIfNoAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if !promoted {
		t.Fatal("expected promotion")
	}

	var users []db.User
	if err := database.Order("id ASC").Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	if !users[0].IsAdmin {
		t.Errorf("first user should be admin")
	}
	for _, u := range users[1:] {
		if u.IsAdmin {
			t.Errorf("user %s should not be admin", u.Email)
		}
	}
}

// When an admin already exists, the backfill must not touch anything.
func TestPromoteFirstUserIfNoAdmin_AdminExists_NoOp(t *testing.T) {
	t.Parallel()
	svc, database := newAuthService(t)

	if err := database.Create(&db.User{
		Name: "boss", Email: "boss@x", PasswordHash: "x", IsAdmin: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&db.User{
		Name: "alice", Email: "a@x", PasswordHash: "x", IsAdmin: false,
	}).Error; err != nil {
		t.Fatal(err)
	}

	promoted, err := svc.PromoteFirstUserIfNoAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("should not promote when admin already exists")
	}

	var alice db.User
	if err := database.First(&alice, "email = ?", "a@x").Error; err != nil {
		t.Fatal(err)
	}
	if alice.IsAdmin {
		t.Error("non-admin user should remain non-admin")
	}
}
