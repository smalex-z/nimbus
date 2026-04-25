package handlers

import (
	"context"
	"net/http"

	"nimbus/internal/api/response"
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
}

// NewIPs constructs the handler. reconciler may be nil — in that case the
// reconcile endpoint returns 503 so callers learn the feature isn't wired up,
// rather than silently doing nothing.
func NewIPs(pool *ippool.Pool, reconciler reconcileRunner) *IPs {
	return &IPs{pool: pool, reconciler: reconciler}
}

// List handles GET /api/ips.
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
func (h *IPs) Reconcile(w http.ResponseWriter, r *http.Request) {
	if h.reconciler == nil {
		response.ServiceUnavailable(w, "reconciliation is not configured on this instance")
		return
	}
	rep, err := h.reconciler.Reconcile(r.Context())
	if err != nil {
		// The Report is still useful even when some per-row updates failed;
		// surface it alongside the error so the client gets visibility into
		// what worked.
		response.Success(w, map[string]any{
			"report": rep,
			"error":  err.Error(),
		})
		return
	}
	response.Success(w, rep)
}
