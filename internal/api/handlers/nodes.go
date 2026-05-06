package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/config"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/nodemgr"
	"nimbus/internal/proxmox"
)

// Nodes wraps the nodemgr service for the cluster-status surface (used by
// the Admin dashboard) and the new admin /nodes page (lock state, drain,
// tags, remove). Telemetry-only callers can read List; admin actions live
// behind requireAdmin in the router.
type Nodes struct {
	mgr     *nodemgr.Service
	cfg     *config.Config
	restart func()
	audit   *audit.Service
}

// NewNodes constructs a Nodes handler. mgr is the shared nodemgr.Service;
// cfg is the live config (read for the binding endpoint and rewritten by
// the change-binding flow); restart is the restartSelf hook that re-execs
// the binary after credentials change so the new Proxmox client takes
// effect. cfg or restart may be nil in tests; the change-binding endpoint
// returns 503 in that case.
func NewNodes(mgr *nodemgr.Service, cfg *config.Config, restart func()) *Nodes {
	return &Nodes{mgr: mgr, cfg: cfg, restart: restart}
}

// WithAudit installs the audit-log sink. Nil disables emission; tests
// pass nil and production wires d.Audit. Returns the receiver so it
// chains after NewNodes.
func (h *Nodes) WithAudit(a *audit.Service) *Nodes { h.audit = a; return h }

// List handles GET /api/nodes. Returns one row per Proxmox node with live
// telemetry merged with persistent fields (lock state, tags, lock context).
// Both the Admin dashboard and the new /nodes page consume this.
//
// When ?include_scores=true is passed, each row is decorated with a
// `score` breakdown for the preview tier (default `medium`; override
// with ?preview_tier=large). The Nodes page's scoring matrix consumes
// this; consumers that don't ask get the plain payload.
//
// @Summary     List cluster nodes with lock state + telemetry (admin)
// @Description Joins Proxmox live telemetry (cpu/mem/storage) with the
// @Description Nimbus-side lock state (none/cordoned/draining/drained) and
// @Description operator-set tags. Powers both the Admin dashboard and the
// @Description /nodes page. When `include_scores=true`, each row carries a
// @Description `score` breakdown for the preview tier so the scoring matrix
// @Description can render without a second round-trip.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Param       include_scores query    string false "set to `true` to decorate rows with score breakdowns"
// @Param       preview_tier   query    string false "tier to score against (default `medium`); only used when include_scores=true" Enums(small, medium, large, xl)
// @Success     200 {object} EnvelopeOK{data=[]nodemgr.View}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes [get]
func (h *Nodes) List(w http.ResponseWriter, r *http.Request) {
	view, err := h.mgr.List(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}

	if r.URL.Query().Get("include_scores") != "true" {
		response.Success(w, view.Nodes)
		return
	}
	previewTier := r.URL.Query().Get("preview_tier")
	scoreByNode, resolvedTier, err := h.mgr.ScoreClusterAtTier(r.Context(), previewTier)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	out := make([]nodemgr.NodeWithScores, 0, len(view.Nodes))
	for _, n := range view.Nodes {
		row := nodemgr.NodeWithScores{View: n, PreviewTier: resolvedTier}
		if b, ok := scoreByNode[n.Name]; ok {
			row.Score = &b
		}
		out = append(out, row)
	}
	response.Success(w, out)
}

// Score handles GET /api/nodes/{name}/score?tier=&tags=. Single-cell
// drill-down — full breakdown including rejection reasons under the
// given tier and host-aggregate constraint. `tags` is a comma-separated
// list (empty = no constraint).
//
// @Summary     Score a single node at a tier + host-aggregate constraint (admin)
// @Description Drill-down view used by the Nodes-page tooltip. Returns the
// @Description full components map plus rejection reasons (when score=0)
// @Description for the given (tier, required_tags) cell.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Param       name path     string true "Proxmox node name"
// @Param       tier query    string false "tier the synthetic VM is sized at (default `medium`)" Enums(small, medium, large, xl)
// @Param       tags query    string false "comma-separated host-aggregate tags the candidate must carry (empty = no constraint)"
// @Success     200  {object} EnvelopeOK{data=nodemgr.ScoreBreakdown}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes/{name}/score [get]
func (h *Nodes) Score(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	tierName := r.URL.Query().Get("tier")
	if tierName == "" {
		tierName = "medium"
	}
	requiredTags := parseTagsParam(r.URL.Query().Get("tags"))
	out, err := h.mgr.ScoreNode(r.Context(), name, tierName, requiredTags)
	if err != nil {
		writeNodeMutationError(w, err)
		return
	}
	response.Success(w, out)
}

// parseTagsParam splits a CSV query-param into a clean tag slice.
// Whitespace trimmed; empty entries dropped.
func parseTagsParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cordonRequest is the body of POST /api/nodes/{name}/cordon. Reason is
// optional — surfaces in the Nodes page as the lock-context label.
type cordonRequest struct {
	Reason string `json:"reason,omitempty"`
}

// Cordon handles POST /api/nodes/{name}/cordon. Body: {"reason": "..."}
// (reason is optional). Refuses if a drain is already in flight or the
// transition would skip a state (none → drained, etc.).
//
// @Summary     Cordon a node (admin)
// @Description Marks the node as ineligible for new VM placement. Refused
// @Description while a drain is in-flight on this node.
// @Tags        nodes
// @Security    cookieAuth
// @Accept      json
// @Param       name path     string        true  "Proxmox node name"
// @Param       body body     cordonRequest false "Optional reason for the cordon"
// @Success     200  {object} EnvelopeOK{data=db.Node}
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     404  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /nodes/{name}/cordon [post]
func (h *Nodes) Cordon(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body cordonRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	user := ctxutil.User(r.Context())
	var actor uint
	if user != nil {
		actor = user.ID
	}
	row, err := h.mgr.Cordon(r.Context(), nodemgr.CordonRequest{
		NodeName: name, Reason: body.Reason, ActorID: actor,
	})
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "node.cordon",
			TargetType:  "node",
			TargetLabel: name,
			Details:     map[string]any{"reason": body.Reason},
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		writeNodeMutationError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.cordon",
		TargetType:  "node",
		TargetLabel: name,
		Details:     map[string]any{"reason": body.Reason},
		Success:     true,
	})
	response.Success(w, row)
}

// Uncordon handles POST /api/nodes/{name}/uncordon. No body. Refused while
// a drain is in flight; refused for state="draining" (must complete first).
//
// @Summary     Lift the cordon on a node (admin)
// @Description Refused while a drain is in-flight; the operator must let it
// @Description complete first. Reverts cordoned/drained → none.
// @Tags        nodes
// @Security    cookieAuth
// @Param       name path     string true "Proxmox node name"
// @Success     200  {object} EnvelopeOK{data=db.Node}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes/{name}/uncordon [post]
func (h *Nodes) Uncordon(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	row, err := h.mgr.Uncordon(r.Context(), name)
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "node.uncordon",
			TargetType:  "node",
			TargetLabel: name,
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		writeNodeMutationError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.uncordon",
		TargetType:  "node",
		TargetLabel: name,
		Success:     true,
	})
	response.Success(w, row)
}

// setTagsRequest is the body of PUT /api/nodes/{name}/tags.
type setTagsRequest struct {
	Tags []string `json:"tags"`
}

// SetTags handles PUT /api/nodes/{name}/tags. Body: {"tags": [...]}.
// Replaces the entire tag set; empty array clears.
//
// @Summary     Replace a node's tag set (admin)
// @Description Replaces the entire set; pass an empty array to clear. Tags
// @Description are operator-set labels surfaced in the Admin dashboard;
// @Description Nimbus doesn't interpret them today.
// @Tags        nodes
// @Security    cookieAuth
// @Accept      json
// @Param       name path     string         true "Proxmox node name"
// @Param       body body     setTagsRequest true "New tag set"
// @Success     200  {object} EnvelopeOK{data=db.Node}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes/{name}/tags [put]
func (h *Nodes) SetTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body setTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	row, err := h.mgr.SetTags(r.Context(), name, body.Tags)
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "node.tags.set",
			TargetType:  "node",
			TargetLabel: name,
			Details:     map[string]any{"tags": body.Tags},
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		writeNodeMutationError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.tags.set",
		TargetType:  "node",
		TargetLabel: name,
		Details:     map[string]any{"tags": body.Tags},
		Success:     true,
	})
	response.Success(w, row)
}

// DrainPlan handles GET /api/nodes/{name}/drain-plan. Returns the
// per-VM recommendations + per-destination aggregate the SPA renders
// in the modal.
//
// @Summary     Compute the drain plan for a node (admin)
// @Description Per-VM recommendations + per-destination aggregate. Renders in
// @Description the drain-confirm modal so the operator can override
// @Description placement before executing.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Param       name path     string true "Proxmox node name"
// @Success     200  {object} EnvelopeOK{data=nodemgr.DrainPlan}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes/{name}/drain-plan [get]
func (h *Nodes) DrainPlan(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	plan, err := h.mgr.ComputePlan(r.Context(), name)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, plan)
}

// drainExecuteRequest is the wire shape of the operator-confirmed plan.
// Choices is a list (rather than a map) so the SPA can submit it as JSON
// without forcing string keys.
type drainExecuteRequest struct {
	Choices []struct {
		VMID   int    `json:"vm_id"`
		Target string `json:"target"`
	} `json:"choices"`
	// ConfirmPhrase must match "DRAIN <NODE>" exactly. The SPA enforces
	// this client-side too; the server check is the authoritative gate
	// in case the SPA bug-bypasses.
	ConfirmPhrase string `json:"confirm_phrase"`
}

// Drain handles POST /api/nodes/{name}/drain as an NDJSON stream. Each
// line is one DrainEvent. Mirrors the s3 deploy pattern for consistency.
//
// @Summary     Execute a drain as an NDJSON stream (admin)
// @Description Streams `application/x-ndjson` — one DrainEvent per line.
// @Description The phrase "DRAIN <NODE>" must match exactly to confirm
// @Description (the SPA enforces this client-side too). Total wall time can
// @Description run tens of minutes per VM on cold migrations; the route
// @Description timeout is 60 minutes.
// @Tags        nodes
// @Security    cookieAuth
// @Accept      json
// @Produce     application/x-ndjson
// @Param       name path     string              true "Proxmox node name"
// @Param       body body     drainExecuteRequest true "Operator-confirmed plan + DRAIN <NODE> phrase"
// @Success     200 "NDJSON stream of DrainEvents"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Router      /nodes/{name}/drain [post]
func (h *Nodes) Drain(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req drainExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	expected := fmt.Sprintf("DRAIN %s", strings.ToUpper(name))
	if strings.TrimSpace(req.ConfirmPhrase) != expected {
		response.BadRequest(w, "confirm_phrase must equal "+expected)
		return
	}

	choices := make(map[int]string, len(req.Choices))
	for _, c := range req.Choices {
		choices[c.VMID] = c.Target
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	report := func(evt nodemgr.DrainEvent) {
		_ = enc.Encode(evt)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Record drain start once — completion is async (NDJSON streamed).
	// A "node.drain.complete" follow-up could fire post-Execute, but
	// that adds complexity for a minor signal; the start event +
	// per-VM migrate events recorded by the executor (future work)
	// cover the operator's "what just happened" question.
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.drain.start",
		TargetType:  "node",
		TargetLabel: name,
		Details:     map[string]any{"choices": req.Choices},
		Success:     true,
	})

	if err := h.mgr.Execute(r.Context(), nodemgr.ExecuteRequest{
		SourceNode: name,
		Choices:    choices,
	}, report); err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "node.drain.complete",
			TargetType:  "node",
			TargetLabel: name,
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		// Stream a terminal error event so the SPA can surface it.
		// Already-ack'd HTTP 200; can't change the status code now.
		_ = enc.Encode(nodemgr.DrainEvent{Type: "error", Error: err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.drain.complete",
		TargetType:  "node",
		TargetLabel: name,
		Success:     true,
	})
}

// removeNodeResponse is the body of DELETE /api/nodes/{name}.
type removeNodeResponse struct {
	Message string `json:"message" example:"node removed"`
}

// Remove handles DELETE /api/nodes/{name}. Refused unless the node's
// lock state is "drained" (the operator must run a successful drain
// first) and the node isn't the host Nimbus itself runs on.
//
// @Summary     Remove a drained node from the cluster (admin)
// @Description Refused unless the node is in state="drained" and isn't the
// @Description host Nimbus itself runs on (self-host removal would lock the
// @Description operator out of the admin UI).
// @Tags        nodes
// @Security    cookieAuth
// @Param       name path     string true "Proxmox node name"
// @Success     200  {object} EnvelopeOK{data=removeNodeResponse}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /nodes/{name} [delete]
func (h *Nodes) Remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.mgr.Remove(r.Context(), name); err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "node.remove",
			TargetType:  "node",
			TargetLabel: name,
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		writeNodeMutationError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "node.remove",
		TargetType:  "node",
		TargetLabel: name,
		Success:     true,
	})
	response.Success(w, removeNodeResponse{Message: "node removed"})
}

// Binding handles GET /api/proxmox/binding. Tiny payload, polled by the
// Nodes page. Errors inside collapse into Reachable=false rather than
// HTTP 500 so the page can render "offline" without spamming alerts.
//
// @Summary     Read the active Proxmox binding (admin)
// @Description Returns host + token-id + reachable flag. Polled by the Nodes
// @Description page; downstream errors collapse to reachable=false rather
// @Description than HTTP 500 so the UI can show "offline" without alerts.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=nodemgr.Binding}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Router      /proxmox/binding [get]
func (h *Nodes) Binding(w http.ResponseWriter, r *http.Request) {
	var host, tokenID string
	if h.cfg != nil {
		host = h.cfg.ProxmoxHost
		tokenID = h.cfg.ProxmoxTokenID
	}
	out, err := h.mgr.GetBinding(r.Context(), host, tokenID)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, out)
}

// changeBindingRequest is the operator-confirmed reconfigure payload.
// Mirrors the install wizard's testConnRequest — host + token-id +
// token-secret. Other env values (IP pool, gateway, etc.) are preserved
// from the current config and rewritten back unchanged.
//
// ProxmoxTokenSecret is optional: empty means "keep the current secret"
// — Proxmox API tokens are cluster-wide, so swapping the entry-point
// node (the common case) doesn't need a new credential. The operator
// only re-enters the secret when actually rotating it.
type changeBindingRequest struct {
	ProxmoxHost        string `json:"proxmox_host"`
	ProxmoxTokenID     string `json:"proxmox_token_id"`
	ProxmoxTokenSecret string `json:"proxmox_token_secret"`
}

// changeBindingResponse is the body of the success branch of PUT
// /api/proxmox/binding. Restart message is for the SPA's toast; the
// actual reload happens 500ms after the response flushes.
type changeBindingResponse struct {
	Message string `json:"message" example:"Proxmox connection updated. Reloading Nimbus…"`
}

// ChangeBinding handles PUT /api/proxmox/binding. Probes the new
// credentials with a one-shot Version call (8s timeout) — fail-fast so
// a typo doesn't get persisted; success path writes the env file with
// the existing IP-pool / gateway / etc. values intact, sets the process
// env so syscall.Exec picks them up, then triggers restartSelf after a
// short delay so the response can flush.
//
// Restart-after-write is the same flow the install wizard uses; the
// alternative (in-process pveClient swap) would require surgery in
// every consumer of *proxmox.Client and silent re-instantiation isn't
// safer than a 1-2s reload.
//
// @Summary     Reconfigure the Proxmox binding (admin)
// @Description Persists a new host + token to the env file and re-execs the
// @Description process to pick up the change. Empty `proxmox_token_secret`
// @Description means "keep the current secret" — useful when swapping
// @Description entry-point nodes (Proxmox tokens are cluster-wide).
// @Tags        nodes
// @Security    cookieAuth
// @Accept      json
// @Param       body body     changeBindingRequest  true "New Proxmox triple (secret optional)"
// @Success     200  {object} EnvelopeOK{data=changeBindingResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     502 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /proxmox/binding [put]
func (h *Nodes) ChangeBinding(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || h.restart == nil {
		response.Error(w, http.StatusServiceUnavailable, "reconfigure not wired in this build")
		return
	}
	var req changeBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	req.ProxmoxHost = strings.TrimSpace(req.ProxmoxHost)
	req.ProxmoxTokenID = strings.TrimSpace(req.ProxmoxTokenID)
	req.ProxmoxTokenSecret = strings.TrimSpace(req.ProxmoxTokenSecret)
	if req.ProxmoxHost == "" || req.ProxmoxTokenID == "" {
		response.BadRequest(w, "proxmox_host and proxmox_token_id are required")
		return
	}
	// Empty secret = "keep the current one" (Proxmox tokens are
	// cluster-wide, so switching entry nodes doesn't need new creds).
	// Reject when there's no current secret to fall back on — should
	// only happen on the wizard path, but defensive.
	secret := req.ProxmoxTokenSecret
	if secret == "" {
		secret = h.cfg.ProxmoxTokenSecret
	}
	if secret == "" {
		response.BadRequest(w, "proxmox_token_secret is required (no current secret to keep)")
		return
	}

	// Probe before persisting — typos shouldn't strand the operator
	// outside their own admin UI.
	probe := proxmox.New(req.ProxmoxHost, req.ProxmoxTokenID, secret, 10*time.Second)
	probeCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if _, err := probe.Version(probeCtx); err != nil {
		response.Error(w, http.StatusBadGateway, "cannot reach Proxmox with the new credentials: "+err.Error())
		return
	}

	// Preserve every other env value — only the Proxmox triple changes.
	envPath := config.EnvFilePath()
	if err := config.WriteEnvFile(envPath, config.EnvValues{
		ProxmoxHost:        req.ProxmoxHost,
		ProxmoxTokenID:     req.ProxmoxTokenID,
		ProxmoxTokenSecret: secret,
		IPPoolStart:        h.cfg.IPPoolStart,
		IPPoolEnd:          h.cfg.IPPoolEnd,
		GatewayIP:          h.cfg.GatewayIP,
		VMPrefixLen:        h.cfg.VMPrefixLen,
		Nameserver:         h.cfg.Nameserver,
		SearchDomain:       h.cfg.SearchDomain,
		Port:               h.cfg.Port,
		GopherAPIURL:       h.cfg.GopherAPIURL,
		GopherAPIKey:       h.cfg.GopherAPIKey,
	}); err != nil {
		response.InternalError(w, "failed to persist env file: "+err.Error())
		return
	}

	// syscall.Exec inherits the parent's env — set so the new image
	// picks up the new credentials immediately.
	_ = os.Setenv("PROXMOX_HOST", req.ProxmoxHost)
	_ = os.Setenv("PROXMOX_TOKEN_ID", req.ProxmoxTokenID)
	_ = os.Setenv("PROXMOX_TOKEN_SECRET", secret)

	h.audit.Record(r.Context(), audit.Event{
		Action: "settings.proxmox.update",
		Details: map[string]any{
			"host":     req.ProxmoxHost,
			"token_id": req.ProxmoxTokenID,
			// secret never leaves the server boundary in audit
		},
		Success: true,
	})
	response.Success(w, changeBindingResponse{
		Message: "Proxmox connection updated. Reloading Nimbus…",
	})

	// Defer the restart so the HTTP response flushes first.
	go func() {
		time.Sleep(500 * time.Millisecond)
		h.restart()
	}()
}

// schedulingSettingsRequest is the body of PUT /api/scheduling. All
// three fields are required; missing/zero values are rejected so the
// operator can't silently strand physical capacity.
type schedulingSettingsRequest struct {
	CPUAllocationRatio  float64 `json:"cpu_allocation_ratio"`
	RAMAllocationRatio  float64 `json:"ram_allocation_ratio"`
	DiskAllocationRatio float64 `json:"disk_allocation_ratio"`
}

// GetSchedulingSettings handles GET /api/scheduling. Returns the
// cluster-wide overcommit ratios used by the placement scorer. Defaults
// (4.0/1.0/1.0) are seeded on first read so the SPA always renders
// concrete values.
//
// @Summary     Read cluster overcommit ratios (admin)
// @Description Returns the placement scheduler's cpu/ram/disk allocation
// @Description ratios. Defaults are seeded on first read so the value is
// @Description never empty.
// @Tags        nodes
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=db.SchedulingSettings}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /scheduling [get]
func (h *Nodes) GetSchedulingSettings(w http.ResponseWriter, r *http.Request) {
	out, err := h.mgr.GetSchedulingSettings(r.Context())
	if err != nil {
		response.InternalError(w, err.Error())
		return
	}
	response.Success(w, out)
}

// SaveSchedulingSettings handles PUT /api/scheduling. Persists new
// ratios; each is clamped to [1.0, 64.0] inside the service layer so a
// negative or absurdly large value can't break placement.
//
// @Summary     Update cluster overcommit ratios (admin)
// @Description Persists the placement scheduler's cpu/ram/disk allocation
// @Description ratios. Each is clamped to [1.0, 64.0]. Takes effect on
// @Description the next provision/drain — no restart required.
// @Tags        nodes
// @Security    cookieAuth
// @Accept      json
// @Param       body body     schedulingSettingsRequest true "New ratios"
// @Success     200  {object} EnvelopeOK{data=db.SchedulingSettings}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /scheduling [put]
func (h *Nodes) SaveSchedulingSettings(w http.ResponseWriter, r *http.Request) {
	var req schedulingSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	out, err := h.mgr.SaveSchedulingSettings(r.Context(), db.SchedulingSettings{
		CPUAllocationRatio:  req.CPUAllocationRatio,
		RAMAllocationRatio:  req.RAMAllocationRatio,
		DiskAllocationRatio: req.DiskAllocationRatio,
	})
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:   "settings.scheduling.update",
			Details:  req,
			Success:  false,
			ErrorMsg: err.Error(),
		})
		response.InternalError(w, err.Error())
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:  "settings.scheduling.update",
		Details: out,
		Success: true,
	})
	response.Success(w, out)
}

// writeNodeMutationError maps nodemgr's typed errors to HTTP statuses.
// Unknown errors fall through to 500.
func writeNodeMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, nodemgr.ErrNodeNotFound):
		response.NotFound(w, err.Error())
	case errors.Is(err, nodemgr.ErrInvalidLock),
		errors.Is(err, nodemgr.ErrNotDrained),
		errors.Is(err, nodemgr.ErrSelfHosted),
		errors.Is(err, nodemgr.ErrAlreadyDrained):
		response.Conflict(w, err.Error())
	case errors.Is(err, nodemgr.ErrDrainInFlight):
		response.Conflict(w, err.Error())
	default:
		response.InternalError(w, err.Error())
	}
}
