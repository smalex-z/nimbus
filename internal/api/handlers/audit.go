package handlers

import (
	"net/http"
	"strconv"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
)

// Audit serves the read-only Infrastructure → Audit log surface. The
// write side is the audit.Service that other handlers call inline at
// every mutation; this handler only exposes the list endpoint.
type Audit struct {
	svc *audit.Service
}

// NewAudit constructs the handler. Nil svc renders an empty list — the
// router still mounts the route so the SPA's network probe doesn't 404.
func NewAudit(svc *audit.Service) *Audit {
	return &Audit{svc: svc}
}

// auditListResponse is the wire shape returned by GET /api/audit.
// Mirrors audit.ListResult but uses snake_case for JSON.
type auditListResponse struct {
	Events []auditEventView `json:"events"`
	Total  int64            `json:"total"`
}

// auditEventView is the per-row payload. Lifts db.AuditEvent into a
// stable wire shape so model-side schema bumps don't surface as SPA
// breakage. The fields are 1:1 with the model today; the layer exists
// so a future detail-explosion (e.g. computed time-relative labels,
// joined actor display names) doesn't churn the model.
type auditEventView struct {
	ID          uint   `json:"id"`
	CreatedAt   string `json:"created_at"`
	ActorID     *uint  `json:"actor_id,omitempty"`
	ActorEmail  string `json:"actor_email,omitempty"`
	ActorAdmin  bool   `json:"actor_admin"`
	Action      string `json:"action"`
	TargetType  string `json:"target_type,omitempty"`
	TargetID    string `json:"target_id,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
	IPAddress   string `json:"ip_address,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Success     bool   `json:"success"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	DetailsJSON string `json:"details_json,omitempty"`
}

// List handles GET /api/audit. Admin-only; returns the most recent
// events newest-first, filterable by actor, action prefix, and time
// range. Pagination via offset+limit (limit defaults to 100, capped at
// 500). Total count is returned alongside so the SPA can render
// pagination chrome.
//
// @Summary     List audit events (admin)
// @Description Read-only inspection of the cluster audit log. Newest
// @Description first; filterable by actor, action prefix, and time
// @Description range. Use the prefix form to scope by domain — e.g.
// @Description `action_prefix=vm.` matches every VM event;
// @Description `action_prefix=settings.` matches all settings updates.
// @Tags        audit
// @Security    cookieAuth
// @Produce     json
// @Param       actor_id      query int    false "filter by actor user id"
// @Param       action_prefix query string false "filter by action prefix (e.g. `vm.`, `settings.`)"
// @Param       since         query string false "RFC3339 lower bound on created_at"
// @Param       until         query string false "RFC3339 upper bound on created_at"
// @Param       limit         query int    false "page size (1-500, default 100)"
// @Param       offset        query int    false "row offset for pagination"
// @Success     200 {object} EnvelopeOK{data=auditListResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /audit [get]
func (h *Audit) List(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		response.Success(w, auditListResponse{Events: []auditEventView{}, Total: 0})
		return
	}
	q := r.URL.Query()
	filter := audit.ListFilter{
		ActionPrefix: q.Get("action_prefix"),
	}
	if v := q.Get("actor_id"); v != "" {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			response.BadRequest(w, "actor_id must be a positive integer")
			return
		}
		castID := uint(id)
		filter.ActorID = &castID
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			response.BadRequest(w, "since must be RFC3339 (e.g. 2026-01-01T00:00:00Z)")
			return
		}
		filter.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			response.BadRequest(w, "until must be RFC3339")
			return
		}
		filter.Until = t
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
	out := auditListResponse{
		Events: make([]auditEventView, 0, len(res.Events)),
		Total:  res.Total,
	}
	for _, e := range res.Events {
		out.Events = append(out.Events, auditEventView{
			ID:          e.ID,
			CreatedAt:   e.CreatedAt.Format(time.RFC3339),
			ActorID:     e.ActorID,
			ActorEmail:  e.ActorEmail,
			ActorAdmin:  e.ActorAdmin,
			Action:      e.Action,
			TargetType:  e.TargetType,
			TargetID:    e.TargetID,
			TargetLabel: e.TargetLabel,
			IPAddress:   e.IPAddress,
			RequestID:   e.RequestID,
			Success:     e.Success,
			ErrorMsg:    e.ErrorMsg,
			DetailsJSON: e.DetailsJSON,
		})
	}
	response.Success(w, out)
}
