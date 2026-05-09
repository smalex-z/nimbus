package service_test

import (
	"path/filepath"
	"testing"

	"nimbus/internal/db"
	"nimbus/internal/service"
)

// newGopherTestService spins up an AuthService with the GopherSettings
// table migrated so the wipe path can read/write it.
func newGopherTestService(t *testing.T) *service.AuthService {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(path, &db.User{}, &db.GopherSettings{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return service.NewAuthService(database)
}

// TestWipeGopherSettings_ClearsEveryColumn covers the full clean-slate
// behaviour: credentials, subdomain, machine/tunnel ids, URL, state,
// and error all land back at empty after WipeGopherSettings.
func TestWipeGopherSettings_ClearsEveryColumn(t *testing.T) {
	t.Parallel()
	svc := newGopherTestService(t)

	if err := svc.SaveGopherSettings(db.GopherSettings{
		APIURL:         "https://gopher.example.com",
		APIKey:         "key-abc",
		CloudSubdomain: "cloud",
	}); err != nil {
		t.Fatalf("SaveGopherSettings: %v", err)
	}
	if err := svc.SaveCloudTunnelState(db.GopherSettings{
		CloudMachineID:      "m-1",
		CloudTunnelID:       "t-1",
		CloudTunnelURL:      "https://cloud.example.com",
		CloudBootstrapState: "active",
		CloudBootstrapError: "",
	}); err != nil {
		t.Fatalf("SaveCloudTunnelState: %v", err)
	}

	if err := svc.WipeGopherSettings(); err != nil {
		t.Fatalf("WipeGopherSettings: %v", err)
	}

	got, err := svc.GetGopherSettings()
	if err != nil {
		t.Fatalf("GetGopherSettings: %v", err)
	}
	if got.APIURL != "" || got.APIKey != "" || got.CloudSubdomain != "" {
		t.Errorf("creds not wiped: %+v", got)
	}
	if got.CloudMachineID != "" || got.CloudTunnelID != "" || got.CloudTunnelURL != "" {
		t.Errorf("tunnel state not wiped: %+v", got)
	}
	if got.CloudBootstrapState != "" || got.CloudBootstrapError != "" {
		t.Errorf("bootstrap state not wiped: %+v", got)
	}
}

// TestWipeGopherSettings_OnEmptyRow_NoError covers calling Wipe before
// anything was ever saved — the FirstOrCreate inside should still
// produce a clean row, not an error.
func TestWipeGopherSettings_OnEmptyRow_NoError(t *testing.T) {
	t.Parallel()
	svc := newGopherTestService(t)

	if err := svc.WipeGopherSettings(); err != nil {
		t.Errorf("WipeGopherSettings on empty row: %v", err)
	}
}
