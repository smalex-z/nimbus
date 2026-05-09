// Package operations is the background-task registry. Long-running
// flows that used to block an HTTP request — migrate, provision, and
// any future similar work — Create an Operation row, fire a goroutine,
// and return immediately. The SPA polls Get/List to track progress and
// re-attach when the operator opens a new tab.
//
// Design notes:
//
//   - The DB row is the source of truth. Goroutines update it via
//     UpdateMessage / Finish; reads always come from the row, never
//     from in-memory state. This means a Nimbus restart loses the
//     work-in-progress (the goroutine dies) but the row survives,
//     and the startup ReapStuck() flips long-orphaned `running` rows
//     to `failed` so the SPA isn't lying about ghost progress.
//
//   - No cancel-from-outside in v1. The work is in a goroutine; the
//     orchestrator package (provision, etc.) decides whether to
//     respect ctx cancellation. We can layer a cancel registry on top
//     once the framework has shipped and we know which kinds of cancel
//     semantics matter (preempt-immediately vs. wait-for-checkpoint).
//
//   - Audit + operations are sibling concepts, not parent/child.
//     Operations track *progress*; audit tracks *the act of starting*.
//     Both rows get written: audit on dispatch, operation row across
//     the lifecycle. Joining is by request_id (audit) ↔ id (op) when
//     needed; no foreign key — keeps the two tables independently
//     prunable.
package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
)

// State enum — the canonical strings stored in db.Operation.State.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// IsTerminal reports whether the state is one a row will never leave.
// Used by readers (SPA poll, reaper) that want to stop watching.
func IsTerminal(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Service is the operations registry. Safe for concurrent use; all
// writes go through gorm and the underlying SQLite single-writer
// serializes them.
type Service struct {
	db *gorm.DB
}

// New constructs a Service. nil database is allowed (handlers that
// haven't been migrated yet still build) — every method on a nil
// receiver is a no-op or a benign zero return.
func New(database *gorm.DB) *Service {
	return &Service{db: database}
}

// CreateInput is the caller-supplied half of a new operation. The
// service stamps in actor + timestamps from ctx + the clock.
type CreateInput struct {
	// Type is the dotted identifier (`vm.migrate`, `vm.provision`).
	// Mirrors the audit Action vocabulary so filters compose.
	Type string
	// Target is what's being acted on. ID is a string so VMIDs and
	// node names share one column. Label is the human display value.
	TargetType  string
	TargetID    string
	TargetLabel string
	// Message is the initial status text shown in the toolbar
	// dropdown until the goroutine bumps it.
	Message string
	// Details, when non-nil, is JSON-marshalled into DetailsJSON.
	// The same conservative fallback as audit.Service: if marshal
	// fails, the operation is still created with empty details
	// rather than dropped.
	Details any
}

// Create inserts a new operation in StateQueued and returns the
// hydrated row. The caller is responsible for transitioning to
// running via Start.
func (s *Service) Create(ctx context.Context, in CreateInput) (*db.Operation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("operations.Service is nil")
	}
	if strings.TrimSpace(in.Type) == "" {
		return nil, errors.New("operation type is required")
	}
	now := time.Now().UTC()
	row := db.Operation{
		Type:            in.Type,
		State:           StateQueued,
		TargetType:      in.TargetType,
		TargetID:        in.TargetID,
		TargetLabel:     in.TargetLabel,
		Message:         in.Message,
		LastHeartbeatAt: now,
	}
	if user := ctxutil.User(ctx); user != nil {
		id := user.ID
		row.ActorID = &id
		row.ActorEmail = user.Email
	}
	if in.Details != nil {
		if buf, err := json.Marshal(in.Details); err == nil {
			row.DetailsJSON = string(buf)
		}
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}
	return &row, nil
}

// Start flips queued → running and stamps StartedAt. Idempotent — a
// row already past queued is left unchanged so the goroutine can
// safely call Start at the top of its work even when the dispatcher
// already did.
func (s *Service) Start(ctx context.Context, id uint) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&db.Operation{}).
		Where("id = ? AND state = ?", id, StateQueued).
		Updates(map[string]any{
			"state":             StateRunning,
			"started_at":        now,
			"last_heartbeat_at": now,
		}).Error
}

// UpdateMessage sets the human-readable status text and bumps the
// heartbeat. Called frequently by the work goroutine to surface
// progress in the SPA poll. State must be StateRunning — terminal
// rows are immutable.
func (s *Service) UpdateMessage(ctx context.Context, id uint, msg string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).Model(&db.Operation{}).
		Where("id = ? AND state = ?", id, StateRunning).
		Updates(map[string]any{
			"message":           msg,
			"last_heartbeat_at": time.Now().UTC(),
		}).Error
}

// UpdateDetails writes the JSON details blob (the op-specific
// structured payload). Used by consumers like migrate to surface a
// failure_code + reason that the SPA dispatches on. Distinct from
// UpdateMessage — message is human-readable text; details is the
// machine-readable structured shape. Both can be set independently.
func (s *Service) UpdateDetails(ctx context.Context, id uint, detailsJSON string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).Model(&db.Operation{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"details_json":      detailsJSON,
			"last_heartbeat_at": time.Now().UTC(),
		}).Error
}

// Finish writes a terminal state. message is preserved as the final
// status (success summary or error reason). Idempotent — calling
// Finish on an already-terminal row no-ops so the goroutine doesn't
// have to track which exit branch ran.
func (s *Service) Finish(ctx context.Context, id uint, state, message string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if !IsTerminal(state) {
		return fmt.Errorf("Finish: %q is not a terminal state", state)
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&db.Operation{}).
		Where("id = ? AND state IN ?", id, []string{StateQueued, StateRunning}).
		Updates(map[string]any{
			"state":             state,
			"message":           message,
			"finished_at":       now,
			"last_heartbeat_at": now,
		}).Error
}

// Get returns one operation by id.
func (s *Service) Get(ctx context.Context, id uint) (*db.Operation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("operations.Service is nil")
	}
	var row db.Operation
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// ListFilter narrows the List() result. Zero values are no-ops.
type ListFilter struct {
	// State filters by exact match (e.g. "running"). Empty = any.
	State string
	// Type filters by exact match (e.g. "vm.migrate"). Empty = any.
	Type string
	// ActorID, when non-nil, restricts to operations started by
	// that user.
	ActorID *uint
	// IncludeFinished, when false, restricts to non-terminal rows.
	// The default — most SPA calls only care about the in-flight
	// view.
	IncludeFinished bool
	// Limit caps the number of rows returned. <=0 falls back to 100.
	// Capped at 500 to match audit pagination.
	Limit int
}

// List returns operations newest-first matching f. The total count is
// the post-filter count for SPA pagination chrome (mirrors the audit
// list shape — readers get one tested pattern instead of two).
func (s *Service) List(ctx context.Context, f ListFilter) ([]db.Operation, int64, error) {
	if s == nil || s.db == nil {
		return nil, 0, nil
	}
	q := s.db.WithContext(ctx).Model(&db.Operation{})
	if f.State != "" {
		q = q.Where("state = ?", f.State)
	}
	if f.Type != "" {
		q = q.Where("type = ?", f.Type)
	}
	if f.ActorID != nil {
		q = q.Where("actor_id = ?", *f.ActorID)
	}
	if !f.IncludeFinished {
		q = q.Where("state IN ?", []string{StateQueued, StateRunning})
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count operations: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	var rows []db.Operation
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("list operations: %w", err)
	}
	return rows, total, nil
}

// ReapStuck flips any non-terminal operation whose last heartbeat is
// older than maxAge to StateFailed. Called from main.go on startup so
// rows orphaned by a Nimbus restart mid-run don't pretend to still be
// running. Returns the number of rows reaped.
//
// One hour is a reasonable default — provision can take 10 min,
// migrate can run 30+ for a busy VM. Tune via main.go if needed.
func (s *Service) ReapStuck(ctx context.Context, maxAge time.Duration) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	res := s.db.WithContext(ctx).Model(&db.Operation{}).
		Where("state IN ? AND last_heartbeat_at < ?",
			[]string{StateQueued, StateRunning}, cutoff).
		Updates(map[string]any{
			"state":       StateFailed,
			"message":     "abandoned: nimbus restarted while running",
			"finished_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("reap stuck operations: %w", res.Error)
	}
	return res.RowsAffected, nil
}
