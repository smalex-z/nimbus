package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/nodemgr"
)

// Nodes wraps the nodemgr service for the cluster-status surface (used by
// the Admin dashboard) and the new admin /nodes page (lock state, drain,
// tags, remove). Telemetry-only callers can read List; admin actions live
// behind requireAdmin in the router.
type Nodes struct {
	mgr    *nodemgr.Service
	pxHost string // surfaces in /api/proxmox/binding
}

// NewNodes constructs a Nodes handler. mgr is the shared nodemgr.Service;
// pxHost is the Proxmox base URL the binding endpoint reports back.
func NewNodes(mgr *nodemgr.Service, pxHost string) *Nodes {
	return &Nodes{mgr: mgr, pxHost: pxHost}
}

// List handles GET /api/nodes. Returns one row per Proxmox node with live
// telemetry merged with persistent fields (lock state, tags, lock context).
// Both the Admin dashboard and the new /nodes page consume this.
func (h *Nodes) List(w http.ResponseWriter, r *http.Request) {
	view, err := h.mgr.List(r.Context())
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, view.Nodes)
}

// Cordon handles POST /api/nodes/{name}/cordon. Body: {"reason": "..."}
// (reason is optional). Refuses if a drain is already in flight or the
// transition would skip a state (none → drained, etc.).
func (h *Nodes) Cordon(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body struct {
		Reason string `json:"reason"`
	}
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
		writeNodeMutationError(w, err)
		return
	}
	response.Success(w, row)
}

// Uncordon handles POST /api/nodes/{name}/uncordon. No body. Refused while
// a drain is in flight; refused for state="draining" (must complete first).
func (h *Nodes) Uncordon(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	row, err := h.mgr.Uncordon(r.Context(), name)
	if err != nil {
		writeNodeMutationError(w, err)
		return
	}
	response.Success(w, row)
}

// SetTags handles PUT /api/nodes/{name}/tags. Body: {"tags": [...]}.
// Replaces the entire tag set; empty array clears.
func (h *Nodes) SetTags(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	row, err := h.mgr.SetTags(r.Context(), name, body.Tags)
	if err != nil {
		writeNodeMutationError(w, err)
		return
	}
	response.Success(w, row)
}

// DrainPlan handles GET /api/nodes/{name}/drain-plan. Returns the
// per-VM recommendations + per-destination aggregate the SPA renders
// in the modal.
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

	if err := h.mgr.Execute(r.Context(), nodemgr.ExecuteRequest{
		SourceNode: name,
		Choices:    choices,
	}, report); err != nil {
		// Stream a terminal error event so the SPA can surface it.
		// Already-ack'd HTTP 200; can't change the status code now.
		_ = enc.Encode(nodemgr.DrainEvent{Type: "error", Error: err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// Remove handles DELETE /api/nodes/{name}. Refused unless the node's
// lock state is "drained" (the operator must run a successful drain
// first) and the node isn't the host Nimbus itself runs on.
func (h *Nodes) Remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.mgr.Remove(r.Context(), name); err != nil {
		writeNodeMutationError(w, err)
		return
	}
	response.Success(w, map[string]string{"message": "node removed"})
}

// Binding handles GET /api/proxmox/binding. Tiny payload, polled by the
// header chip. Errors inside collapse into Reachable=false rather than
// HTTP 500 so the chip can render "offline" without spamming alerts.
func (h *Nodes) Binding(w http.ResponseWriter, r *http.Request) {
	out, err := h.mgr.GetBinding(r.Context(), h.pxHost)
	if err != nil {
		response.FromError(w, err)
		return
	}
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
