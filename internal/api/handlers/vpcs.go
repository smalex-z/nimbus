package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/vpcmgr"
)

// VPCs is the HTTP surface for the Networking-v1 VPC primitive
// (VXLAN zone shared across nodes + per-VPC gateway LXC). The handler
// is always mounted; when the service is nil (admin hasn't configured
// the network node + gateway-LXC IP pool) every method returns 503
// with a clear "VPCs not configured" message instead of letting the
// route 404.
//
// The svc reference is swappable via SetSvc so the Settings → Network
// save handler can flip VPCs on without a Nimbus restart. Mutex-
// protected — HTTP requests race the swap, so reads need to be safe.
type VPCs struct {
	mu  sync.RWMutex
	svc *vpcmgr.Service
}

// NewVPCs constructs the VPCs handler. svc may be nil; subsequent
// SetSvc calls live-rotate the service.
func NewVPCs(svc *vpcmgr.Service) *VPCs { return &VPCs{svc: svc} }

// SetSvc swaps the live vpcmgr service. Called from main.go's
// rebuildVPCStack on every Settings → Network save.
func (h *VPCs) SetSvc(svc *vpcmgr.Service) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.svc = svc
}

// service returns the current service under read-lock.
func (h *VPCs) service() *vpcmgr.Service {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.svc
}

// disabledMessage is what the handler returns when no vpcmgr is wired.
const disabledMessage = "VPC networking is not configured on this Nimbus instance — set NIMBUS_NETWORK_NODE, NIMBUS_GATEWAY_LXC_IP_POOL, and NIMBUS_GATEWAY_LXC_TEMPLATE, then restart"

func (h *VPCs) requireEnabled(w http.ResponseWriter) bool {
	if h.service() == nil {
		response.Error(w, http.StatusServiceUnavailable, disabledMessage)
		return false
	}
	return true
}

// Status returns whether the VPC primitive is configured. Used by
// the Provision page picker so the frontend can grey out the VPC
// chip with a clear reason instead of failing on submit.
type vpcStatusView struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// GetStatus handles GET /api/vpcs/status (public — gating logic is
// the same for admins and members).
//
// @Summary     Report VPC primitive availability
// @Tags        vpcs
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=vpcStatusView}
// @Router      /vpcs/status [get]
func (h *VPCs) GetStatus(w http.ResponseWriter, _ *http.Request) {
	if h.service() == nil {
		response.Success(w, vpcStatusView{Enabled: false, Reason: disabledMessage})
		return
	}
	response.Success(w, vpcStatusView{Enabled: true})
}

// NetworkingInfo is the unified availability snapshot the Provision
// page uses to render its picker. Each field maps 1:1 to a chip:
//   - StandaloneEnabled gates the Standalone chip (always true in v1
//     except when the admin disabled the primitive).
//   - VPCEnabled gates the VPC chip; VPCReason carries the configure
//     hint when disabled.
//   - ClusterLANForMembers gates the Cluster LAN chip for non-admins.
type NetworkingInfo struct {
	StandaloneEnabled    bool   `json:"standalone_enabled"`
	VPCEnabled           bool   `json:"vpc_enabled"`
	VPCReason            string `json:"vpc_reason,omitempty"`
	ClusterLANForMembers bool   `json:"cluster_lan_for_members"`
}

// NetworkingInfoSource bundles the per-toggle reads NetworkingInfo
// needs. Implemented by *provision.Service in production; tests can
// supply a stub.
type NetworkingInfoSource interface {
	ClusterLANForMembers() bool
}

// Networking is the public networking-info handler. The VPC source
// is swappable via SetVPCSource so the Settings → Network save can
// flip the picker chip without a Nimbus restart.
type Networking struct {
	mu  sync.RWMutex
	vpc NetworkingVPCSource
	src NetworkingInfoSource
}

// NetworkingVPCSource is the slice of vpcmgr.Service that the
// networking-info handler needs — in practice "is the service wired
// or not?" is the only signal we expose.
type NetworkingVPCSource interface {
	Enabled() bool
}

// NewNetworking constructs the handler. vpc may be nil (means VPC
// primitive isn't wired); src may be nil (defaults to "cluster-LAN
// off for members").
func NewNetworking(vpc NetworkingVPCSource, src NetworkingInfoSource) *Networking {
	return &Networking{vpc: vpc, src: src}
}

// SetVPCSource swaps the VPC availability source. Called by main.go's
// rebuildVPCStack on every Settings → Network save.
func (h *Networking) SetVPCSource(vpc NetworkingVPCSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vpc = vpc
}

// GetInfo handles GET /api/networking/info.
//
// @Summary     Networking-v1 availability snapshot
// @Tags        networking
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=NetworkingInfo}
// @Router      /networking/info [get]
func (h *Networking) GetInfo(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	vpc := h.vpc
	h.mu.RUnlock()

	info := NetworkingInfo{StandaloneEnabled: true}
	if vpc != nil && vpc.Enabled() {
		info.VPCEnabled = true
	} else {
		info.VPCReason = disabledMessage
	}
	if h.src != nil {
		info.ClusterLANForMembers = h.src.ClusterLANForMembers()
	}
	response.Success(w, info)
}

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
	if !h.requireEnabled(w) {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	svc := h.service()
	rows, err := svc.ListVPCs(r.Context(), user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]vpcView, 0, len(rows))
	for i := range rows {
		count, _ := svc.CountMembers(r.Context(), rows[i].ID)
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
	if !h.requireEnabled(w) {
		return
	}
	id, ok := parseVPCID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	svc := h.service()
	row, err := svc.GetVPC(r.Context(), id, user.ID, user.IsAdmin)
	if err != nil {
		response.FromError(w, err)
		return
	}
	count, _ := svc.CountMembers(r.Context(), row.ID)
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
	if !h.requireEnabled(w) {
		return
	}
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
	row, err := h.service().CreateVPC(r.Context(), user.ID, req.Name)
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
	if !h.requireEnabled(w) {
		return
	}
	id, ok := parseVPCID(w, r)
	if !ok {
		return
	}
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	if err := h.service().DeleteVPC(r.Context(), id, user.ID, user.IsAdmin); err != nil {
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
