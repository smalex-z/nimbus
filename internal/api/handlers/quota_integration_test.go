package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"nimbus/internal/api"
	"nimbus/internal/config"
	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/provision"
	"nimbus/internal/secrets"
	"nimbus/internal/service"
	"nimbus/internal/sshkeys"
)

// testEnv bundles the server, database, and auth state needed by
// every integration test in this file. Construct via newTestEnv.
type testEnv struct {
	server   *httptest.Server
	db       *db.DB
	auth     *service.AuthService
	adminSID string // session cookie for the admin user
	adminID  uint
}

// newTestEnv boots a real Chi router backed by SQLite + newTestService
// and returns a live httptest.Server. The admin user is created and
// logged in; the returned testEnv.adminSID is the session cookie
// callers must send for admin endpoints.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integration.db")
	database, err := db.New(path,
		ippool.Model(),
		&db.VM{},
		&db.NodeTemplate{},
		&db.SSHKey{},
		&db.Node{},
		&db.User{},
		&db.Session{},
		&db.OAuthSettings{},
		&db.QuotaSettings{},
	)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}

	pool := ippool.New(database.DB)
	if err := pool.Seed(context.Background(), "10.0.0.1", "10.0.0.254"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Seed node_templates so pickNode works.
	vmid := 9000
	for _, os := range []string{"ubuntu-24.04", "ubuntu-22.04", "debian-12", "debian-13"} {
		if err := database.Create(&db.NodeTemplate{
			Node: "alpha", OS: os, VMID: vmid,
		}).Error; err != nil {
			t.Fatalf("seed node_templates: %v", err)
		}
		vmid++
	}

	cipher, err := secrets.New(make([]byte, secrets.KeyLen))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	keysSvc := sshkeys.New(database.DB, cipher)

	provSvc := provision.New(happyFakePVE(t), pool, database.DB, cipher, keysSvc, provision.Config{
		TemplateBaseVMID: 9000,
		GatewayIP:        "10.0.0.1",
		Nameserver:       "1.1.1.1",
		SearchDomain:     "local",
		IPReadyTimeout:   1 * time.Second,
		PollInterval:     5 * time.Millisecond,
		CPUType:          "x86-64-v3",
	})

	authSvc := service.NewAuthService(database)
	provSvc.SetQuotaResolver(authSvc)

	router := api.NewRouter(api.Deps{
		Auth:      authSvc,
		Provision: provSvc,
		Pool:      pool,
		Config:    &config.Config{},
	})

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	// Create admin user via direct DB + service call (matching the real
	// admin flow: Register → promote → login → session cookie).
	adminView, err := authSvc.Register(service.RegisterParams{
		Name: "admin", Email: "admin@test.local", Password: "pass1234",
	})
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if err := database.Model(&db.User{}).Where("id = ?", adminView.ID).
		Update("is_admin", true).Error; err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	adminSID, _, err := authSvc.Login("admin@test.local", "pass1234")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	return &testEnv{
		server:   ts,
		db:       database,
		auth:     authSvc,
		adminSID: adminSID,
		adminID:  adminView.ID,
	}
}

// registerMember creates a non-admin user and returns their session ID
// and user ID.
func (e *testEnv) registerMember(t *testing.T, name, email, password string) (sid string, userID uint) {
	t.Helper()
	view, err := e.auth.Register(service.RegisterParams{
		Name: name, Email: email, Password: password,
	})
	if err != nil {
		t.Fatalf("register member %s: %v", name, err)
	}
	// Auto-verify the member so requireVerified doesn't block provision.
	if err := e.db.Model(&db.User{}).Where("id = ?", view.ID).
		Update("verified_code_version", 1).Error; err != nil {
		t.Fatalf("verify member: %v", err)
	}
	// Seed the OAuthSettings with AccessCodeVersion=1 so IsUserVerified
	// returns true for our verified_code_version=1 user.
	e.db.Where(db.OAuthSettings{ID: 1}).Assign(db.OAuthSettings{AccessCodeVersion: 1}).FirstOrCreate(&db.OAuthSettings{})
	sid, _, err = e.auth.Login(email, password)
	if err != nil {
		t.Fatalf("login member %s: %v", name, err)
	}
	return sid, view.ID
}

// seedVMs inserts n VMs owned by ownerID with distinct vmid/hostname/ip
// values so unique-index constraints don't trip.
func (e *testEnv) seedVMs(t *testing.T, ownerID uint, n int) {
	t.Helper()
	owner := ownerID
	for i := 0; i < n; i++ {
		row := &db.VM{
			VMID:       5000 + int(ownerID)*100 + i,
			Hostname:   fmt.Sprintf("seed-%d-%d", ownerID, i),
			IP:         fmt.Sprintf("10.99.%d.%d", ownerID, i+1),
			Node:       "alpha",
			Tier:       "small",
			OSTemplate: "ubuntu-24.04",
			Status:     "running",
			OwnerID:    &owner,
		}
		if err := e.db.Create(row).Error; err != nil {
			t.Fatalf("seedVMs(%d, %d): %v", ownerID, i, err)
		}
	}
}

// provisionVM sends POST /api/vms as the given session and returns the
// HTTP status code plus the body bytes.
func (e *testEnv) provisionVM(t *testing.T, sid, hostname, tier string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"hostname":     hostname,
		"tier":         tier,
		"os_template":  "ubuntu-24.04",
		"generate_key": true,
	})
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/api/vms", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "nimbus_sid", Value: sid})
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/vms: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// setUserQuota calls PUT /api/users/{id}/quota as admin.
func (e *testEnv) setUserQuota(t *testing.T, userID uint, vmQuota *int) int {
	t.Helper()
	bodyMap := map[string]any{}
	if vmQuota != nil {
		bodyMap["vm_quota_override"] = *vmQuota
	}
	body, _ := json.Marshal(bodyMap)
	req, _ := http.NewRequest(http.MethodPut,
		e.server.URL+"/api/users/"+strconv.Itoa(int(userID))+"/quota",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "nimbus_sid", Value: e.adminSID})
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT /api/users/%d/quota: %v", userID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

// --- Per-user provision quotas ---

func TestIntegration_MemberAtCapRejected(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, uid := env.registerMember(t, "alice", "alice@test.local", "pass1234")

	// Default cap is 5. Seed 5 VMs, then try to provision one more.
	env.seedVMs(t, uid, 5)
	status, body := env.provisionVM(t, sid, "over-cap", "small")

	// The provision endpoint streams NDJSON — when the quota gate fires
	// before any streaming starts, the handler returns a standard JSON
	// error envelope. The key signal is the error message mentioning
	// "quota" or "resource-limit." A pre-stream rejection could be 400
	// (ValidationError) or an NDJSON error line; check for a resource-
	// limit-shaped message in either case.
	if status == http.StatusOK {
		// NDJSON was streamed; check for an error line.
		if !bytes.Contains(body, []byte("quota")) && !bytes.Contains(body, []byte("VM quota reached")) {
			t.Fatalf("expected quota error in NDJSON body, got: %s", body)
		}
	} else if status != http.StatusOK {
		// Standard JSON error — verify the message.
		if !bytes.Contains(body, []byte("quota")) && !bytes.Contains(body, []byte("VM quota reached")) &&
			!bytes.Contains(body, []byte("resource-limit")) {
			t.Fatalf("expected quota error, status=%d body=%s", status, body)
		}
	}
}

func TestIntegration_MemberBelowCapSucceeds(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, uid := env.registerMember(t, "bob", "bob@test.local", "pass1234")

	// Seed 4 VMs (one below the default cap of 5).
	env.seedVMs(t, uid, 4)
	status, body := env.provisionVM(t, sid, "under-cap", "small")

	if status != http.StatusOK {
		t.Fatalf("expected 200 (streaming), got %d: %s", status, body)
	}
	// Should contain a result line with the VM data.
	if !bytes.Contains(body, []byte(`"type":"result"`)) {
		t.Errorf("expected result event in NDJSON stream, got: %s", body)
	}
}

func TestIntegration_AdminOverrideRaisesCap(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, uid := env.registerMember(t, "carol", "carol@test.local", "pass1234")

	// Admin sets per-user override to 10.
	override := 10
	qStatus := env.setUserQuota(t, uid, &override)
	if qStatus != http.StatusOK {
		t.Fatalf("setUserQuota: status %d", qStatus)
	}

	// Seed 7 VMs (above the default 5, but below override 10).
	env.seedVMs(t, uid, 7)
	status, body := env.provisionVM(t, sid, "override-ok", "small")

	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"type":"result"`)) {
		t.Errorf("expected result event, got: %s", body)
	}
}

func TestIntegration_OverrideLowered_BlocksNextProvision(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, uid := env.registerMember(t, "dave", "dave@test.local", "pass1234")

	// Seed 5 VMs.
	env.seedVMs(t, uid, 5)

	// Admin lowers the override to 3 (user already has 5 — no
	// retroactive deletion, but next provision attempt should be blocked).
	override := 3
	env.setUserQuota(t, uid, &override)

	status, body := env.provisionVM(t, sid, "blocked", "small")
	if status == http.StatusOK && bytes.Contains(body, []byte(`"type":"result"`)) {
		t.Fatalf("expected quota rejection, but provision succeeded: %s", body)
	}
	if !bytes.Contains(body, []byte("quota")) && !bytes.Contains(body, []byte("VM quota reached")) {
		t.Fatalf("expected quota error, got status=%d body=%s", status, body)
	}
}

// --- Tier allowlist ---

func TestIntegration_MemberDisallowedTier(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, _ := env.registerMember(t, "eve", "eve@test.local", "pass1234")

	// "xl" is not in MemberAllowedTiers → should be rejected.
	status, body := env.provisionVM(t, sid, "xl-attempt", "xl")

	// The handler-level validateCreate rejects unknown tiers (xl is not
	// in nodescore.Tiers) with a 400. The quota_test.go unit tests
	// confirm the service-level tier allowlist fires for tier values
	// that ARE in nodescore.Tiers but NOT in MemberAllowedTiers. At the
	// HTTP boundary, the signal is the same: a 4xx with a tier error.
	if status == http.StatusOK && bytes.Contains(body, []byte(`"type":"result"`)) {
		t.Fatalf("expected tier rejection, but provision succeeded")
	}
	if !bytes.Contains(body, []byte("tier")) {
		t.Fatalf("expected tier error, got status=%d body=%s", status, body)
	}
}

func TestIntegration_AdminBypassesTierAllowlist(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	// Admin provisions with "xl" (admin-only tier). This verifies the
	// admin path through the full HTTP stack bypasses the tier allowlist.
	status, body := env.provisionVM(t, env.adminSID, "admin-xl", "xl")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"type":"result"`)) {
		t.Errorf("expected result event, got: %s", body)
	}
}

// --- Admin quota settings ---

func TestIntegration_SaveQuotaSettings_PropagatesLowerCap(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	sid, uid := env.registerMember(t, "frank", "frank@test.local", "pass1234")

	// Seed 4 VMs (under default cap of 5).
	env.seedVMs(t, uid, 4)

	// Admin lowers MemberMaxVMs from 5 → 3 via PUT /api/settings/quotas.
	body, _ := json.Marshal(map[string]any{"member_max_vms": 3})
	req, _ := http.NewRequest(http.MethodPut, env.server.URL+"/api/settings/quotas", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "nimbus_sid", Value: env.adminSID})
	resp, err := env.server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT /api/settings/quotas: %v", err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SaveQuotas: status %d", resp.StatusCode)
	}

	// Member at 4 VMs now exceeds the new cap of 3 → next provision blocked.
	status, respBody := env.provisionVM(t, sid, "after-lower", "small")
	if status == http.StatusOK && bytes.Contains(respBody, []byte(`"type":"result"`)) {
		t.Fatalf("expected quota rejection after lowered cap, but provision succeeded")
	}
	if !bytes.Contains(respBody, []byte("quota")) && !bytes.Contains(respBody, []byte("VM quota reached")) {
		t.Fatalf("expected quota error, got status=%d body=%s", status, respBody)
	}
}
