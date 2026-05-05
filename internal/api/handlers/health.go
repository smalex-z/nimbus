package handlers

import (
	"context"
	"net/http"
	"time"

	"nimbus/internal/api/response"
	"nimbus/internal/build"
	"nimbus/internal/proxmox"
)

// Health reports liveness plus the Proxmox cluster reachability check. Used
// by the install wizard's post-install probe and by the UI's status indicator.
type Health struct {
	px *proxmox.Client
}

func NewHealth(px *proxmox.Client) *Health { return &Health{px: px} }

type healthResponse struct {
	Status         string `json:"status"`
	Timestamp      string `json:"timestamp"`
	Version        string `json:"version"`
	ProxmoxOK      bool   `json:"proxmox_ok"`
	ProxmoxVersion string `json:"proxmox_version,omitempty"`
	ProxmoxError   string `json:"proxmox_error,omitempty"`
}

// Check handles GET /api/health.
//
// @Summary     Liveness + Proxmox reachability probe
// @Description Returns the build version, current timestamp, and the result of
// @Description a 3-second Proxmox `/version` probe. Always 200 — the
// @Description proxmox_ok field carries the dependency result so dashboards
// @Description can distinguish "Nimbus is up but Proxmox is sad" from "Nimbus
// @Description is down".
// @Tags        health
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=healthResponse}
// @Router      /health [get]
func (h *Health) Check(w http.ResponseWriter, r *http.Request) {
	out := healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Version:   build.Version,
	}

	// Brief Proxmox probe — never block the health endpoint for long.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if v, err := h.px.Version(ctx); err == nil {
		out.ProxmoxOK = true
		out.ProxmoxVersion = v
	} else {
		out.ProxmoxOK = false
		out.ProxmoxError = err.Error()
	}

	response.Success(w, out)
}
