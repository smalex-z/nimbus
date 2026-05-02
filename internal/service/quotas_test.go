package service_test

import (
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/service"
)

// newQuotaAuthService is a quota-test-specific helper. Adds the
// QuotaSettings table to AutoMigrate so the FirstOrCreate-on-read
// path doesn't trip; seeds one member user the tests can override.
func newQuotaAuthService(t *testing.T) (*service.AuthService, uint) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quotas.db")
	database, err := db.New(path, &db.User{}, &db.OAuthSettings{}, &db.QuotaSettings{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	user := &db.User{Name: "alice", Email: "a@x"}
	if err := database.Create(user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return service.NewAuthService(database), user.ID
}

// Default-only path: with no override on the user, the workspace
// default flows through to the effective cap. Confirms the seed
// (MemberMaxVMs=5, MemberMaxActiveJobs=5) lands as the answer.
func TestEffectiveQuota_FallsBackToWorkspaceDefault(t *testing.T) {
	t.Parallel()
	svc, uid := newQuotaAuthService(t)

	got, err := svc.EffectiveVMQuota(uid)
	if err != nil {
		t.Fatalf("EffectiveVMQuota: %v", err)
	}
	if got != 5 {
		t.Errorf("VM quota = %d, want 5 (default seed)", got)
	}
	got, err = svc.EffectiveGPUJobQuota(uid)
	if err != nil {
		t.Fatalf("EffectiveGPUJobQuota: %v", err)
	}
	if got != 5 {
		t.Errorf("GPU quota = %d, want 5 (default seed)", got)
	}
}

// Override path: per-user override beats the workspace default.
// Seven > 5 verifies "above default" and zero verifies the explicit
// "this user can't provision" semantic.
func TestEffectiveQuota_OverrideTrumpsDefault(t *testing.T) {
	t.Parallel()
	svc, uid := newQuotaAuthService(t)

	seven := 7
	if err := svc.SetUserQuotaOverride(uid, &seven, nil); err != nil {
		t.Fatalf("SetUserQuotaOverride: %v", err)
	}
	got, err := svc.EffectiveVMQuota(uid)
	if err != nil {
		t.Fatalf("EffectiveVMQuota: %v", err)
	}
	if got != 7 {
		t.Errorf("VM quota = %d, want 7 (override)", got)
	}
	// GPU dimension wasn't overridden; should still report the default.
	if got, _ := svc.EffectiveGPUJobQuota(uid); got != 5 {
		t.Errorf("GPU quota = %d, want 5 (untouched, defaults)", got)
	}

	// Explicit zero — this user can't provision.
	zero := 0
	if err := svc.SetUserQuotaOverride(uid, &zero, nil); err != nil {
		t.Fatalf("SetUserQuotaOverride zero: %v", err)
	}
	if got, _ := svc.EffectiveVMQuota(uid); got != 0 {
		t.Errorf("VM quota = %d, want 0 (explicit zero override)", got)
	}
}

// Clear path: override flipped on, then off, reverts to default.
// Critical because we use a typed Updates(nil-pointer) trick to write
// NULL — easy to get wrong and end up writing zero instead.
func TestEffectiveQuota_ClearRevertsToDefault(t *testing.T) {
	t.Parallel()
	svc, uid := newQuotaAuthService(t)

	ten := 10
	if err := svc.SetUserQuotaOverride(uid, &ten, &ten); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := svc.EffectiveVMQuota(uid); got != 10 {
		t.Fatalf("pre-clear VM = %d, want 10", got)
	}

	if err := svc.ClearUserQuotaOverride(uid, true, true); err != nil {
		t.Fatalf("ClearUserQuotaOverride: %v", err)
	}
	got, _ := svc.EffectiveVMQuota(uid)
	if got != 5 {
		t.Errorf("post-clear VM = %d, want 5 (back to default)", got)
	}
	got, _ = svc.EffectiveGPUJobQuota(uid)
	if got != 5 {
		t.Errorf("post-clear GPU = %d, want 5", got)
	}
}

// Workspace default change propagates to a user without an override.
// Confirms the resolver always reads the live row, not a snapshot.
func TestEffectiveQuota_DefaultChangeFlowsThrough(t *testing.T) {
	t.Parallel()
	svc, uid := newQuotaAuthService(t)

	if _, err := svc.SaveQuotaSettings(11, 12); err != nil {
		t.Fatalf("SaveQuotaSettings: %v", err)
	}
	if got, _ := svc.EffectiveVMQuota(uid); got != 11 {
		t.Errorf("VM quota = %d, want 11", got)
	}
	if got, _ := svc.EffectiveGPUJobQuota(uid); got != 12 {
		t.Errorf("GPU quota = %d, want 12", got)
	}
}

// Negative quota values are rejected at save — they never reach the
// settings row, so an EffectiveVMQuota call won't return a negative.
func TestSaveQuotaSettings_RejectsNegative(t *testing.T) {
	t.Parallel()
	svc, _ := newQuotaAuthService(t)

	if _, err := svc.SaveQuotaSettings(-1, 5); err == nil {
		t.Error("expected error on negative VM quota")
	}
	if _, err := svc.SaveQuotaSettings(5, -1); err == nil {
		t.Error("expected error on negative GPU quota")
	}
}
