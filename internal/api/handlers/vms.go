package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
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
	TunnelPort   int    `json:"tunnel_port,omitempty"`
	EnableGPU    bool   `json:"enable_gpu,omitempty"`
}

// Create handles POST /api/vms — the long-running provision call.
//
// The response is **newline-delimited JSON** (Content-Type:
// application/x-ndjson). Each line is one event: a `progress` event as each
// backend phase finishes, then a single terminal `result` (success) or
// `error` (failure) line. Validation failures still respond with the
// regular {success,error} JSON envelope and a 4xx status, since headers
// haven't been flushed yet at that point.
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

	// Provision-time tunnel is SSH-only — there's no public subdomain in
	// play (Gopher allocates a port on the gateway instead), but its API
	// still needs a unique tunnel identifier. Default to the VM hostname,
	// which is already validated unique upstream and shaped like a DNS
	// label, so the operator never has to think about it.
	subdomain := req.Subdomain
	if req.PublicTunnel && subdomain == "" {
		subdomain = req.Hostname
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	writeLine := func(v any) {
		_ = enc.Encode(v) // Encode appends '\n'
		if flusher != nil {
			flusher.Flush()
		}
	}

	reporter := func(evt provision.ProgressEvent) {
		writeLine(map[string]any{
			"type":  "progress",
			"step":  evt.Step,
			"label": evt.Label,
		})
	}

	// Stamp the creator's user ID so the VM shows up in their My Machines and
	// is the only person who can delete it later. Pre-existing VMs without an
	// owner remain visible to everyone (see service.List) but immutable
	// through the Delete endpoint. RequesterIsAdmin lets the service skip
	// member quotas and the tier allowlist for admin callers.
	var (
		ownerID          *uint
		requesterIsAdmin bool
	)
	if user := ctxutil.User(r.Context()); user != nil {
		id := user.ID
		ownerID = &id
		requesterIsAdmin = user.IsAdmin
	}

	res, err := h.svc.Provision(r.Context(), provision.Request{
		Hostname:         req.Hostname,
		Tier:             req.Tier,
		OSTemplate:       req.OSTemplate,
		SSHKeyID:         req.SSHKeyID,
		SSHPubKey:        req.SSHPubKey,
		SSHPrivKey:       req.SSHPrivKey,
		GenerateKey:      req.GenerateKey,
		PublicTunnel:     req.PublicTunnel,
		Subdomain:        subdomain,
		TunnelPort:       req.TunnelPort,
		EnableGPU:        req.EnableGPU,
		OwnerID:          ownerID,
		RequesterIsAdmin: requesterIsAdmin,
	}, reporter)
	if err != nil {
		writeLine(map[string]any{
			"type":    "error",
			"code":    classifyProvisionError(err),
			"message": err.Error(),
		})
		return
	}
	writeLine(map[string]any{
		"type": "result",
		"data": res,
	})
}

// classifyProvisionError tags the failure for the frontend so it can
// distinguish user-actionable errors (validation, conflict, not-found)
// from internal failures.
func classifyProvisionError(err error) string {
	var (
		validation *internalerrors.ValidationError
		conflict   *internalerrors.ConflictError
		notFound   *internalerrors.NotFoundError
	)
	switch {
	case errors.As(err, &validation):
		return "validation"
	case errors.As(err, &conflict):
		return "conflict"
	case errors.As(err, &notFound):
		return "not_found"
	default:
		return "internal"
	}
}

// List handles GET /api/vms — returns the caller's own VMs plus any legacy
// rows that pre-date owner tracking (see service.List). The same scope
// applies for both members and admins; cluster-wide views live on the Admin
// dashboard.
func (h *VMs) List(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	vms, err := h.svc.ListWithLiveStatus(r.Context(), &user.ID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, vms)
}

// Delete handles DELETE /api/vms/{id} — destroys the VM on Proxmox, releases
// its IP, and removes the local row. Strict ownership: only the user who
// originally provisioned the VM can delete it. Returns 404 (not 403) for
// cross-user or legacy (owner_id NULL) rows so we don't disclose existence.
func (h *VMs) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	if err := h.svc.Delete(r.Context(), uint(id), user.ID); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Lifecycle handles POST /api/vms/{id}/{op} where op is one of
// start | shutdown | stop | reboot. Owner-gated like Delete: a non-owning
// requester gets 404 so existence isn't disclosed.
func (h *VMs) Lifecycle(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	op := provision.VMLifecycleOp(chi.URLParam(r, "op"))
	if err := h.svc.LifecycleOp(r.Context(), uint(id), user.ID, op); err != nil {
		response.FromError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	keyName, privateKey, err := h.svc.GetPrivateKey(r.Context(), uint(id), &user.ID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, map[string]string{
		"key_name":    keyName,
		"private_key": privateKey,
	})
}

// Get handles GET /api/vms/{id}. Owner-gated: a non-owning requester gets
// 404 (not 403) so existence isn't disclosed — same convention as Delete and
// Lifecycle.
func (h *VMs) Get(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	vm, err := h.svc.Get(r.Context(), uint(id), &user.ID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, vm)
}

// ListTunnels handles GET /api/vms/{id}/tunnels — every Gopher per-port
// tunnel attached to this VM. Returns an empty array for VMs without a
// Gopher machine record (and for tunnel-disabled deployments). Owner-gated
// to prevent enumeration of another user's tunnel layout.
func (h *VMs) ListTunnels(w http.ResponseWriter, r *http.Request) {
	id, ok := parseVMID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	tunnels, err := h.svc.ListVMTunnels(r.Context(), id, &user.ID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, tunnels)
}

type createTunnelRequest struct {
	TargetPort           int    `json:"target_port"`
	Subdomain            string `json:"subdomain,omitempty"`
	Transport            string `json:"transport,omitempty"`
	Private              bool   `json:"private,omitempty"`
	NoTLS                bool   `json:"no_tls,omitempty"`
	BotProtectionEnabled bool   `json:"bot_protection_enabled,omitempty"`
	BotProtectionTTL     int    `json:"bot_protection_ttl,omitempty"`
	BotProtectionAllowIP string `json:"bot_protection_allow_ip,omitempty"`
	TLSSkipVerify        bool   `json:"tls_skip_verify,omitempty"`
}

// CreateTunnel handles POST /api/vms/{id}/tunnels — registers a per-port
// tunnel on this VM's Gopher machine. Mirrors Gopher's POST /api/v1/tunnels
// body shape; see Gopher's OpenAPI spec for field semantics + UDP/bot-
// protection coercion rules. Owner-gated: only the VM owner may attach
// internet-facing tunnels to it.
func (h *VMs) CreateTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseVMID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req createTunnelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	t, err := h.svc.CreateVMTunnel(r.Context(), id, provision.VMTunnelRequest{
		TargetPort:           req.TargetPort,
		Subdomain:            req.Subdomain,
		Transport:            req.Transport,
		Private:              req.Private,
		NoTLS:                req.NoTLS,
		BotProtectionEnabled: req.BotProtectionEnabled,
		BotProtectionTTL:     req.BotProtectionTTL,
		BotProtectionAllowIP: req.BotProtectionAllowIP,
		TLSSkipVerify:        req.TLSSkipVerify,
	}, &user.ID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Created(w, t)
}

// DeleteTunnel handles DELETE /api/vms/{id}/tunnels/{tunnelId}. Owner-gated.
func (h *VMs) DeleteTunnel(w http.ResponseWriter, r *http.Request) {
	id, ok := parseVMID(w, r)
	if !ok {
		return
	}
	tunnelID := chi.URLParam(r, "tunnelId")
	if tunnelID == "" {
		response.BadRequest(w, "missing tunnelId")
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	if err := h.svc.DeleteVMTunnel(r.Context(), id, tunnelID, &user.ID); err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, map[string]string{"message": "tunnel deleted"})
}

// parseVMID extracts and validates the {id} URL param common to the
// per-VM endpoints. Writes a 400 and returns ok=false on failure.
func parseVMID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return 0, false
	}
	return uint(id), true
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
	// Per-tier authorization (member allowlist vs admin-bypass) is enforced
	// in provision.Service.Provision so the rule lives next to the quota
	// it shares semantics with. validateCreate only checks tier shape.
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
	if req.PublicTunnel && (req.TunnelPort < 0 || req.TunnelPort > 65535) {
		return &internalerrors.ValidationError{
			Field:   "tunnel_port",
			Message: "must be 1–65535 (omit or 0 for default 80)",
		}
	}
	return nil
}

// Reconcile handles POST /api/vms/reconcile (admin only). Walks the local vms
// table against the live Proxmox cluster snapshot, updates rows whose VMs
// have migrated to a different node, and soft-deletes rows whose VMID hasn't
// been observed for vacateMissThreshold consecutive runs. Refuses to act when
// Proxmox returns an empty cluster snapshot (transient API failure → would
// otherwise wipe every row).
func (h *VMs) Reconcile(w http.ResponseWriter, r *http.Request) {
	rep, err := h.svc.ReconcileVMs(r.Context())
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	response.Success(w, rep)
}
