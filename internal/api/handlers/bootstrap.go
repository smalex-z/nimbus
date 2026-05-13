package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/bootstrap"
	"nimbus/internal/provision"
)

// LegacyCIDataSweeper is the small surface BootstrapTemplates uses to run
// the post-bootstrap legacy → managed cloud-init sweep across every VM.
// Defined as an interface so the handler can be wired up without a hard
// dependency on *provision.Service in tests.
type LegacyCIDataSweeper interface {
	SweepLegacyCIData(ctx context.Context) (provision.SweepResult, error)
}

// Bootstrap wraps long-running admin operations (template bootstrap today;
// re-bootstrap, template refresh, etc. later).
type Bootstrap struct {
	svc     *bootstrap.Service
	audit   *audit.Service
	sweeper LegacyCIDataSweeper
}

func NewBootstrap(svc *bootstrap.Service) *Bootstrap { return &Bootstrap{svc: svc} }

// WithAudit installs the audit-log sink. Nil disables emission.
func (h *Bootstrap) WithAudit(a *audit.Service) *Bootstrap { h.audit = a; return h }

// WithSweeper installs the legacy cloud-init sweeper. When set, a
// successful BootstrapTemplates call also sweeps every Nimbus VM, swapping
// any remaining pre-D-boot per-VM cidata ISOs for managed cloudinit
// drives. Nil-ok — sweep silently skipped if unset (test setups).
func (h *Bootstrap) WithSweeper(s LegacyCIDataSweeper) *Bootstrap { h.sweeper = s; return h }

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

// TemplatesStatus handles GET /api/admin/templates-status.
//
// @Summary     Per-template freshness check (admin)
// @Description Reports how many node_templates rows still point at a
// @Description Proxmox template that carries the nimbus-baked-v1 tag.
// @Description Used by the SPA banner to nudge operators of pre-D-boot
// @Description deployments to re-run bootstrap so future provisions
// @Description don't fail at the template-baked guard.
// @Tags        bootstrap
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=bootstrap.TemplatesStatus}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /admin/templates-status [get]
func (h *Bootstrap) TemplatesStatus(w http.ResponseWriter, r *http.Request) {
	// One GetVMConfig per template row — for typical 4×4 clusters that
	// completes well under 5s. A 30s timeout caps pathological cases
	// where the Proxmox API is degraded.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := h.svc.CheckTemplatesStatus(ctx)
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	response.Success(w, st)
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

	// Post-bootstrap legacy-VM sweep: any pre-D-boot VM still carrying a
	// per-node cidata ISO at ide2 gets converted to a managed cloudinit
	// drive so future migrations don't fail at Proxmox's local-cdrom
	// guard. Sweep errors don't fail the response — templates ARE built;
	// the per-VM failures are reported back so the operator can retry.
	var sweep *provision.SweepResult
	if h.sweeper != nil {
		// Bounded timeout — keeps a slow Proxmox from holding the request
		// open indefinitely. Each per-VM swap is a few API calls; budget
		// generously for fleets of a hundred or two.
		sweepCtx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		swept, sweepErr := h.sweeper.SweepLegacyCIData(sweepCtx)
		cancel()
		if sweepErr != nil {
			// Cluster-wide failure (DB list etc.) — surface in audit but
			// don't fail the bootstrap response. Operator sees template
			// outcomes; sweep can be retried by re-running.
			h.audit.Record(r.Context(), audit.Event{
				Action:   "bootstrap.sweep_legacy_cidata",
				Success:  false,
				ErrorMsg: sweepErr.Error(),
			})
		} else {
			sweep = &swept
		}
	}

	auditDetails := map[string]any{
		"nodes":   req.Nodes,
		"os":      req.OS,
		"force":   req.Force,
		"created": len(res.Created),
		"skipped": len(res.Skipped),
		"failed":  len(res.Failed),
	}
	if sweep != nil {
		auditDetails["sweep_total"] = sweep.Total
		auditDetails["sweep_ok"] = sweep.OK
		auditDetails["sweep_failed"] = len(sweep.Failed)
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:  "bootstrap.templates",
		Details: auditDetails,
		Success: true,
	})

	response.Success(w, bootstrapResponse{Result: *res, Sweep: sweep})
}

// bootstrapResponse is the wire-shape returned by BootstrapTemplates.
// Embeds bootstrap.Result so the existing template-outcome fields stay
// untouched, and adds the optional Sweep payload from the post-bootstrap
// legacy-CI swap (nil when no sweeper is wired or when the sweep
// itself errored at the cluster level).
type bootstrapResponse struct {
	bootstrap.Result
	Sweep *provision.SweepResult `json:"sweep,omitempty"`
}

// SweepTemplates handles POST /api/admin/templates-sweep. The endpoint
// supports a `dry_run` query param: when set, the response describes
// what *would* be destroyed without making any Proxmox calls; when
// omitted (or "false"), the destroys run. Two-stage flow lets the SPA
// render a preview the operator confirms before any state changes.
//
// Removes three flavors of redundant template artifact across every
// online node:
//
//   - duplicate baked templates (multiple `<os>-template` VMs tagged
//     nimbus-baked-v1 — keeps one, destroys the rest)
//   - unbaked templates whose OS has a baked sibling still surviving
//   - stopped non-template VMs in the template VMID range with template-
//     style names (failed bake leftovers)
//
// Conservative: an OS with no surviving baked template is skipped
// entirely so the operator's rebuild surface stays intact.
//
// @Summary     Sweep redundant template artifacts (admin)
// @Description Removes duplicate templates, unbaked siblings, and failed-
// @Description bake leftover VMs across every online node. Pass
// @Description ?dry_run=true to preview the changes without destroying
// @Description anything — the response describes what would be removed.
// @Tags        bootstrap
// @Security    cookieAuth
// @Produce     json
// @Param       dry_run query bool false "Preview only when true; default false executes destroys"
// @Success     200  {object} EnvelopeOK{data=bootstrap.SweepResult}
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /admin/templates-sweep [post]
func (h *Bootstrap) SweepTemplates(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "true"

	// Bounded but generous — each destroy is a wait-for-task; large
	// fleets with many duplicates can take a couple of minutes.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res, err := h.svc.SweepTemplates(ctx, dryRun)
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:   "bootstrap.templates_sweep",
			Details:  map[string]any{"dry_run": dryRun},
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, err.Error())
		return
	}

	h.audit.Record(r.Context(), audit.Event{
		Action: "bootstrap.templates_sweep",
		Details: map[string]any{
			"dry_run": dryRun,
			"removed": res.TotalRemoved,
			"nodes":   len(res.Nodes),
		},
		Success: true,
	})
	response.Success(w, res)
}
