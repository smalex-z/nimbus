package handlers

import (
	"net/http"
	"strconv"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/reconciler"
)

// Divergences exposes the read-only divergence inventory the EPIC #296
// reconciler populates. Write side (admin actions to resolve a
// divergence) lives in #300 and is mounted separately.
type Divergences struct {
	svc *reconciler.Reconciler
}

// NewDivergences constructs the handler. Nil svc renders empty results
// — the router still mounts the routes so the SPA's network probes
// don't 404 on instances where the divergence loop is disabled.
func NewDivergences(svc *reconciler.Reconciler) *Divergences {
	return &Divergences{svc: svc}
}

// divergenceView is the wire shape per row. Lifts db.VMDivergence into
// a stable snake_case JSON shape that doesn't change when the model
// gains a column.
type divergenceView struct {
	ID              uint    `json:"id"`
	Type            string  `json:"type"`
	NimbusID        string  `json:"nimbus_id,omitempty"`
	VMID            int     `json:"vmid,omitempty"`
	Node            string  `json:"node,omitempty"`
	Hostname        string  `json:"hostname,omitempty"`
	DetectedAt      string  `json:"detected_at"`
	FirstDetectedAt string  `json:"first_detected_at"`
	ResolvedAt      *string `json:"resolved_at,omitempty"`
	ResolvedBy      string  `json:"resolved_by,omitempty"`
	ResolvedAction  string  `json:"resolved_action,omitempty"`
	ActionHint      string  `json:"action_hint"`
	DetailsJSON     string  `json:"details_json,omitempty"`
}

// divergenceListResponse is the wire shape returned by GET
// /api/admin/divergences. Mirrors reconciler.ListResult with the
// per-row view embedded.
type divergenceListResponse struct {
	Divergences []divergenceView `json:"divergences"`
	Total       int64            `json:"total"`
}

// List handles GET /api/admin/divergences. Admin-only; returns the
// most recent open divergences by default. Filterable by type and
// status; paginated via limit+offset.
//
// @Summary     List VM divergences (admin)
// @Description Read-only inventory of mismatches between the Nimbus DB
// @Description and the Proxmox cluster snapshot. By default returns
// @Description open (unresolved) divergences newest-first. Filter by
// @Description category via the `type` parameter; switch to historical
// @Description view with `status=resolved` or `status=all`.
// @Tags        divergences
// @Security    cookieAuth
// @Produce     json
// @Param       type   query string false "filter by category" Enums(orphaned, external-unmanaged, external-tagged-orphan, vmid-mismatch)
// @Param       status query string false "filter by resolution state" Enums(open, resolved, all)
// @Param       limit  query int    false "page size (1-500, default 100)"
// @Param       offset query int    false "row offset for pagination"
// @Success     200 {object} EnvelopeOK{data=divergenceListResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /admin/divergences [get]
func (h *Divergences) List(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		response.Success(w, divergenceListResponse{Divergences: []divergenceView{}, Total: 0})
		return
	}
	q := r.URL.Query()
	filter := reconciler.ListFilter{
		Type:   q.Get("type"),
		Status: q.Get("status"),
	}
	if filter.Status == "" {
		filter.Status = "open"
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			response.BadRequest(w, "limit must be an integer")
			return
		}
		filter.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			response.BadRequest(w, "offset must be an integer")
			return
		}
		filter.Offset = n
	}
	res, err := h.svc.List(r.Context(), filter)
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	out := divergenceListResponse{
		Divergences: make([]divergenceView, 0, len(res.Rows)),
		Total:       res.Total,
	}
	for _, row := range res.Rows {
		out.Divergences = append(out.Divergences, toDivergenceView(row))
	}
	response.Success(w, out)
}

// Summary handles GET /api/admin/divergences/summary. Returns
// aggregate counters per category plus the most recent detection
// timestamp — the data the dashboard's Sync Status widget needs.
//
// @Summary     Divergence aggregate summary (admin)
// @Description Counts of unresolved divergences by category, plus the
// @Description latest detection timestamp. Suitable for the dashboard
// @Description widget's 10s poll interval.
// @Tags        divergences
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=reconciler.Summary}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /admin/divergences/summary [get]
func (h *Divergences) Summary(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		response.Success(w, reconciler.Summary{})
		return
	}
	s, err := h.svc.SummaryFor(r.Context())
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	response.Success(w, s)
}

// toDivergenceView projects a db.VMDivergence into the wire view.
// Stamped one-line so the conversion stays trivially inlineable —
// any computed labels (humanized "5 minutes ago", action chips, etc.)
// belong on the frontend.
func toDivergenceView(row db.VMDivergence) divergenceView {
	v := divergenceView{
		ID:              row.ID,
		Type:            row.Type,
		NimbusID:        row.NimbusID,
		VMID:            row.VMID,
		Node:            row.Node,
		Hostname:        row.Hostname,
		DetectedAt:      row.DetectedAt.UTC().Format(time.RFC3339),
		FirstDetectedAt: row.FirstDetectedAt.UTC().Format(time.RFC3339),
		ResolvedBy:      row.ResolvedBy,
		ResolvedAction:  row.ResolvedAction,
		ActionHint:      ActionHint(row.Type),
		DetailsJSON:     row.DetailsJSON,
	}
	if row.ResolvedAt != nil {
		s := row.ResolvedAt.UTC().Format(time.RFC3339)
		v.ResolvedAt = &s
	}
	return v
}

// ActionHint returns the suggested resolution for a divergence type,
// surfaced to the SPA so the detail page can render the right CTA chip.
// Mirrors the action_hint string the reconciler logs.
func ActionHint(dtype string) string {
	switch dtype {
	case reconciler.TypeOrphaned:
		return "mark-deleted-or-restore"
	case reconciler.TypeExternalUnmanaged:
		return "import-or-ignore"
	case reconciler.TypeExternalTaggedOrphan:
		return "adopt-or-force-delete"
	case reconciler.TypeVMIDMismatch:
		return "investigate-recycle"
	}
	return "investigate"
}
