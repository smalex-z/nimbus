package handlers

import (
	"errors"
	"net/http"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/service"
)

// Admin handles admin-specific endpoints.
type Admin struct {
	auth *service.AuthService
}

// NewAdmin creates a new Admin handler.
func NewAdmin(auth *service.AuthService) *Admin {
	return &Admin{auth: auth}
}

// Status handles GET /api/admin/status — public endpoint.
func (a *Admin) Status(w http.ResponseWriter, r *http.Request) {
	claimed, err := a.auth.IsAdminClaimed()
	if err != nil {
		response.InternalError(w, "Failed to check admin status")
		return
	}
	response.Success(w, map[string]bool{"claimed": claimed})
}

// Claim handles POST /api/admin/claim — requires auth middleware.
// Promotes the current user to admin. Only succeeds when no admin exists yet.
func (a *Admin) Claim(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if err := a.auth.ClaimAdmin(user.ID); err != nil {
		if errors.Is(err, service.ErrAdminAlreadyClaimed) {
			response.Error(w, http.StatusConflict, "Admin has already been claimed")
			return
		}
		response.InternalError(w, "Failed to claim admin")
		return
	}
	response.Success(w, map[string]string{"message": "Admin claimed successfully"})
}
