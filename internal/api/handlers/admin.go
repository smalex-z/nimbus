package handlers

import (
	"encoding/json"
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/bootstrap"
)

// Admin wraps long-running admin operations (template bootstrap today;
// re-bootstrap, template refresh, etc. later).
type Admin struct {
	svc *bootstrap.Service
}

func NewAdmin(svc *bootstrap.Service) *Admin { return &Admin{svc: svc} }

type bootstrapRequest struct {
	Nodes []string `json:"nodes,omitempty"`
	OS    []string `json:"os,omitempty"`
	Force bool     `json:"force,omitempty"`
}

// BootstrapTemplates handles POST /api/admin/bootstrap-templates.
//
// Synchronous — the call blocks for up to ~20 minutes while Proxmox downloads
// cloud images, creates VMs, and converts them to templates. The route
// timeout in the router is set generously to accommodate this.
//
// Empty body is valid: it kicks off the default flow (every catalogue OS on
// every online node).
func (h *Admin) BootstrapTemplates(w http.ResponseWriter, r *http.Request) {
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
		response.FromError(w, err)
		return
	}
	response.Success(w, res)
}
