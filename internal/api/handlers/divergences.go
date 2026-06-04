package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/reconciler"
)

// Divergences exposes both the read-only divergence inventory (#299)
// and the admin resolve actions (#300) the EPIC #296 reconciler
// supports.
type Divergences struct {
	svc  *reconciler.Reconciler
	deps reconciler.ResolveDeps
}

// NewDivergences constructs the handler. Nil svc renders empty results
// — the router still mounts the routes so the SPA's network probes
// don't 404 on instances where the divergence loop is disabled.
// Nil deps disables the resolve actions (they return 503) while
// keeping the list/summary endpoints functional.
func NewDivergences(svc *reconciler.Reconciler) *Divergences {
	return &Divergences{svc: svc}
}

// WithResolveDeps wires the side-effect surface admin actions need
// (Proxmox client). Returns the same handler so the constructor can
// chain. Optional — when nil the resolve endpoints 503 cleanly.
func (h *Divergences) WithResolveDeps(deps reconciler.ResolveDeps) *Divergences {
	h.deps = deps
	return h
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

// resolveResponse is the wire shape returned by every resolve action.
// Wraps reconciler.ResolveResult unchanged — the result struct's
// fields are already snake_case and JSON-tagged.
type resolveResponse struct {
	Result reconciler.ResolveResult `json:"result"`
}

// Import handles POST /api/admin/divergences/{id}/import.
//
// @Summary     Import an external Proxmox VM into Nimbus
// @Description Mint a fresh nimbus_id, stamp it onto the Proxmox tag +
// @Description description, and insert a vms row. Tier/OS/IP are
// @Description placeholder values the operator can edit afterwards;
// @Description Nimbus doesn't have enough information to infer them.
// @Tags        divergences
// @Security    cookieAuth
// @Param       id path int true "divergence id"
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=resolveResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /admin/divergences/{id}/import [post]
func (h *Divergences) Import(w http.ResponseWriter, r *http.Request) {
	h.runResolve(w, r, "import", h.svc.ImportExternal)
}

// Adopt handles POST /api/admin/divergences/{id}/adopt.
//
// @Summary     Re-link a tagged orphan with its existing DB row
// @Tags        divergences
// @Security    cookieAuth
// @Param       id path int true "divergence id"
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=resolveResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /admin/divergences/{id}/adopt [post]
func (h *Divergences) Adopt(w http.ResponseWriter, r *http.Request) {
	h.runResolve(w, r, "adopt", h.svc.Adopt)
}

// MarkDeleted handles POST /api/admin/divergences/{id}/mark-deleted.
//
// @Summary     Remove the local DB row for an orphan
// @Tags        divergences
// @Security    cookieAuth
// @Param       id path int true "divergence id"
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=resolveResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /admin/divergences/{id}/mark-deleted [post]
func (h *Divergences) MarkDeleted(w http.ResponseWriter, r *http.Request) {
	h.runResolve(w, r, "mark-deleted", h.svc.MarkDeleted)
}

// ForceDelete handles DELETE /api/admin/divergences/{id}?force=true.
// The force=true query parameter is mandatory — the destructive nature
// of "destroy a Proxmox VM" warrants an explicit flag that can't be
// hit by an accidental DELETE.
//
// @Summary     Destroy the Proxmox VM and drop the local row
// @Description Requires the literal query string `force=true`. Stops +
// @Description destroys the PVE VM and removes any local DB row
// @Description pointing at the same identity. Idempotent — "already
// @Description gone" is treated as success.
// @Tags        divergences
// @Security    cookieAuth
// @Param       id    path  int    true  "divergence id"
// @Param       force query string true  "must equal 'true' to confirm" Enums(true)
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=resolveResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /admin/divergences/{id} [delete]
func (h *Divergences) ForceDelete(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("force") != "true" {
		response.BadRequest(w, "force=true is required to confirm this destructive action")
		return
	}
	h.runResolve(w, r, "force-delete", h.svc.ForceDelete)
}

// resolveFn is the signature shared by every action method on the
// reconciler. Lifting the common preamble (id parse, dep check, actor
// extraction, error mapping) here keeps each handler one line.
type resolveFn func(ctx context.Context, divID uint, deps reconciler.ResolveDeps, actor reconciler.ResolveActor) (reconciler.ResolveResult, error)

func (h *Divergences) runResolve(w http.ResponseWriter, r *http.Request, _ string, fn resolveFn) {
	if h.svc == nil || h.deps == nil {
		response.Error(w, http.StatusServiceUnavailable, "divergence reconciler is not configured")
		return
	}
	idStr := chi.URLParam(r, "id")
	id64, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "id must be a positive integer")
		return
	}
	actor := reconciler.ResolveActor{}
	if u := ctxutil.User(r.Context()); u != nil {
		actor.Email = u.Email
		actor.IsAdmin = u.IsAdmin
	}
	res, err := fn(r.Context(), uint(id64), h.deps, actor)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	response.Success(w, resolveResponse{Result: res})
}

// writeResolveError maps the typed errors the reconciler exposes onto
// HTTP statuses. Other errors fall through to 500.
func writeResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reconciler.ErrDivergenceNotFound):
		response.Error(w, http.StatusNotFound, err.Error())
	default:
		var wrong *reconciler.ErrWrongDivergenceType
		if errors.As(err, &wrong) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		response.InternalError(w, err.Error())
	}
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
