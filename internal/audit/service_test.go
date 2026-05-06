package audit_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nimbus/internal/audit"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/service"
)

// TestRecord_PersistsActorAndDetails verifies the happy path: Record
// stamps in the actor from ctx, marshals Details to JSON, and the row
// round-trips intact via List.
func TestRecord_PersistsActorAndDetails(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	ctx := context.Background()
	ctx = ctxutil.WithUser(ctx, &service.UserView{ID: 42, Email: "alice@example.com", IsAdmin: true})
	ctx = ctxutil.WithClientIP(ctx, "10.0.0.99")
	ctx = ctxutil.WithRequestID(ctx, "req-123")

	svc.Record(ctx, audit.Event{
		Action:      "vm.provision",
		TargetType:  "vm",
		TargetID:    "100",
		TargetLabel: "test-vm",
		Details:     map[string]any{"tier": "small", "node": "alpha"},
		Success:     true,
	})

	res, err := svc.List(ctx, audit.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(res.Events))
	}
	got := res.Events[0]
	if got.Action != "vm.provision" {
		t.Errorf("Action = %q, want vm.provision", got.Action)
	}
	if got.ActorID == nil || *got.ActorID != 42 {
		t.Errorf("ActorID = %v, want 42", got.ActorID)
	}
	if got.ActorEmail != "alice@example.com" {
		t.Errorf("ActorEmail = %q", got.ActorEmail)
	}
	if !got.ActorAdmin {
		t.Errorf("ActorAdmin should be true")
	}
	if got.IPAddress != "10.0.0.99" {
		t.Errorf("IPAddress = %q", got.IPAddress)
	}
	if got.RequestID != "req-123" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.TargetLabel != "test-vm" {
		t.Errorf("TargetLabel = %q", got.TargetLabel)
	}
	if got.DetailsJSON == "" {
		t.Errorf("DetailsJSON empty; expected JSON-encoded payload")
	}
	if !got.Success {
		t.Errorf("Success should be true")
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1", res.Total)
	}
}

// TestRecord_AnonymousEventNoActor verifies a Record call without a
// user in ctx (e.g. failed-login audit) lands with empty actor fields
// rather than dropping.
func TestRecord_AnonymousEventNoActor(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	svc.Record(context.Background(), audit.Event{
		Action:   "auth.login",
		Success:  false,
		ErrorMsg: "invalid credentials",
	})

	res, err := svc.List(context.Background(), audit.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(res.Events))
	}
	got := res.Events[0]
	if got.ActorID != nil {
		t.Errorf("ActorID = %v, want nil for anonymous", got.ActorID)
	}
	if got.Success {
		t.Errorf("Success should be false")
	}
	if got.ErrorMsg != "invalid credentials" {
		t.Errorf("ErrorMsg = %q", got.ErrorMsg)
	}
}

// TestList_FiltersByActionPrefix ensures the prefix filter scopes
// results correctly — the SPA's category pills depend on this.
func TestList_FiltersByActionPrefix(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	for _, action := range []string{"vm.provision", "vm.delete", "node.cordon", "auth.login"} {
		svc.Record(context.Background(), audit.Event{Action: action, Success: true})
	}

	cases := []struct {
		prefix string
		want   int
	}{
		{"", 4},
		{"vm.", 2},
		{"node.", 1},
		{"auth.", 1},
		{"missing.", 0},
	}
	for _, c := range cases {
		t.Run(c.prefix, func(t *testing.T) {
			res, err := svc.List(context.Background(), audit.ListFilter{ActionPrefix: c.prefix})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(res.Events) != c.want {
				t.Errorf("prefix %q: got %d events, want %d", c.prefix, len(res.Events), c.want)
			}
		})
	}
}

// TestPrune_DeletesOldEvents verifies the reaper drops rows past the
// retention horizon and leaves recent ones alone. Backdates one row
// via a direct UPDATE since CreatedAt is set inside Record.
func TestPrune_DeletesOldEvents(t *testing.T) {
	t.Parallel()
	svc, dbConn := newTestService(t)

	svc.Record(context.Background(), audit.Event{Action: "vm.provision", Success: true})
	svc.Record(context.Background(), audit.Event{Action: "vm.delete", Success: true})

	// Backdate the first row 100 days into the past.
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	if err := dbConn.Model(&db.AuditEvent{}).Where("action = ?", "vm.provision").
		Update("created_at", old).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}

	pruned, err := svc.Prune(context.Background(), 90*24*time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	res, err := svc.List(context.Background(), audit.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Action != "vm.delete" {
		t.Errorf("after prune got %d events; want vm.delete only", len(res.Events))
	}
}

// TestPrune_ZeroAgeIsNoOp — operators who want infinite retention set
// retention=0; Prune must short-circuit rather than nuke the table.
func TestPrune_ZeroAgeIsNoOp(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	svc.Record(context.Background(), audit.Event{Action: "vm.provision", Success: true})

	pruned, err := svc.Prune(context.Background(), 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 for zero maxAge", pruned)
	}
}

// newTestService spins up a fresh in-memory SQLite + audit.Service.
func newTestService(t *testing.T) (*audit.Service, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	dbConn, err := db.New(dbPath, &db.AuditEvent{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return audit.New(dbConn.DB), dbConn
}
