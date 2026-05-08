package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/vpcmgr"
)

// VPCs is the HTTP surface for the Networking-v1 VPC primitive
// (VXLAN zone shared across nodes + per-VPC gateway LXC). User-facing
// surface mirrors the Subnets handler shape during the deprecation
// window so a migration to this page feels continuous.
type VPCs struct {
	svc *vpcmgr.Service
}

// NewVPCs constructs the VPCs handler. The service must already be
// wired with a GatewayProvisioner.
func NewVPCs(svc *vpcmgr.Service) *VPCs { return &VPCs{svc: svc} }

// vpcView is the JSON projection of a VPC row, augmented with member
// count for the list view.
type vpcView struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	CIDR         string `json:"cidr"`
	Status       string `json:"status"`
	GatewayLXCID *int   `json:"gateway_lxc_id,omitempty"`
	GatewayNode  string `json:"gateway_node,omitempty"`
	MemberCount  int    `json:"member_count"`
	CreatedAt    string `json:"created_at"`
}

func toVPCView(v *db.VPC, memberCount int) vpcView {
	return vpcView{
		ID:           v.ID,
		Name:         v.Name,
		CIDR:         v.CIDR,
		Status:       v.Status,
		GatewayLXCID: v.GatewayLXCID,
		GatewayNode:  v.GatewayNode,
		MemberCount:  memberCount,
		CreatedAt:    v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

type createVPCRequest struct {
	Name string `json:"name"`
}

// List handles GET /api/vpcs.
//
// @Summary     List the caller's VPCs
// @Tags        vpcs
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]vpcView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /vpcs [get]
func (h *VPCs) List(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	rows, err := h.svc.ListVPCs(r.Context(), user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]vpcView, 0, len(rows))
	for i := range rows {
		count, _ := h.svc.CountMembers(r.Context(), rows[i].ID)
		out = append(out, toVPCView(&rows[i], count))
	}
	response.Success(w, out)
}

// Get handles GET /api/vpcs/{id}.
//
// @Summary     Get a single VPC
// @Tags        vpcs
// @Security    cookieAuth
// @Produce     json
// @Param       id  path     int true "VPC ID"
// @Success     200 {object} EnvelopeOK{data=vpcView}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /vpcs/{id} [get]
func (h *VPCs) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseVPCID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	row, err := h.svc.GetVPC(r.Context(), id, user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	count, _ := h.svc.CountMembers(r.Context(), row.ID)
	response.Success(w, toVPCView(row, count))
}

// Create handles POST /api/vpcs.
//
// @Summary     Create a new VPC
// @Description Provisions a per-VPC VXLAN zone + dedicated gateway LXC
// @Description for NAT egress. Returns once the gateway LXC is healthy.
// @Tags        vpcs
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     createVPCRequest true "VPC to create"
// @Success     201  {object} EnvelopeOK{data=vpcView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /vpcs [post]
func (h *VPCs) Create(w http.ResponseWriter, r *http.Request) {
	var req createVPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	row, err := h.svc.CreateVPC(r.Context(), user.ID, req.Name)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Created(w, toVPCView(row, 0))
}

// Delete handles DELETE /api/vpcs/{id}. Refused while VMs are members.
//
// @Summary     Delete a VPC
// @Description Refused while any VM is still a member. Tears down the
// @Description gateway LXC, VXLAN zone, VNet, subnet, and IP allocations.
// @Tags        vpcs
// @Security    cookieAuth
// @Param       id  path int true "VPC ID"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Router      /vpcs/{id} [delete]
func (h *VPCs) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseVPCID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	if err := h.svc.DeleteVPC(r.Context(), id, user.ID, user.IsAdmin); err != nil {
		response.FromError(w, err)
		return
	}
	response.NoContent(w)
}

func parseVPCID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	idStr := chi.URLParam(r, "id")
	id64, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid VPC ID")
		return 0, false
	}
	return uint(id64), true
}
