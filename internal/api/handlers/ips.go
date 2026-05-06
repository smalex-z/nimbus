package handlers

import (
	"context"
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/ippool"
)

// reconcileRunner is the small interface the handler depends on. Defined here
// in the consumer per the small-interface idiom; satisfied in production by
// *ippool.Reconciler.
type reconcileRunner interface {
	Reconcile(ctx context.Context) (ippool.Report, error)
}

// IPs exposes the static IP pool state for debugging and the admin UI, plus
// the on-demand reconcile endpoint.
type IPs struct {
	pool       *ippool.Pool
	reconciler reconcileRunner
	audit      *audit.Service
}

// NewIPs constructs the handler. reconciler may be nil — in that case the
// reconcile endpoint returns 503 so callers learn the feature isn't wired up,
// rather than silently doing nothing.
func NewIPs(pool *ippool.Pool, reconciler reconcileRunner) *IPs {
	return &IPs{pool: pool, reconciler: reconciler}
}

// WithAudit installs the audit-log sink. Nil disables emission.
func (h *IPs) WithAudit(a *audit.Service) *IPs { h.audit = a; return h }

// List handles GET /api/ips.
//
// @Summary     List the static IP pool with per-row state
// @Description Admin-only view of the local cache: free/reserved/allocated +
// @Description who holds each IP. The reconciler converges this against
// @Description Proxmox in the background; use POST /ips/reconcile to force
// @Description a fresh diff on demand.
// @Tags        ips
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]ippool.IPAllocation}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /ips [get]
func (h *IPs) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.List(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, rows)
}

// Reconcile handles POST /api/ips/reconcile. Forces a fresh diff against the
// Proxmox cluster and returns the resulting Report. Used by operators to
// surface conflicts on demand without waiting for the background loop.
//
// @Summary     Force an IP-pool reconcile against Proxmox (admin)
// @Description Bypasses the background loop and runs a one-shot diff. Returns
// @Description the resulting Report (adopted / conflicts / freed / vacated).
// @Description Even on partial failure the response is 200 — the report still
// @Description carries the rows that were touched, with the error string
// @Description alongside.
// @Tags        ips
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=ippool.Report}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /ips/reconcile [post]
func (h *IPs) Reconcile(w http.ResponseWriter, r *http.Request) {
	if h.reconciler == nil {
		response.ServiceUnavailable(w, "reconciliation is not configured on this instance")
		return
	}
	rep, err := h.reconciler.Reconcile(r.Context())
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:   "ips.reconcile",
			Success:  false,
			ErrorMsg: err.Error(),
		})
		// The Report is still useful even when some per-row updates failed;
		// surface it alongside the error so the client gets visibility into
		// what worked.
		response.Success(w, map[string]any{
			"report": rep,
			"error":  err.Error(),
		})
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:  "ips.reconcile",
		Success: true,
	})
	response.Success(w, rep)
}
