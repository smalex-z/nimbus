package nodemgr

import (
	"context"
	"fmt"
	"time"

	"nimbus/internal/db"
)

// validTransition returns nil iff the (from → to) lock-state move is allowed.
// Transitions are intentionally narrow:
//
//	none      → cordoned   (Cordon)
//	cordoned  → none       (Uncordon)
//	none      → draining   (StartDrain — internal, not directly callable)
//	cordoned  → draining   (StartDrain — operator may drain a cordoned node)
//	draining  → drained    (executor flips this on the last successful migration)
//	draining  → cordoned   (drain failed mid-flight; left in cordoned for retry)
//	drained   → none       (Uncordon — operator decided not to remove after all)
//
// Anything else is rejected with ErrInvalidLock so the caller can surface a
// "you can't go there from here" 409.
func validTransition(from, to string) error {
	from = lockOrNone(from)
	to = lockOrNone(to)
	if from == to {
		return nil // no-op moves are accepted (idempotent caller-friendly)
	}
	allowed := map[string]map[string]bool{
		"none":     {"cordoned": true, "draining": true},
		"cordoned": {"none": true, "draining": true},
		"draining": {"drained": true, "cordoned": true},
		"drained":  {"none": true},
	}
	if next, ok := allowed[from]; ok && next[to] {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrInvalidLock, from, to)
}

// CordonRequest carries operator context for an attempted lock change.
// Reason is free-text shown to other operators (visible in /nodes detail).
type CordonRequest struct {
	NodeName string
	Reason   string
	ActorID  uint // user.id of the admin clicking the button
}

// Cordon flips a node from "none" to "cordoned". The scheduler checks
// LockState on every Provision call so the next provision attempt can't
// land here.
//
// Idempotent for callers — recordoning an already-cordoned node returns
// nil but doesn't update LockedAt/LockedBy/LockReason (those reflect the
// initial cordon, not the most recent re-confirmation).
func (s *Service) Cordon(ctx context.Context, req CordonRequest) (*db.Node, error) {
	if s.drainsInFlight[req.NodeName] {
		return nil, ErrDrainInFlight
	}
	row, err := s.loadOrCreate(ctx, req.NodeName)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if err := validTransition(row.LockState, "cordoned"); err != nil {
		return nil, err
	}
	if lockOrNone(row.LockState) == "cordoned" {
		return row, nil // no-op
	}
	now := time.Now().UTC()
	updates := map[string]interface{}{
		"lock_state":  "cordoned",
		"locked_at":   now,
		"locked_by":   req.ActorID,
		"lock_reason": req.Reason,
	}
	if err := s.db.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", req.NodeName).
		Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("cordon node %s: %w", req.NodeName, err)
	}
	return s.loadOrCreate(ctx, req.NodeName)
}

// Uncordon flips back to "none" from cordoned or drained. Clears the
// lock-context fields so a future cordon doesn't surface stale reason text.
//
// Refused if a drain is in flight — the operator must wait for the drain
// to finish (success or failure). Refused on draining state for the same
// reason.
func (s *Service) Uncordon(ctx context.Context, name string) (*db.Node, error) {
	if s.drainsInFlight[name] {
		return nil, ErrDrainInFlight
	}
	row, err := s.loadOrCreate(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if err := validTransition(row.LockState, "none"); err != nil {
		return nil, err
	}
	if lockOrNone(row.LockState) == "none" {
		return row, nil // no-op
	}
	if err := s.db.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", name).
		Updates(map[string]interface{}{
			"lock_state":  "none",
			"locked_at":   nil,
			"locked_by":   nil,
			"lock_reason": "",
		}).Error; err != nil {
		return nil, fmt.Errorf("uncordon node %s: %w", name, err)
	}
	return s.loadOrCreate(ctx, name)
}
