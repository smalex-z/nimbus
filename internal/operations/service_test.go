package operations_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nimbus/internal/db"
	"nimbus/internal/operations"
)

func newTestService(t *testing.T) (*operations.Service, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops.db")
	database, err := db.New(path, &db.User{}, &db.Operation{})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	return operations.New(database.DB), database
}

// TestCreate_PersistsRowAndReturnsID covers the happy create path.
// Defaults (state=queued, last_heartbeat_at != zero) match the
// post-Create invariant the rest of the framework depends on.
func TestCreate_PersistsRowAndReturnsID(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	op, err := svc.Create(context.Background(), operations.CreateInput{
		Type:        "vm.migrate",
		TargetType:  "vm",
		TargetID:    "100",
		TargetLabel: "web-01",
		Message:     "queued",
		Details:     map[string]any{"target_node": "beta"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.ID == 0 {
		t.Errorf("Create returned id=0")
	}
	if op.State != operations.StateQueued {
		t.Errorf("State = %q, want queued", op.State)
	}
	if op.LastHeartbeatAt.IsZero() {
		t.Errorf("LastHeartbeatAt is zero — heartbeat invariant broken")
	}
	if op.DetailsJSON == "" {
		t.Errorf("Details serialised to empty string")
	}
}

// TestLifecycle_QueuedThroughTerminal walks one operation through the
// canonical state machine and asserts each transition writes the
// expected fields.
func TestLifecycle_QueuedThroughTerminal(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	op, err := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Start(ctx, op.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, _ := svc.Get(ctx, op.ID)
	if got.State != operations.StateRunning {
		t.Errorf("after Start, state = %q, want running", got.State)
	}
	if got.StartedAt == nil {
		t.Errorf("StartedAt should be populated after Start")
	}

	if err := svc.UpdateMessage(ctx, op.ID, "step 2 of 5"); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	got, _ = svc.Get(ctx, op.ID)
	if got.Message != "step 2 of 5" {
		t.Errorf("Message = %q, want %q", got.Message, "step 2 of 5")
	}

	if err := svc.Finish(ctx, op.ID, operations.StateSucceeded, "done"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _ = svc.Get(ctx, op.ID)
	if got.State != operations.StateSucceeded {
		t.Errorf("after Finish, state = %q, want succeeded", got.State)
	}
	if got.FinishedAt == nil {
		t.Errorf("FinishedAt should be populated after Finish")
	}
}

// TestUpdateMessage_OnTerminalRow_NoOps covers the immutability
// guarantee: a goroutine that races a Finish call with a final
// UpdateMessage must not overwrite the terminal state.
func TestUpdateMessage_OnTerminalRow_NoOps(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	op, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	_ = svc.Start(ctx, op.ID)
	_ = svc.Finish(ctx, op.ID, operations.StateSucceeded, "done")

	if err := svc.UpdateMessage(ctx, op.ID, "stale write"); err != nil {
		t.Fatalf("UpdateMessage on terminal row should silently no-op, got: %v", err)
	}
	got, _ := svc.Get(ctx, op.ID)
	if got.Message != "done" {
		t.Errorf("Message overwritten on terminal row: got %q, want %q", got.Message, "done")
	}
}

// TestList_DefaultExcludesTerminal covers the toolbar-dropdown poll:
// the default (IncludeFinished=false) returns only in-flight rows
// so the SPA's count badge is meaningful.
func TestList_DefaultExcludesTerminal(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	running, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	_ = svc.Start(ctx, running.ID)

	done, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	_ = svc.Start(ctx, done.ID)
	_ = svc.Finish(ctx, done.ID, operations.StateSucceeded, "done")

	rows, total, err := svc.List(ctx, operations.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("default list size = %d/%d, want 1/1 (only running)", len(rows), total)
	}
	if rows[0].ID != running.ID {
		t.Errorf("list returned id=%d, want running id=%d", rows[0].ID, running.ID)
	}

	rows, total, err = svc.List(ctx, operations.ListFilter{IncludeFinished: true})
	if err != nil {
		t.Fatalf("List include_finished: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("include_finished list size = %d/%d, want 2/2", len(rows), total)
	}
}

// TestReapStuck_FlipsLongRunningRowsToFailed covers the startup-time
// recovery: rows whose work goroutine died with a previous nimbus
// process get marked failed so the SPA's poll observes a final state
// rather than waiting forever.
func TestReapStuck_FlipsLongRunningRowsToFailed(t *testing.T) {
	t.Parallel()
	svc, database := newTestService(t)
	ctx := context.Background()

	op, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	_ = svc.Start(ctx, op.ID)
	// Backdate heartbeat so the reaper sees it as stuck.
	stale := time.Now().Add(-2 * time.Hour)
	if err := database.DB.Model(&db.Operation{}).
		Where("id = ?", op.ID).
		Update("last_heartbeat_at", stale).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Recently-updated row should NOT be reaped.
	fresh, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	_ = svc.Start(ctx, fresh.ID)

	reaped, err := svc.ReapStuck(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ReapStuck: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped %d, want 1", reaped)
	}

	stuckRow, _ := svc.Get(ctx, op.ID)
	if stuckRow.State != operations.StateFailed {
		t.Errorf("stuck op state = %q, want failed", stuckRow.State)
	}

	freshRow, _ := svc.Get(ctx, fresh.ID)
	if freshRow.State != operations.StateRunning {
		t.Errorf("fresh op state = %q, want running (must not be reaped)", freshRow.State)
	}
}

// TestUpdateDetails_SetsJSONBlob covers the details-update path that
// the migrate handler uses to surface failure_code on terminal failure.
func TestUpdateDetails_SetsJSONBlob(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	ctx := context.Background()

	op, _ := svc.Create(ctx, operations.CreateInput{Type: "vm.migrate"})
	if err := svc.UpdateDetails(ctx, op.ID, `{"failure_code":"online_migration_failed"}`); err != nil {
		t.Fatalf("UpdateDetails: %v", err)
	}
	got, _ := svc.Get(ctx, op.ID)
	if got.DetailsJSON != `{"failure_code":"online_migration_failed"}` {
		t.Errorf("DetailsJSON = %q, want failure_code blob", got.DetailsJSON)
	}
}
