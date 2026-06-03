package reconciler

import (
	"context"
	"fmt"
	"time"

	"nimbus/internal/db"
)

// ListFilter narrows the divergence query. Empty values are wildcards.
type ListFilter struct {
	// Type filters to one divergence category. Empty = all categories.
	Type string
	// Status filters by resolution state. Allowed: "open" (resolved_at IS NULL),
	// "resolved" (NOT NULL), "" / "all" (no filter).
	Status string
	// Since / Until bound detected_at. Zero values are open intervals.
	Since time.Time
	Until time.Time
	// Limit caps the row count. <= 0 falls back to defaultListLimit;
	// > maxListLimit is clamped to maxListLimit.
	Limit int
	// Offset is the row offset for pagination.
	Offset int
}

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// ListResult pairs a page of rows with the total open-divergence count
// — the SPA renders pagination chrome against Total.
type ListResult struct {
	Rows  []db.VMDivergence
	Total int64
}

// List returns a page of divergence rows matching the filter, newest
// detected_at first. The total count is unfiltered by pagination but
// respects every other filter — drives the "X of Y" hint in the UI.
func (r *Reconciler) List(ctx context.Context, f ListFilter) (ListResult, error) {
	q := r.dbConn.WithContext(ctx).Model(&db.VMDivergence{})
	if f.Type != "" {
		q = q.Where("type = ?", f.Type)
	}
	switch f.Status {
	case "open":
		q = q.Where("resolved_at IS NULL")
	case "resolved":
		q = q.Where("resolved_at IS NOT NULL")
	}
	if !f.Since.IsZero() {
		q = q.Where("detected_at >= ?", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		q = q.Where("detected_at <= ?", f.Until.UTC())
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return ListResult{}, fmt.Errorf("count divergences: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var rows []db.VMDivergence
	if err := q.Order("detected_at DESC").
		Limit(limit).
		Offset(f.Offset).
		Find(&rows).Error; err != nil {
		return ListResult{}, fmt.Errorf("list divergences: %w", err)
	}
	return ListResult{Rows: rows, Total: total}, nil
}

// Summary is the aggregate snapshot the dashboard widget consumes. All
// counters refer to open (resolved_at IS NULL) divergences.
type Summary struct {
	Open                 int64      `json:"open"`
	Orphaned             int64      `json:"orphaned"`
	ExternalUnmanaged    int64      `json:"external_unmanaged"`
	ExternalTaggedOrphan int64      `json:"external_tagged_orphan"`
	VMIDMismatch         int64      `json:"vmid_mismatch"`
	LastDetectedAt       *time.Time `json:"last_detected_at,omitempty"`
}

// SummaryFor returns counts per divergence category and the wall-clock
// of the most recent observation. Drives the Sync Status panel on the
// dashboard.
func (r *Reconciler) SummaryFor(ctx context.Context) (Summary, error) {
	q := r.dbConn.WithContext(ctx).Model(&db.VMDivergence{}).Where("resolved_at IS NULL")
	var rows []struct {
		Type           string
		Count          int64
		LastDetectedAt *time.Time
	}
	if err := q.Select("type, COUNT(*) as count, MAX(detected_at) as last_detected_at").
		Group("type").
		Find(&rows).Error; err != nil {
		return Summary{}, fmt.Errorf("aggregate divergences: %w", err)
	}
	out := Summary{}
	for _, row := range rows {
		out.Open += row.Count
		switch row.Type {
		case TypeOrphaned:
			out.Orphaned = row.Count
		case TypeExternalUnmanaged:
			out.ExternalUnmanaged = row.Count
		case TypeExternalTaggedOrphan:
			out.ExternalTaggedOrphan = row.Count
		case TypeVMIDMismatch:
			out.VMIDMismatch = row.Count
		}
		if row.LastDetectedAt != nil && (out.LastDetectedAt == nil || row.LastDetectedAt.After(*out.LastDetectedAt)) {
			out.LastDetectedAt = row.LastDetectedAt
		}
	}
	return out, nil
}
