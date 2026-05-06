package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/bootstrap"
)

// Bootstrap wraps long-running admin operations (template bootstrap today;
// re-bootstrap, template refresh, etc. later).
type Bootstrap struct {
	svc   *bootstrap.Service
	audit *audit.Service
}

func NewBootstrap(svc *bootstrap.Service) *Bootstrap { return &Bootstrap{svc: svc} }

// WithAudit installs the audit-log sink. Nil disables emission.
func (h *Bootstrap) WithAudit(a *audit.Service) *Bootstrap { h.audit = a; return h }

type bootstrapRequest struct {
	Nodes []string `json:"nodes,omitempty"`
	OS    []string `json:"os,omitempty"`
	Force bool     `json:"force,omitempty"`
}

// bootstrapStatusView is the body of GET /api/admin/bootstrap-status.
type bootstrapStatusView struct {
	Bootstrapped bool `json:"bootstrapped"`
}

// BootstrapStatus handles GET /api/admin/bootstrap-status.
//
// @Summary     Whether at least one VM template exists
// @Description Read-only yes/no — both admins and members need it so the
// @Description Provision UI can decide whether to render the form (templates
// @Description ready) or the "admin access required" card.
// @Tags        bootstrap
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=bootstrapStatusView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /admin/bootstrap-status [get]
func (h *Bootstrap) BootstrapStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	has, err := h.svc.HasTemplates(ctx)
	if err != nil {
		response.InternalError(w, "db error: "+err.Error())
		return
	}
	response.Success(w, bootstrapStatusView{Bootstrapped: has})
}

// BootstrapTemplates handles POST /api/admin/bootstrap-templates.
//
// Synchronous — the call blocks for up to ~20 minutes while Proxmox downloads
// cloud images, creates VMs, and converts them to templates. The route
// timeout in the router is set generously to accommodate this.
//
// Empty body is valid: it kicks off the default flow (every catalogue OS on
// every online node).
//
// @Summary     Build VM templates on cluster nodes (admin)
// @Description Long-running (up to ~20 min) — downloads cloud images, clones,
// @Description and converts to templates. Empty body uses defaults (all
// @Description catalogue OSes on all online nodes). The 30-minute route
// @Description timeout in the router accommodates the slowest path.
// @Tags        bootstrap
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     bootstrapRequest false "Optional override of node/OS scope and force-rebuild flag"
// @Success     200  {object} EnvelopeOK{data=bootstrap.Result}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /admin/bootstrap-templates [post]
func (h *Bootstrap) BootstrapTemplates(w http.ResponseWriter, r *http.Request) {
	var req bootstrapRequest
	// Allow empty body for "use defaults" — only fail on actively malformed JSON.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.BadRequest(w, "invalid JSON body")
			return
		}
	}

	res, err := h.svc.Bootstrap(r.Context(), bootstrap.Request{
		Nodes: req.Nodes,
		OS:    req.OS,
		Force: req.Force,
	})
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:   "bootstrap.templates",
			Details:  map[string]any{"nodes": req.Nodes, "os": req.OS, "force": req.Force},
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.FromError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action: "bootstrap.templates",
		Details: map[string]any{
			"nodes":   req.Nodes,
			"os":      req.OS,
			"force":   req.Force,
			"created": len(res.Created),
			"skipped": len(res.Skipped),
			"failed":  len(res.Failed),
		},
		Success: true,
	})
	response.Success(w, res)
}
