package service_test

import (
	"errors"
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

// SetPassword on an OAuth-only account (no PasswordHash) accepts any
// oldPassword (including empty) and sets a fresh hash. After setting, the
// new password verifies and the user can sign in via Login.
func TestSetPassword_SetOnOAuthOnlyAccount(t *testing.T) {
	t.Parallel()
	svc, database := newAuthService(t)

	if err := database.Create(&db.User{
		Name: "oauth-only", Email: "oa@x", PasswordHash: "",
	}).Error; err != nil {
		t.Fatal(err)
	}
	var user db.User
	if err := database.First(&user, "email = ?", "oa@x").Error; err != nil {
		t.Fatal(err)
	}

	// oldPassword should be ignored when there's no current password —
	// pass a junk value and confirm SetPassword still succeeds.
	if err := svc.SetPassword(user.ID, "ignored", "newpassword12"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := svc.VerifyPassword(user.ID, "newpassword12"); err != nil {
		t.Errorf("expected new password to verify, got %v", err)
	}

	// Login flow works end-to-end with the freshly set password.
	if _, _, err := svc.Login("oa@x", "newpassword12"); err != nil {
		t.Errorf("Login with fresh password failed: %v", err)
	}
}

// SetPassword on an account that already has one requires the old to match.
// Wrong-old returns ErrInvalidCredentials and leaves the stored hash alone.
func TestSetPassword_ChangeRequiresOldPassword(t *testing.T) {
	t.Parallel()
	svc, _ := newAuthService(t)

	view, err := svc.Register(service.RegisterParams{
		Name: "alice", Email: "a@x", Password: "originalpw1",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Wrong old — rejected, original password still works.
	err = svc.SetPassword(view.ID, "wrong-old", "newpassword12")
	if !errors.Is(err, service.ErrInvalidCredentials) {
		t.Fatalf("wrong old should return ErrInvalidCredentials, got %v", err)
	}
	if err := svc.VerifyPassword(view.ID, "originalpw1"); err != nil {
		t.Errorf("original password should still work after rejected change, got %v", err)
	}

	// Correct old — accepted, new password takes effect, old no longer works.
	if err := svc.SetPassword(view.ID, "originalpw1", "newpassword12"); err != nil {
		t.Fatalf("SetPassword with correct old: %v", err)
	}
	if err := svc.VerifyPassword(view.ID, "newpassword12"); err != nil {
		t.Errorf("new password should verify, got %v", err)
	}
	if err := svc.VerifyPassword(view.ID, "originalpw1"); !errors.Is(err, service.ErrInvalidCredentials) {
		t.Errorf("old password should no longer work, got %v", err)
	}
}

// SetPassword on a missing user returns ErrUserNotFound — the handler
// surfaces this as 404 so a deleted-then-resurrected session can't quietly
// land on a stranger's row.
func TestSetPassword_UnknownUser(t *testing.T) {
	t.Parallel()
	svc, _ := newAuthService(t)

	err := svc.SetPassword(999, "", "newpassword12")
	if !errors.Is(err, service.ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
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
