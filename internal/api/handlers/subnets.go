package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/vnetmgr"
)

// Subnets handler — per-user SDN subnet CRUD. Mirrors the Keys handler
// shape (List / Get / Create / Delete / SetDefault) so the user-facing
// surface for subnets feels like the SSH-keys page operators already
// know.
//
// Authentication: every endpoint runs inside the verified-user route
// group (cookie + access-code-verified), so users only ever see their
// own subnets. Cross-user reads return 404 (consistent with VMs).
type Subnets struct {
	svc *vnetmgr.Service
}

// NewSubnets constructs a Subnets handler. The vnetmgr.Service must be
// wired with WithDB / WithPool / WithVMRefCounter; the router does
// this in NewRouter.
func NewSubnets(svc *vnetmgr.Service) *Subnets { return &Subnets{svc: svc} }

// subnetView is the JSON projection of a UserSubnet row. Includes
// Status so the UI can render an "error" pill if Proxmox failed
// mid-create. CreatedAt/UpdatedAt are surfaced for the admin's
// debugging convenience.
type subnetView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	VNet      string `json:"vnet"`
	Subnet    string `json:"subnet"`
	Gateway   string `json:"gateway"`
	PoolStart string `json:"pool_start"`
	PoolEnd   string `json:"pool_end"`
	IsDefault bool   `json:"is_default"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toSubnetView(s *db.UserSubnet) subnetView {
	return subnetView{
		ID:        s.ID,
		Name:      s.Name,
		VNet:      s.VNet,
		Subnet:    s.Subnet,
		Gateway:   s.Gateway,
		PoolStart: s.PoolStart,
		PoolEnd:   s.PoolEnd,
		IsDefault: s.IsDefault,
		Status:    s.Status,
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: s.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// createSubnetRequest is the body of POST /api/subnets.
type createSubnetRequest struct {
	Name       string `json:"name"`
	SetDefault bool   `json:"set_default,omitempty"`
}

// List handles GET /api/subnets — returns the caller's subnets,
// default first then alphabetical (matches vnetmgr.ListSubnets order).
//
// @Summary     List the caller's SDN subnets
// @Tags        subnets
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]subnetView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /subnets [get]
func (h *Subnets) List(w http.ResponseWriter, r *http.Request) {
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListSubnets(r.Context(), uid)
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]subnetView, 0, len(rows))
	for i := range rows {
		out = append(out, toSubnetView(&rows[i]))
	}
	response.Success(w, out)
}

// Get handles GET /api/subnets/{id} — owner-gated.
//
// @Summary     Get a single subnet
// @Tags        subnets
// @Security    cookieAuth
// @Produce     json
// @Param       id  path     int true "Subnet ID"
// @Success     200 {object} EnvelopeOK{data=subnetView}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /subnets/{id} [get]
func (h *Subnets) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSubnetID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	row, err := h.svc.GetSubnet(r.Context(), id, uid)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, toSubnetView(row))
}

// Create handles POST /api/subnets — provisions a fresh user-subnet
// (Proxmox VNet + Subnet + IP pool). Async-feeling but actually
// synchronous: the call returns once Proxmox + DB + pool seed all
// succeed. Failures partway through mark the row Status="error" so
// the user can retry the operation by deleting + re-creating.
//
// @Summary     Create a new SDN subnet
// @Description Carves a fresh /N from the configured supernet, creates
// @Description the Proxmox VNet + Subnet, seeds the per-subnet IP pool.
// @Description Set `set_default: true` to make it the user's default
// @Description in one round trip.
// @Tags        subnets
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     createSubnetRequest true "Subnet to create"
// @Success     201  {object} EnvelopeOK{data=subnetView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     503  {object} EnvelopeError
// @Router      /subnets [post]
func (h *Subnets) Create(w http.ResponseWriter, r *http.Request) {
	var req createSubnetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	row, err := h.svc.CreateSubnet(r.Context(), vnetmgr.CreateSubnetRequest{
		OwnerID:    uid,
		Name:       req.Name,
		SetDefault: req.SetDefault,
	})
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Created(w, toSubnetView(row))
}

// Delete handles DELETE /api/subnets/{id}. Refused while VMs are
// attached or while it's the only subnet for the user.
//
// @Summary     Delete a subnet
// @Description Refused while any VM still references the subnet, and
// @Description for an only-default subnet (no fallback to land new
// @Description VMs on). Tears down the Proxmox VNet + Subnet + IP pool.
// @Tags        subnets
// @Security    cookieAuth
// @Param       id  path int true "Subnet ID"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /subnets/{id} [delete]
func (h *Subnets) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSubnetID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteSubnet(r.Context(), id, uid); err != nil {
		response.FromError(w, err)
		return
	}
	response.NoContent(w)
}

// SetDefault handles POST /api/subnets/{id}/default — flips IsDefault
// on this subnet and clears it on every other subnet for the owner.
//
// @Summary     Mark this subnet as the caller's default
// @Description New VMs without an explicit subnet pick land on the
// @Description default. Idempotent — calling on an already-default
// @Description subnet is a no-op.
// @Tags        subnets
// @Security    cookieAuth
// @Param       id  path int true "Subnet ID"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /subnets/{id}/default [post]
func (h *Subnets) SetDefault(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSubnetID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	if err := h.svc.SetDefault(r.Context(), id, uid); err != nil {
		response.FromError(w, err)
		return
	}
	response.NoContent(w)
}

func parseSubnetID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return 0, false
	}
	return uint(id), true
}
