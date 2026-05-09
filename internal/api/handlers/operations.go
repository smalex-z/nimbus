package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/operations"
)

// Operations serves the read-only background-tasks surface. Writes
// happen inline in the handlers that *start* operations
// (Cluster.MigrateVM etc.) — this handler only exposes list + detail
// endpoints the SPA polls.
type Operations struct {
	svc *operations.Service
}

// NewOperations constructs the handler. Nil svc is allowed — every
// route returns an empty payload so the SPA's poll doesn't 500 on a
// fresh-deploy DB that hasn't seen its first operation yet.
func NewOperations(svc *operations.Service) *Operations {
	return &Operations{svc: svc}
}

// operationsListResponse is the wire shape returned by GET /api/operations.
type operationsListResponse struct {
	Operations []operationView `json:"operations"`
	Total      int64           `json:"total"`
}

// operationView is the per-row payload. Mirrors db.Operation but
// renders timestamps as RFC3339 strings so the SPA never has to think
// about zone offsets.
type operationView struct {
	ID              uint   `json:"id"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	Type            string `json:"type"`
	State           string `json:"state"`
	ActorID         *uint  `json:"actor_id,omitempty"`
	ActorEmail      string `json:"actor_email,omitempty"`
	TargetType      string `json:"target_type,omitempty"`
	TargetID        string `json:"target_id,omitempty"`
	TargetLabel     string `json:"target_label,omitempty"`
	Message         string `json:"message,omitempty"`
	DetailsJSON     string `json:"details_json,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
}

// List handles GET /api/operations. Verified-user; non-admins see
// only their own operations (admins see everything).
//
// Default narrows to non-terminal rows so the toolbar dropdown's
// poll has a small payload. Pass `include_finished=1` for the
// full Activity-page view.
//
// @Summary     List background operations
// @Description Long-running tasks (migrate, provision, ...) start
// @Description Operation rows that survive the originating HTTP
// @Description request. Default narrows to in-flight; pass
// @Description include_finished=1 for the historical view.
// @Tags        operations
// @Security    cookieAuth
// @Produce     json
// @Param       state            query string false "filter by state" Enums(queued, running, succeeded, failed, cancelled)
// @Param       type             query string false "filter by operation type (e.g. vm.migrate)"
// @Param       include_finished query int    false "include terminal rows (0/1, default 0)"
// @Param       limit            query int    false "page size (1-500, default 100)"
// @Success     200 {object} EnvelopeOK{data=operationsListResponse}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /operations [get]
func (h *Operations) List(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		response.Success(w, operationsListResponse{Operations: []operationView{}})
		return
	}
	q := r.URL.Query()
	filter := operations.ListFilter{
		State: q.Get("state"),
		Type:  q.Get("type"),
	}
	if v := q.Get("include_finished"); v == "1" || v == "true" {
		filter.IncludeFinished = true
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			response.BadRequest(w, "limit must be an integer")
			return
		}
		filter.Limit = n
	}
	// Visibility: non-admins only see their own ops. Admins see
	// everything — they're the ones running migrations cluster-wide
	// and need the full picture in the toolbar dropdown.
	user := ctxutil.User(r.Context())
	if user != nil && !user.IsAdmin {
		id := user.ID
		filter.ActorID = &id
	}

	rows, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	out := operationsListResponse{
		Operations: make([]operationView, 0, len(rows)),
		Total:      total,
	}
	for _, op := range rows {
		out.Operations = append(out.Operations, toView(op))
	}
	response.Success(w, out)
}

// Get handles GET /api/operations/{id}. Verified-user; non-admins
// only see their own rows (404 otherwise).
//
// @Summary     Read one background operation
// @Tags        operations
// @Security    cookieAuth
// @Produce     json
// @Param       id   path int true "operation id"
// @Success     200 {object} EnvelopeOK{data=operationView}
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /operations/{id} [get]
func (h *Operations) Get(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		response.NotFound(w, "operation not found")
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	row, err := h.svc.Get(r.Context(), uint(id))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.NotFound(w, "operation not found")
			return
		}
		response.InternalError(w, err.Error())
		return
	}
	user := ctxutil.User(r.Context())
	if user != nil && !user.IsAdmin {
		// Hide other users' operations behind a 404 — same shape
		// as the DB-miss path so callers can't probe for ids.
		if row.ActorID == nil || *row.ActorID != user.ID {
			response.NotFound(w, "operation not found")
			return
		}
	}
	response.Success(w, toView(*row))
}

// toView lifts the model row into the wire shape with stringified
// timestamps. Optional times (StartedAt, FinishedAt) collapse to
// empty string when nil so the SPA's `started_at?:` field stays
// idiomatic.
func toView(op db.Operation) operationView {
	v := operationView{
		ID:              op.ID,
		CreatedAt:       op.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       op.UpdatedAt.Format(time.RFC3339),
		Type:            op.Type,
		State:           op.State,
		ActorID:         op.ActorID,
		ActorEmail:      op.ActorEmail,
		TargetType:      op.TargetType,
		TargetID:        op.TargetID,
		TargetLabel:     op.TargetLabel,
		Message:         op.Message,
		DetailsJSON:     op.DetailsJSON,
		LastHeartbeatAt: op.LastHeartbeatAt.Format(time.RFC3339),
	}
	if op.StartedAt != nil {
		v.StartedAt = op.StartedAt.Format(time.RFC3339)
	}
	if op.FinishedAt != nil {
		v.FinishedAt = op.FinishedAt.Format(time.RFC3339)
	}
	return v
}
