// Package audit owns the write-side audit log: a single SQLite table
// (db.AuditEvent) populated by every service that mutates cluster
// state, surfaced read-only on Infrastructure → Audit log.
//
// The package exposes three calls:
//
//   - Record  — fire-and-forget insert. Service-layer code calls this
//     at every meaningful write boundary (provision a VM, save a
//     setting, log in). Failures are logged but never propagate; an
//     audit-write hiccup must not abort the operation it audits.
//   - List    — paginated/filterable read for the SPA's audit page.
//     Filters by actor, action prefix, and time window; orders newest
//     first; returns total count for pagination chrome.
//   - Prune   — TTL reaper; deletes rows older than retention. Driven
//     by a goroutine in cmd/server/main.go; respects
//     NIMBUS_AUDIT_RETENTION_DAYS (default 90).
//
// Action names are dotted identifiers ("vm.provision",
// "settings.smtp.update"). The first component is the domain (vm,
// node, settings, auth, user) so the SPA can render a category filter
// without server-side enum churn. New actions are added free-form;
// readers are tolerant of unknown values.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
)

// Service is the entry point service-layer code uses to record events.
// Concurrency-safe by virtue of GORM's connection pool; the actual
// write goes through the same SQLite single-writer the rest of the app
// uses (one row per event, append-only, no contention).
type Service struct {
	db *gorm.DB
}

// New constructs an audit Service backed by the shared *gorm.DB.
func New(database *gorm.DB) *Service {
	return &Service{db: database}
}

// Event is the input struct callers fill in when recording. The Service
// stamps in the actor + IP + request id from ctx so callers don't have
// to plumb those values themselves.
//
// Action is the dotted identifier (mandatory). Target* describes what
// was acted on (all optional — some actions, like a settings update,
// have no target). Details, when non-nil, is JSON-marshalled and
// stored as DetailsJSON; non-marshallable values fall back to a plain
// string representation rather than dropping the event.
type Event struct {
	Action      string
	TargetType  string
	TargetID    string
	TargetLabel string
	Details     any
	Success     bool
	ErrorMsg    string
}

// Record persists one audit event. Always returns nil — failures are
// logged at warn level but never propagate, so service-layer code can
// call it inline without an error-handling branch:
//
//	defer audit.Record(ctx, audit.Event{Action: "vm.delete", ...})
//
// (defer + a short-circuit on success works fine; the event still
// records the user's intent even if the operation later errored.)
func (s *Service) Record(ctx context.Context, evt Event) {
	if s == nil || s.db == nil {
		return
	}
	if evt.Action == "" {
		log.Printf("audit: dropped event with empty action")
		return
	}
	row := db.AuditEvent{
		Action:      evt.Action,
		TargetType:  evt.TargetType,
		TargetID:    evt.TargetID,
		TargetLabel: evt.TargetLabel,
		Success:     evt.Success,
		ErrorMsg:    evt.ErrorMsg,
		IPAddress:   ctxutil.ClientIP(ctx),
		RequestID:   ctxutil.RequestID(ctx),
	}
	if user := ctxutil.User(ctx); user != nil {
		id := user.ID
		row.ActorID = &id
		row.ActorEmail = user.Email
		row.ActorAdmin = user.IsAdmin
	}
	if evt.Details != nil {
		if encoded, err := json.Marshal(evt.Details); err == nil {
			row.DetailsJSON = string(encoded)
		} else {
			row.DetailsJSON = fmt.Sprintf("%v", evt.Details)
		}
	}
	// CreatedAt is set by GORM via the column tag's index + default
	// CURRENT_TIMESTAMP — but explicit is clearer.
	row.CreatedAt = time.Now().UTC()
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		// Log + drop. Auditing failures must never tank the operation
		// they're auditing — that would be worse than no audit at all.
		log.Printf("audit: record %q failed: %v", evt.Action, err)
	}
}

// ListFilter narrows the result set returned by List. Empty fields are
// no-ops (no constraint). Time bounds use Go's zero-time as "no bound."
type ListFilter struct {
	ActorID      *uint
	ActionPrefix string // "vm." matches all VM actions; "" matches everything
	Since        time.Time
	Until        time.Time
	Limit        int // 0 = default 100; capped at 500
	Offset       int
}

// ListResult bundles the page of events with the total count so the
// SPA can render pagination chrome ("showing 1-100 of 1,243").
type ListResult struct {
	Events []db.AuditEvent
	Total  int64
}

// List returns a filtered page of audit events, newest first. Tolerant
// of malformed filters: the caller's intent is read-only inspection,
// so we coerce/clamp rather than reject.
func (s *Service) List(ctx context.Context, f ListFilter) (*ListResult, error) {
	if s == nil || s.db == nil {
		return &ListResult{}, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	q := s.db.WithContext(ctx).Model(&db.AuditEvent{})
	if f.ActorID != nil {
		q = q.Where("actor_id = ?", *f.ActorID)
	}
	if f.ActionPrefix != "" {
		q = q.Where("action LIKE ?", strings.TrimSuffix(f.ActionPrefix, "%")+"%")
	}
	if !f.Since.IsZero() {
		q = q.Where("created_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("created_at <= ?", f.Until)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("count audit events: %w", err)
	}
	var rows []db.AuditEvent
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	return &ListResult{Events: rows, Total: total}, nil
}

// Prune deletes rows older than (now - maxAge). Returns the number of
// rows removed so callers can log the reaper's effect. A non-positive
// maxAge is a no-op (defensive — operators who want infinite retention
// just set NIMBUS_AUDIT_RETENTION_DAYS=0).
func (s *Service) Prune(ctx context.Context, maxAge time.Duration) (int64, error) {
	if s == nil || s.db == nil || maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	res := s.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&db.AuditEvent{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune audit events: %w", res.Error)
	}
	return res.RowsAffected, nil
}
