package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	internalerrors "nimbus/internal/errors"
	"nimbus/internal/nodescore"
	"nimbus/internal/provision"
	"nimbus/internal/proxmox"
)

// hostnameRE matches RFC 1123 single-label hostnames in lowercase. Restricting
// hostnames keeps cloud-init happy and avoids URL-encoding surprises.
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// VMs wraps the provision Service.
type VMs struct {
	svc *provision.Service
}

func NewVMs(svc *provision.Service) *VMs { return &VMs{svc: svc} }

type createVMRequest struct {
	Hostname     string `json:"hostname"`
	Tier         string `json:"tier"`
	OSTemplate   string `json:"os_template"`
	SSHKeyID     *uint  `json:"ssh_key_id,omitempty"`
	SSHPubKey    string `json:"ssh_pubkey,omitempty"`
	SSHPrivKey   string `json:"ssh_privkey,omitempty"`
	GenerateKey  bool   `json:"generate_key,omitempty"`
	PublicTunnel bool   `json:"public_tunnel,omitempty"`
	Subdomain    string `json:"subdomain,omitempty"`
}

// Create handles POST /api/vms — the long-running provision call.
func (h *VMs) Create(w http.ResponseWriter, r *http.Request) {
	var req createVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if err := validateCreate(req); err != nil {
		response.FromError(w, err)
		return
	}

	res, err := h.svc.Provision(r.Context(), provision.Request{
		Hostname:     req.Hostname,
		Tier:         req.Tier,
		OSTemplate:   req.OSTemplate,
		SSHKeyID:     req.SSHKeyID,
		SSHPubKey:    req.SSHPubKey,
		SSHPrivKey:   req.SSHPrivKey,
		GenerateKey:  req.GenerateKey,
		PublicTunnel: req.PublicTunnel,
		Subdomain:    req.Subdomain,
	})
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Created(w, res)
}

// List handles GET /api/vms.
func (h *VMs) List(w http.ResponseWriter, r *http.Request) {
	vms, err := h.svc.List(r.Context(), nil)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, vms)
}

// GetPrivateKey handles GET /api/vms/{id}/private-key. Returns
// {key_name, private_key} for vault-stored keys; 404 when no key was
// deposited for this VM.
//
// Phase 1 has no auth, so this is reachable to anyone who can hit the API
// (matching the rest of the surface). Once OAuth lands, owner gating goes
// here first.
func (h *VMs) GetPrivateKey(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	keyName, privateKey, err := h.svc.GetPrivateKey(r.Context(), uint(id))
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, map[string]string{
		"key_name":    keyName,
		"private_key": privateKey,
	})
}

// Get handles GET /api/vms/{id}.
func (h *VMs) Get(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	vm, err := h.svc.Get(r.Context(), uint(id))
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, vm)
}

func validateCreate(req createVMRequest) error {
	if !hostnameRE.MatchString(req.Hostname) {
		return &internalerrors.ValidationError{
			Field:   "hostname",
			Message: "must match [a-z0-9](-[a-z0-9])* and be 1-63 chars",
		}
	}
	if _, ok := nodescore.Tiers[req.Tier]; !ok {
		return &internalerrors.ValidationError{Field: "tier", Message: "must be one of small, medium, large"}
	}
	// xl is admin-only and not yet wired to OAuth; reject explicitly.
	if req.Tier == "xl" {
		return &internalerrors.ValidationError{
			Field:   "tier",
			Message: "xl tier requires admin approval (not yet enabled in this build)",
		}
	}
	if _, ok := proxmox.TemplateOffsets[req.OSTemplate]; !ok {
		return &internalerrors.ValidationError{
			Field:   "os_template",
			Message: "must be one of ubuntu-24.04, ubuntu-22.04, debian-12, debian-11",
		}
	}
	// At most one SSH-key input mode at a time. None is allowed — the
	// service falls back to the user's default key.
	modes := 0
	if req.SSHKeyID != nil {
		modes++
	}
	if req.GenerateKey {
		modes++
	}
	if req.SSHPubKey != "" {
		modes++
	}
	if modes > 1 {
		return &internalerrors.ValidationError{
			Field:   "ssh",
			Message: "specify at most one of ssh_key_id, ssh_pubkey, or generate_key",
		}
	}
	return nil
}
