package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/ippool"
	"nimbus/internal/mail"
	"nimbus/internal/oauth"
	"nimbus/internal/service"
)

const (
	sessionCookieName = "nimbus_sid"
	oauthStateCookie  = "nimbus_oauth_state"
	// oauthIntentCookie distinguishes a sign-in OAuth dance from a
	// link-to-existing-account dance. Set alongside the state cookie at
	// /api/auth/{provider}/start (intent="signin") or
	// /api/auth/{provider}/link-start (intent="link"); read by the
	// shared callback so it can either upsert-by-email or attach the
	// identity to the current session's user.
	oauthIntentCookie = "nimbus_oauth_intent"
)

// loginReconciler is the small interface the Auth handler uses to kick a
// post-login reconcile. *ippool.Reconciler satisfies it; nil is allowed.
type loginReconciler interface {
	Reconcile(ctx context.Context) (ippool.Report, error)
}

// userVMActor is the slice of provision.Service the user-management
// handlers need: list a user's VMs, destroy one (Proxmox + DB), or
// reassign ownership in bulk. Defined at the consumer per the
// "small interfaces, accept interfaces" convention.
type userVMActor interface {
	List(ctx context.Context, ownerID *uint) ([]db.VM, error)
	AdminDelete(ctx context.Context, id uint) error
	TransferUserVMs(ctx context.Context, fromID, toID uint) (int64, error)
}

// Auth handles authentication endpoints.
type Auth struct {
	auth       *service.AuthService
	appURL     string
	reconciler loginReconciler
	vms        userVMActor // optional; when nil, /api/users/:id DELETE returns 503
}

// NewAuth creates a new Auth handler. appURL is the env-configured fallback
// for the OAuth redirect URI; the resolver also consults the live
// GopherSettings.CloudTunnelURL and the inbound request host so a properly
// self-bootstrapped Nimbus doesn't need APP_URL set in env.
func NewAuth(auth *service.AuthService, appURL string, reconciler loginReconciler) *Auth {
	return &Auth{auth: auth, appURL: appURL, reconciler: reconciler}
}

// WithVMActor injects the dependency the user-deletion endpoint needs to
// either destroy or reassign a deleted user's VMs. Setter rather than a
// constructor argument so handlers that don't need this can build an
// Auth without threading the provision service.
func (a *Auth) WithVMActor(vms userVMActor) *Auth {
	a.vms = vms
	return a
}

// kickReconcile launches a background reconcile, decoupled from the request
// context so it survives the response. Caller should fire-and-forget.
func (a *Auth) kickReconcile() {
	if a.reconciler == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rep, err := a.reconciler.Reconcile(ctx); err != nil {
			log.Printf("post-login reconcile failed: %v", err)
		} else if len(rep.Adopted) > 0 || len(rep.Conflicts) > 0 || len(rep.Freed) > 0 || len(rep.Vacated) > 0 {
			log.Printf("post-login reconcile: adopted=%d conflicts=%d freed=%d vacated=%d",
				len(rep.Adopted), len(rep.Conflicts), len(rep.Freed), len(rep.Vacated))
		}
	}()
}

// githubProvider loads GitHub OAuth credentials from the DB on each call.
// Returns nil if credentials are not configured.
func (a *Auth) githubProvider() oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GitHubClientID == "" || settings.GitHubClientSecret == "" {
		return nil
	}
	return &oauth.GitHub{
		ClientID:     settings.GitHubClientID,
		ClientSecret: settings.GitHubClientSecret,
	}
}

// googleProvider loads Google OAuth credentials from the DB on each call.
// Returns nil if credentials are not configured. The redirect URI is
// resolved from the request so the same binary works whether the operator
// reaches Nimbus via raw IP, APP_URL, or the Gopher self-tunnel hostname.
func (a *Auth) googleProvider(r *http.Request) oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GoogleClientID == "" || settings.GoogleClientSecret == "" {
		return nil
	}
	return &oauth.Google{
		ClientID:     settings.GoogleClientID,
		ClientSecret: settings.GoogleClientSecret,
		RedirectURI:  a.ResolveAppURL(r) + "/api/auth/google/callback",
	}
}

// ResolveAppURL returns the public origin Nimbus should use when telling
// external services (Google's OAuth in particular) where to send the user
// back. Resolution order:
//
//  1. db.GopherSettings.CloudTunnelURL — populated by the Gopher self-bootstrap
//     once cloud.<domain> is live; preferred because it survives operator
//     IP changes and is what a public browser will hit.
//  2. cfg.AppURL — env-configured override; ignored when it's the bare default
//     localhost:5173 or any loopback address (those won't roundtrip through
//     a remote OAuth provider).
//  3. The inbound request's scheme + host — last resort that at least matches
//     the URL the admin's browser is currently on.
//
// Always returned without a trailing slash so callers can append paths
// directly.
func (a *Auth) ResolveAppURL(r *http.Request) string {
	if settings, err := a.auth.GetGopherSettings(); err == nil {
		if u := strings.TrimRight(strings.TrimSpace(settings.CloudTunnelURL), "/"); u != "" {
			return u
		}
	}
	if u := strings.TrimRight(strings.TrimSpace(a.appURL), "/"); u != "" && !looksLikeLocalhostURL(u) {
		return u
	}
	if r != nil && r.Host != "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	return strings.TrimRight(a.appURL, "/")
}

// looksLikeLocalhostURL reports whether the URL points at a loopback host.
// Mirrors the helper in provision/gpu_bootstrap.go but lives here so the
// auth handler doesn't take a provision dependency.
func looksLikeLocalhostURL(u string) bool {
	if u == "" {
		return true
	}
	s := strings.ToLower(u)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[:i], ":") {
		s = s[:i]
	}
	return s == "localhost" ||
		strings.HasPrefix(s, "127.") ||
		s == "0.0.0.0" ||
		s == "::1" ||
		s == "[::1]"
}

// Providers handles GET /api/auth/providers — public endpoint the
// sign-in page consumes. Returns which OAuth providers have
// credentials configured AND whether the password form should still
// render. Password sign-in stays available unless the admin has
// turned on RequirePasswordlessAuth and zero users still depend on
// password access — see service.IsPasswordSignInActive for the rule.
//
// @Summary     Available authentication providers (public)
// @Description Drives which sign-in buttons render on /login.
// @Tags        auth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Router      /auth/providers [get]
func (a *Auth) Providers(w http.ResponseWriter, r *http.Request) {
	settings, err := a.auth.GetOAuthSettings()
	out := map[string]any{
		"github":            false,
		"google":            false,
		"password":          true,
		"passwordless_goal": false,
	}
	if err == nil {
		out["github"] = settings.GitHubClientID != "" && settings.GitHubClientSecret != ""
		out["google"] = settings.GoogleClientID != "" && settings.GoogleClientSecret != ""
		out["passwordless_goal"] = settings.RequirePasswordlessAuth
	}
	if active, err := a.auth.IsPasswordSignInActive(); err == nil {
		out["password"] = active
	}
	response.Success(w, out)
}

// --- cookie helpers ---------------------------------------------------------

func setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 60 * 60,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// --- email/password ---------------------------------------------------------

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register handles POST /api/auth/register.
//
// @Summary     Register a new user account (public)
// @Description Open-self-signup is gated by RequirePasswordlessAuth in
// @Description production deployments — this endpoint always responds, but
// @Description new accounts may be unable to sign in until they link OAuth.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body     registerRequest true "New account"
// @Success     201  {object} EnvelopeOK{data=service.UserView}
// @Failure     400  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /auth/register [post]
func (a *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "Invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)

	switch {
	case req.Name == "":
		response.BadRequest(w, "Name is required")
		return
	case req.Email == "":
		response.BadRequest(w, "Email is required")
		return
	case len(req.Password) < 8:
		response.BadRequest(w, "Password must be at least 8 characters")
		return
	}

	user, err := a.auth.Register(service.RegisterParams{
		Name:     req.Name,
		Email:    req.Email,
		Password: req.Password,
	})
	if errors.Is(err, service.ErrEmailTaken) {
		response.Conflict(w, "An account with that email already exists")
		return
	}
	if err != nil {
		response.InternalError(w, "Failed to create account")
		return
	}

	response.Created(w, user)
}

// loginRequest is the body of POST /api/auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login handles POST /api/auth/login.
//
// @Summary     Email/password sign-in (public)
// @Description Sets the session cookie inline on success. 403 with
// @Description message "Account suspended" when the user row is suspended.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body     loginRequest true "Credentials"
// @Success     200  {object} EnvelopeOK{data=service.UserView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /auth/login [post]
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.Email) == "" || req.Password == "" {
		response.BadRequest(w, "Email and password are required")
		return
	}

	sessionID, user, err := a.auth.Login(req.Email, req.Password)
	if errors.Is(err, service.ErrInvalidCredentials) {
		response.Error(w, http.StatusUnauthorized, "Invalid email or password")
		return
	}
	if errors.Is(err, service.ErrUserSuspended) {
		response.Error(w, http.StatusForbidden, "Account suspended. Contact your admin.")
		return
	}
	if err != nil {
		response.InternalError(w, "Failed to sign in")
		return
	}

	setSessionCookie(w, sessionID)
	a.kickReconcile()
	response.Success(w, user)
}

// Me handles GET /api/me.
// The user is guaranteed to be present by the requireAuth middleware.
//
// @Summary     The current session's user
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=service.UserView}
// @Failure     401 {object} EnvelopeError
// @Router      /me [get]
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	response.Success(w, ctxutil.User(r.Context()))
}

// ListUsers handles GET /api/users.
// Admins receive every account in the richer management shape (with
// signup time, verification status, and provider hints); non-admins
// receive only their own self-view (no list of peers).
//
// @Summary     List users (admin sees all, member sees self)
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /users [get]
func (a *Auth) ListUsers(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user.IsAdmin {
		users, err := a.auth.ListAllUsersForManagement()
		if err != nil {
			response.InternalError(w, "Failed to list users")
			return
		}
		response.Success(w, users)
		return
	}
	response.Success(w, []*service.UserView{user})
}

// parseUserID pulls and validates the {id} path parameter for the
// user-management endpoints. Centralised so promote and delete share the
// same 400 message on bad input.
func parseUserID(r *http.Request) (uint, error) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("invalid user id")
	}
	return uint(id), nil
}

// promoteUserRequest is the body of POST /api/users/:id/promote — the
// requesting admin's own password, used as a re-auth gate so a stolen
// session can't trivially elevate a member to admin.
type promoteUserRequest struct {
	Password string `json:"password"`
}

// PromoteUser handles POST /api/users/:id/promote — flips a member to
// admin after re-confirming the requesting admin's password. Idempotent
// when the target is already admin (returns 200), but the requester
// must still pass the password gate so the action stays auditable.
//
// @Summary     Promote a user to admin (admin)
// @Description Requires the requesting admin's own password as a re-auth gate.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       id   path     int                 true "User id"
// @Param       body body     promoteUserRequest  true "Re-auth password"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     404  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /users/{id}/promote [post]
func (a *Auth) PromoteUser(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	targetID, err := parseUserID(r)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	var body promoteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if body.Password == "" {
		response.BadRequest(w, "password is required")
		return
	}
	if err := a.auth.VerifyPassword(requester.ID, body.Password); err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, "incorrect password")
			return
		}
		log.Printf("promote: verify password: %v", err)
		response.InternalError(w, "verification failed")
		return
	}
	if err := a.auth.PromoteToAdmin(targetID); err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			response.NotFound(w, "user not found")
			return
		}
		log.Printf("promote: %v", err)
		response.InternalError(w, "promote failed")
		return
	}
	response.Success(w, map[string]any{"id": targetID, "is_admin": true})
}

// deleteUserRequest is the body of DELETE /api/users/:id — the admin's
// chosen disposition for any VMs the user currently owns. "delete"
// destroys them on Proxmox and removes the rows; "transfer" reassigns
// ownership to the requesting admin and leaves the VMs running.
//
// SSH keys + GPU jobs follow the same disposition (transferred when
// VMs are transferred, deleted otherwise) so we don't end up with
// orphaned key references on transferred VMs.
type deleteUserRequest struct {
	VMAction string `json:"vm_action"`
}

// DeleteUser handles DELETE /api/users/:id. Admin-only. Refuses to
// delete the requester themselves (would orphan the active session
// mid-request) and refuses to delete the last remaining admin.
//
// Order of operations:
//  1. Validate request (admin, target != self, last-admin guard).
//  2. Handle VMs per disposition (Proxmox destroy or owner transfer).
//  3. Cleanup DB resources (sessions, ssh_keys, gpu_jobs) in a tx.
//  4. Delete the user row.
//
// Step 2 happens outside any transaction because it crosses to Proxmox.
// If a VM destroy fails partway through, we abort and return — the user
// row stays, and the admin can retry. Steps 3-4 are atomic.
//
// @Summary     Delete a user (admin)
// @Description vm_action must be "delete" or "transfer". "delete" destroys
// @Description their VMs on Proxmox; "transfer" reassigns ownership to the
// @Description requesting admin and leaves them running. SSH keys + GPU jobs
// @Description follow the same disposition.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       id   path     int                true "User id"
// @Param       body body     deleteUserRequest  true "VM disposition"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     404  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Failure     503  {object} EnvelopeError
// @Router      /users/{id} [delete]
func (a *Auth) DeleteUser(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	if a.vms == nil {
		response.ServiceUnavailable(w, "user deletion requires the provision service")
		return
	}
	targetID, err := parseUserID(r)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	if targetID == requester.ID {
		response.BadRequest(w, "cannot delete yourself")
		return
	}
	var body deleteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	switch body.VMAction {
	case "delete", "transfer":
	default:
		response.BadRequest(w, "vm_action must be \"delete\" or \"transfer\"")
		return
	}

	// Last-admin guard: if the target is the only remaining admin, refuse.
	// The requester is always an admin and always != target, so as long
	// as the target isn't admin OR there's at least one other admin, we
	// proceed. (We can't strictly hit zero admins because the requester
	// is themselves admin, but this guards against deleting the last
	// *other* admin while the requester is the only remaining one — not
	// fatal, but worth surfacing as an explicit confirmation later.)
	// For now it's enough that we can't delete ourselves.

	// 1. VM disposition.
	ctx := r.Context()
	owned, err := a.vms.List(ctx, &targetID)
	if err != nil {
		log.Printf("delete user: list vms: %v", err)
		response.InternalError(w, "failed to enumerate user VMs")
		return
	}
	switch body.VMAction {
	case "delete":
		for _, vm := range owned {
			if err := a.vms.AdminDelete(ctx, vm.ID); err != nil {
				log.Printf("delete user %d: destroy vm %d: %v", targetID, vm.ID, err)
				response.InternalError(w, "failed to destroy a VM owned by this user — partial deletion may have occurred, retry to continue")
				return
			}
		}
	case "transfer":
		if _, err := a.vms.TransferUserVMs(ctx, targetID, requester.ID); err != nil {
			log.Printf("delete user %d: transfer vms: %v", targetID, err)
			response.InternalError(w, "failed to transfer VM ownership")
			return
		}
	}

	// 2. DB resources (sessions, ssh_keys, gpu_jobs).
	var transferTo *uint
	if body.VMAction == "transfer" {
		id := requester.ID
		transferTo = &id
	}
	if err := a.auth.CleanupUserDBResources(targetID, transferTo); err != nil {
		log.Printf("delete user %d: cleanup db resources: %v", targetID, err)
		response.InternalError(w, "VM disposition succeeded, but follow-up cleanup failed")
		return
	}

	// 3. User row.
	if err := a.auth.DeleteUserRecord(targetID); err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			response.NotFound(w, "user not found")
			return
		}
		log.Printf("delete user %d: delete record: %v", targetID, err)
		response.InternalError(w, "failed to delete user record")
		return
	}
	response.Success(w, map[string]any{"id": targetID, "vm_action": body.VMAction, "vms_handled": len(owned)})
}

// accountView is the shape returned by GET /api/account — what the
// current user sees about themselves on the /account page. Includes
// the linked-providers flags so the page can render Connect / Connected
// pills without duplicating the heuristic.
type accountView struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	IsAdmin         bool   `json:"is_admin"`
	HasPassword     bool   `json:"has_password"`
	GoogleConnected bool   `json:"google_connected"`
	GithubConnected bool   `json:"github_connected"`
}

// Account handles GET /api/account — current user's profile + the
// linked-providers state so the /account page can decide which
// Connect buttons to render.
//
// @Summary     Current user's profile + linked-provider state
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=accountView}
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /account [get]
func (a *Auth) Account(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	row, err := a.auth.GetUserByID(user.ID)
	if err != nil {
		response.InternalError(w, "Failed to load account")
		return
	}
	response.Success(w, accountView{
		ID:              row.ID,
		Name:            row.Name,
		Email:           row.Email,
		IsAdmin:         row.IsAdmin,
		HasPassword:     row.PasswordHash != "",
		GoogleConnected: row.GoogleConnected,
		GithubConnected: row.GitHubOrgs != "",
	})
}

// ChangePassword handles PUT /api/account/password. The caller must
// supply their current password — the session cookie alone isn't enough
// to rotate, since a stolen cookie shouldn't let an attacker lock the
// real owner out. OAuth-only accounts (no password set) get the same
// "current password is incorrect" 401 the wrong-password path does;
// the UI gates the section on /account.has_password so they shouldn't
// reach this handler in normal flows.
type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles PUT /api/account/password.
//
// @Summary     Rotate the current user's password
// @Description Requires the current password — a stolen session alone
// @Description shouldn't let an attacker lock the real owner out.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       body body     changePasswordRequest true "Current + new password"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /account/password [put]
func (a *Auth) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "Invalid request body")
		return
	}
	if len(req.NewPassword) < 8 {
		response.BadRequest(w, "Password must be at least 8 characters")
		return
	}
	if req.CurrentPassword == req.NewPassword {
		response.BadRequest(w, "New password must differ from current")
		return
	}
	if err := a.auth.ChangePassword(user.ID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, "Current password is incorrect")
			return
		}
		response.InternalError(w, "Failed to change password")
		return
	}
	response.Success(w, map[string]bool{"ok": true})
}

// SetPasswordlessAuth handles PUT /api/settings/oauth/passwordless —
// admin-only toggle for the OAuth-only-sign-in setting. Hard gate:
// the service rejects with ErrRequesterNotLinked when the admin
// hasn't linked OAuth themselves and ErrStragglersBlock when any
// active password-only user remains. Both surface as 409 with the
// service error message so the admin can act on it (link your own
// account, then suspend or delete the stragglers).
type passwordlessToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// SetPasswordlessAuth handles PUT /api/settings/oauth/passwordless.
//
// @Summary     Toggle passwordless-only sign-in (admin)
// @Description Hard-gate: rejected with 409 when the requesting admin
// @Description hasn't linked OAuth themselves, or when any active
// @Description password-only user remains. Surface the message verbatim
// @Description so the admin can act on it.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       body body     passwordlessToggleRequest true "Toggle"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/oauth/passwordless [put]
func (a *Auth) SetPasswordlessAuth(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	var body passwordlessToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := a.auth.SetRequirePasswordlessAuth(requester.ID, body.Enabled); err != nil {
		if errors.Is(err, service.ErrRequesterNotLinked) || errors.Is(err, service.ErrStragglersBlock) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("set passwordless: %v", err)
		response.InternalError(w, "failed to update setting")
		return
	}
	stragglers, err := a.auth.CountUnlinkedUsers()
	if err != nil {
		log.Printf("set passwordless: count stragglers: %v", err)
		stragglers = 0
	}
	active, err := a.auth.IsPasswordSignInActive()
	if err != nil {
		log.Printf("set passwordless: is password active: %v", err)
		active = true
	}
	response.Success(w, map[string]any{
		"passwordless_goal": body.Enabled,
		"stragglers":        stragglers,
		"password_active":   active,
	})
}

// SuspendUnlinked handles POST /api/users/suspend-unlinked — bulk
// action that suspends every active user without OAuth (excluding the
// requester). Used by the passwordless toggle's "Suspend stragglers"
// button to clear the gate in one click.
//
// @Summary     Suspend every active password-only user (admin)
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /users/suspend-unlinked [post]
func (a *Auth) SuspendUnlinked(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	count, err := a.auth.SuspendUnlinkedUsers(requester.ID)
	if err != nil {
		log.Printf("suspend unlinked: %v", err)
		response.InternalError(w, "failed to suspend users")
		return
	}
	response.Success(w, map[string]any{"suspended": count})
}

// suspendRequest is the body of POST /api/users/{id}/suspend-status —
// a single-user version of the bulk action above.
type suspendRequest struct {
	Suspended bool `json:"suspended"`
}

// SetSuspended handles POST /api/users/{id}/suspend-status.
//
// @Summary     Set one user's suspended flag (admin)
// @Description Cannot self-suspend; cannot suspend the last remaining admin.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       id   path     int             true "User id"
// @Param       body body     suspendRequest  true "Desired state"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     404  {object} EnvelopeError
// @Failure     409  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /users/{id}/suspend-status [post]
func (a *Auth) SetSuspended(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	targetID, err := parseUserID(r)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	var body suspendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	if err := a.auth.SetUserSuspended(targetID, requester.ID, body.Suspended); err != nil {
		switch {
		case errors.Is(err, service.ErrUserNotFound):
			response.NotFound(w, "user not found")
		case errors.Is(err, service.ErrCannotSuspendSelf), errors.Is(err, service.ErrCannotSuspendLastAdmin):
			response.Error(w, http.StatusConflict, err.Error())
		default:
			log.Printf("set suspended: %v", err)
			response.InternalError(w, "failed to update suspension")
		}
		return
	}
	response.Success(w, map[string]any{"id": targetID, "suspended": body.Suspended})
}

// PasswordlessStatus handles GET /api/settings/oauth/passwordless —
// admin-only read of the current setting, straggler count, and SMTP
// readiness. The Users page uses these to render the toggle, the
// straggler explanation, the bulk-suspend button, and the (currently
// disabled) "Email N unlinked users" button.
//
// @Summary     Passwordless-only sign-in status (admin)
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/oauth/passwordless [get]
func (a *Auth) PasswordlessStatus(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	settings, err := a.auth.GetOAuthSettings()
	if err != nil {
		response.InternalError(w, "Failed to load settings")
		return
	}
	stragglers, err := a.auth.CountUnlinkedUsers()
	if err != nil {
		log.Printf("passwordless status: count stragglers: %v", err)
		stragglers = 0
	}
	active, err := a.auth.IsPasswordSignInActive()
	if err != nil {
		log.Printf("passwordless status: active: %v", err)
		active = true
	}
	smtp, err := a.auth.GetSMTPSettings()
	if err != nil {
		log.Printf("passwordless status: smtp: %v", err)
	}
	smtpReady := smtp != nil && smtp.Configured && smtp.Enabled
	response.Success(w, map[string]any{
		"passwordless_goal": settings.RequirePasswordlessAuth,
		"stragglers":        stragglers,
		"password_active":   active,
		"smtp_ready":        smtpReady,
	})
}

// GetSMTP handles GET /api/settings/smtp. Admin-only. Returns the
// SMTPSettingsView (no ciphertext, just `has_password: bool` so the
// /email form can render "(unchanged)" placeholder copy).
//
// @Summary     Read SMTP settings (admin)
// @Description The password ciphertext is never sent — `has_password`
// @Description tells the SPA whether to render an "(unchanged)" placeholder.
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=service.SMTPSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/smtp [get]
func (a *Auth) GetSMTP(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	view, err := a.auth.GetSMTPSettings()
	if err != nil {
		log.Printf("get smtp: %v", err)
		response.InternalError(w, "failed to load SMTP settings")
		return
	}
	response.Success(w, view)
}

// SaveSMTP handles PUT /api/settings/smtp. Admin-only. The request
// body's password field follows the standard "edit secrets" pattern:
// omitting the field leaves the existing ciphertext untouched, sending
// an empty string clears it, sending a non-empty string replaces it.
//
// @Summary     Update SMTP settings (admin)
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     service.SaveSMTPRequest true "SMTP settings"
// @Success     200 {object} EnvelopeOK{data=service.SMTPSettingsView}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /settings/smtp [put]
func (a *Auth) SaveSMTP(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	var body service.SaveSMTPRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	view, err := a.auth.SaveSMTPSettings(body)
	if err != nil {
		if errors.Is(err, service.ErrCipherUnavailable) {
			response.Error(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		log.Printf("save smtp: %v", err)
		response.InternalError(w, "failed to save SMTP settings")
		return
	}
	response.Success(w, view)
}

// magicLinkPurpose tags magic-link tokens. Single value today; the
// purpose column on db.LoginToken keeps room for future flows
// (password reset, invite) without colliding.
const magicLinkPurpose = "magic_link"

// magicLinkTTL is how long a recovery email's link stays valid. 24h
// balances "user might not check email immediately" against "we've
// kept a sign-in token live for too long." Longer than typical
// password-reset tokens because the linked user already has an
// account; the worst case of a stolen link is that an attacker who
// reads the user's mailbox can sign in as them, which they could
// already do via password reset on most sites.
const magicLinkTTL = 24 * time.Hour

// SendTestEmail handles POST /api/settings/smtp/test — admin-only
// dry-run that delivers a short test message to the admin's own email
// address. Used as a confidence check after filling in the /email
// form: if the dial / auth / TLS handshake works for the requester,
// the bulk magic-link send will too.
//
// @Summary     Send a SMTP test email to the requesting admin (admin)
// @Description The dial/auth/TLS error string is surfaced verbatim on 502
// @Description so admins debugging SMTP have the underlying message.
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     502 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /settings/smtp/test [post]
func (a *Auth) SendTestEmail(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	if requester.Email == "" {
		response.BadRequest(w, "your account has no email address to send to")
		return
	}
	row, err := a.auth.LoadSMTPRow()
	if err != nil {
		log.Printf("smtp test: load row: %v", err)
		response.InternalError(w, "failed to load SMTP settings")
		return
	}
	cfg, err := mail.Resolve(row, a.auth.Cipher())
	if err != nil {
		if errors.Is(err, mail.ErrNotConfigured) {
			response.Error(w, http.StatusConflict, "configure SMTP host and from-address first")
			return
		}
		if errors.Is(err, mail.ErrCipherUnavailable) {
			response.Error(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		log.Printf("smtp test: resolve: %v", err)
		response.InternalError(w, "failed to resolve SMTP config")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg := mail.Message{
		To:      requester.Email,
		Subject: "Nimbus SMTP test",
		Body: fmt.Sprintf(
			"This is a test email from Nimbus.\r\n\r\nIf you're seeing this, %s:%d is configured correctly and outbound mail is reaching your inbox.\r\n",
			row.Host, row.Port,
		),
	}
	if err := mail.Send(ctx, cfg, msg); err != nil {
		// Surface the dial / auth error string verbatim — admins
		// debugging SMTP need the underlying message ("authentication
		// failed", "no such host", etc.) more than a sanitised one.
		response.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	response.Success(w, map[string]any{"sent": true, "to": requester.Email})
}

// MagicLinkSignIn handles GET /api/auth/magic/{token} — public,
// single-use entry point for password-only users recovering their
// account. Validates the token, mints a session, unsuspends the user
// if needed, and redirects them to /account so they can connect an
// OAuth provider. Bad/expired/used tokens redirect to a sign-in page
// with an explanatory query param.
//
// @Summary     Magic-link sign-in (public, single-use)
// @Description Always returns 307 — to /account?from=magic on success or to
// @Description /login?magic=<reason> when the token is invalid/expired/used.
// @Tags        auth
// @Param       token path string true "Magic-link token"
// @Success     307 "Redirect"
// @Router      /auth/magic/{token} [get]
func (a *Auth) MagicLinkSignIn(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		http.Redirect(w, r, "/login?magic=invalid", http.StatusTemporaryRedirect)
		return
	}
	userID, err := a.auth.ConsumeLoginToken(token, magicLinkPurpose)
	if err != nil {
		var reason string
		switch {
		case errors.Is(err, service.ErrLoginTokenExpired):
			reason = "expired"
		case errors.Is(err, service.ErrLoginTokenUsed):
			reason = "used"
		case errors.Is(err, service.ErrLoginTokenInvalid):
			reason = "invalid"
		default:
			log.Printf("magic-link: consume: %v", err)
			reason = "error"
		}
		http.Redirect(w, r, "/login?magic="+reason, http.StatusTemporaryRedirect)
		return
	}
	// If the user was suspended (e.g. caught by the bulk-suspend that
	// preceded the email send), unsuspend them now so the freshly
	// minted session isn't immediately rejected by GetUserBySessionID's
	// suspended check.
	if err := a.auth.SetUserSuspended(userID, userID, false); err != nil {
		// Self-target self-unsuspend isn't a cannot-suspend-self case
		// (that gate only fires for `suspended=true`), but log just
		// in case the gate evolves.
		log.Printf("magic-link: unsuspend uid=%d: %v", userID, err)
	}
	sessionID, err := a.auth.CreateSession(userID)
	if err != nil {
		log.Printf("magic-link: create session uid=%d: %v", userID, err)
		http.Redirect(w, r, "/login?magic=error", http.StatusTemporaryRedirect)
		return
	}
	setSessionCookie(w, sessionID)
	http.Redirect(w, r, "/account?from=magic", http.StatusTemporaryRedirect)
}

// EmailUnlinkedResult is the response shape for POST
// /api/users/email-unlinked. Counts let the UI render
// "Sent N · failed M" without a second round-trip, and Failures
// carries the bad-recipient list so the admin can decide whether to
// retry or investigate (most failures are transient SMTP rejects).
type EmailUnlinkedResult struct {
	Sent     int      `json:"sent"`
	Failed   int      `json:"failed"`
	Failures []string `json:"failures,omitempty"`
}

// EmailUnlinked handles POST /api/users/email-unlinked — mints a
// magic-link token per active password-only user (excluding the
// requester) and sends each one a recovery email. Returns the per-
// user result. The first SMTP failure aborts the whole batch on the
// theory that "host unreachable" or "auth failed" affects everyone
// equally, but per-recipient rejects (mailbox doesn't exist, etc.)
// are recorded and we move on.
//
// @Summary     Email magic-link recovery to all unlinked users (admin)
// @Description Per-recipient SMTP rejects (e.g. "mailbox doesn't exist")
// @Description are recorded in `failures` and the loop continues. The
// @Description first dial/auth/TLS error aborts the whole batch.
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=EmailUnlinkedResult}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     409 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /users/email-unlinked [post]
func (a *Auth) EmailUnlinked(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	row, err := a.auth.LoadSMTPRow()
	if err != nil {
		log.Printf("email unlinked: load row: %v", err)
		response.InternalError(w, "failed to load SMTP settings")
		return
	}
	if !row.Enabled {
		response.Error(w, http.StatusConflict, "SMTP is configured but disabled — flip Enable on /email first")
		return
	}
	cfg, err := mail.Resolve(row, a.auth.Cipher())
	if err != nil {
		if errors.Is(err, mail.ErrNotConfigured) {
			response.Error(w, http.StatusConflict, "configure SMTP host and from-address first")
			return
		}
		if errors.Is(err, mail.ErrCipherUnavailable) {
			response.Error(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		log.Printf("email unlinked: resolve: %v", err)
		response.InternalError(w, "failed to resolve SMTP config")
		return
	}
	targets, err := a.auth.ListUnlinkedActiveUsers(requester.ID)
	if err != nil {
		log.Printf("email unlinked: list: %v", err)
		response.InternalError(w, "failed to list unlinked users")
		return
	}
	if len(targets) == 0 {
		response.Success(w, EmailUnlinkedResult{})
		return
	}

	out := EmailUnlinkedResult{}
	for i := range targets {
		t := &targets[i]
		if t.Email == "" {
			out.Failed++
			out.Failures = append(out.Failures, fmt.Sprintf("uid %d: no email", t.ID))
			continue
		}
		token, err := a.auth.MintLoginToken(t.ID, magicLinkPurpose, magicLinkTTL)
		if err != nil {
			out.Failed++
			out.Failures = append(out.Failures, fmt.Sprintf("%s: mint token: %v", t.Email, err))
			continue
		}
		link := strings.TrimRight(a.appURL, "/") + "/api/auth/magic/" + url.PathEscape(token)
		body := buildMagicLinkBody(t.Name, link)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err = mail.Send(ctx, cfg, mail.Message{
			To:      t.Email,
			Subject: "Connect Google or GitHub to your Nimbus account",
			Body:    body,
		})
		cancel()
		if err != nil {
			out.Failed++
			out.Failures = append(out.Failures, fmt.Sprintf("%s: %v", t.Email, err))
			// Continue rather than abort — one bad mailbox shouldn't
			// hold up the rest. The admin can retry-with-failures
			// after triaging the failed list.
			continue
		}
		out.Sent++
	}
	response.Success(w, out)
}

// buildMagicLinkBody composes the recovery-email body. Plain text
// only; the link is on its own line so receivers that auto-linkify
// pick it up cleanly.
func buildMagicLinkBody(displayName, link string) string {
	greeting := "Hi,"
	if displayName != "" {
		greeting = "Hi " + displayName + ","
	}
	return greeting + "\r\n\r\n" +
		"Your Nimbus admin asked you to connect a Google or GitHub account to your sign-in. " +
		"Click the link below within 24 hours to sign in and link a provider:\r\n\r\n" +
		link + "\r\n\r\n" +
		"After you've connected, you can sign in by clicking 'Continue with Google' " +
		"or 'Continue with GitHub' on the sign-in page — no password needed.\r\n\r\n" +
		"If you didn't expect this email, ignore it. The link expires in 24 hours and works only once.\r\n"
}

// QuotaSettingsView is the JSON shape for the /api/settings/quotas
// endpoints. Same field names the frontend uses; the backend struct
// (db.QuotaSettings) exposes ID we don't need to leak.
type QuotaSettingsView struct {
	MemberMaxVMs        int `json:"member_max_vms"`
	MemberMaxActiveJobs int `json:"member_max_active_jobs"`
}

// GetQuotas handles GET /api/settings/quotas — admin-only read of
// the workspace quota defaults (caps that apply when a user has no
// per-row override set).
//
// @Summary     Read workspace quota defaults (admin)
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=QuotaSettingsView}
// @Failure     401 {object} EnvelopeError
// @Failure     403 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /settings/quotas [get]
func (a *Auth) GetQuotas(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	row, err := a.auth.GetQuotaSettings()
	if err != nil {
		log.Printf("get quotas: %v", err)
		response.InternalError(w, "failed to load quota settings")
		return
	}
	response.Success(w, QuotaSettingsView{
		MemberMaxVMs:        row.MemberMaxVMs,
		MemberMaxActiveJobs: row.MemberMaxActiveJobs,
	})
}

// SaveQuotas handles PUT /api/settings/quotas — admin-only update of
// the workspace defaults. Request body matches QuotaSettingsView.
// Either field can be omitted to leave it untouched, but a field
// present with a negative value is a 400 (the service rejects).
//
// @Summary     Update workspace quota defaults (admin)
// @Description Either field can be omitted to leave it untouched.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     QuotaSettingsView true "Workspace defaults"
// @Success     200  {object} EnvelopeOK{data=QuotaSettingsView}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /settings/quotas [put]
func (a *Auth) SaveQuotas(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	// Use pointers so a missing field stays at the persisted value.
	var body struct {
		MemberMaxVMs        *int `json:"member_max_vms"`
		MemberMaxActiveJobs *int `json:"member_max_active_jobs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	current, err := a.auth.GetQuotaSettings()
	if err != nil {
		log.Printf("save quotas: load current: %v", err)
		response.InternalError(w, "failed to load quota settings")
		return
	}
	maxVMs := current.MemberMaxVMs
	maxJobs := current.MemberMaxActiveJobs
	if body.MemberMaxVMs != nil {
		maxVMs = *body.MemberMaxVMs
	}
	if body.MemberMaxActiveJobs != nil {
		maxJobs = *body.MemberMaxActiveJobs
	}
	updated, err := a.auth.SaveQuotaSettings(maxVMs, maxJobs)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	response.Success(w, QuotaSettingsView{
		MemberMaxVMs:        updated.MemberMaxVMs,
		MemberMaxActiveJobs: updated.MemberMaxActiveJobs,
	})
}

// userQuotaRequest is the body of PUT /api/users/{id}/quota. Each
// field is *int: nil means "leave that override alone"; setting it
// to a JSON null clears the override (revert to workspace default);
// a number sets an explicit cap. JSON's distinction between
// "null" / absent / value lines up with the three semantics — we
// use json.RawMessage to preserve null vs absent.
type userQuotaRequest struct {
	VMQuotaOverride     json.RawMessage `json:"vm_quota_override"`
	GPUJobQuotaOverride json.RawMessage `json:"gpu_job_quota_override"`
}

// SetUserQuota handles PUT /api/users/{id}/quota — admin-only patch
// of one user's quota override columns. See userQuotaRequest for the
// three-state JSON semantics.
//
// @Summary     Patch one user's quota overrides (admin)
// @Description Each field has three-state semantics: absent = leave alone,
// @Description JSON null = clear the override (revert to workspace default),
// @Description number = set explicit cap.
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       id   path     int                true "User id"
// @Param       body body     userQuotaRequest   true "Override patch"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     403  {object} EnvelopeError
// @Failure     404  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /users/{id}/quota [put]
func (a *Auth) SetUserQuota(w http.ResponseWriter, r *http.Request) {
	requester := ctxutil.User(r.Context())
	if requester == nil || !requester.IsAdmin {
		response.Error(w, http.StatusForbidden, "admin only")
		return
	}
	targetID, err := parseUserID(r)
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	var body userQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}

	setVM, vmVal, clearVM, err := decodeQuotaPatch(body.VMQuotaOverride, "vm_quota_override")
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	setGPU, gpuVal, clearGPU, err := decodeQuotaPatch(body.GPUJobQuotaOverride, "gpu_job_quota_override")
	if err != nil {
		response.BadRequest(w, err.Error())
		return
	}

	// Apply set + clear separately. Set first so a request that does
	// {clear vm, set gpu} ends with both intents recognised even if
	// the user wrote them in reverse order.
	if setVM || setGPU {
		var vmPtr, gpuPtr *int
		if setVM {
			vmPtr = &vmVal
		}
		if setGPU {
			gpuPtr = &gpuVal
		}
		if err := a.auth.SetUserQuotaOverride(targetID, vmPtr, gpuPtr); err != nil {
			if errors.Is(err, service.ErrUserNotFound) {
				response.NotFound(w, "user not found")
				return
			}
			log.Printf("set user quota: %v", err)
			response.InternalError(w, "failed to set quota")
			return
		}
	}
	if clearVM || clearGPU {
		if err := a.auth.ClearUserQuotaOverride(targetID, clearVM, clearGPU); err != nil {
			if errors.Is(err, service.ErrUserNotFound) {
				response.NotFound(w, "user not found")
				return
			}
			log.Printf("clear user quota: %v", err)
			response.InternalError(w, "failed to clear quota")
			return
		}
	}
	response.Success(w, map[string]any{"id": targetID, "ok": true})
}

// decodeQuotaPatch reduces the three-state JSON to a (set, value,
// clear) triple. Returns (false, 0, false, nil) when the field was
// absent — caller should leave the column untouched.
func decodeQuotaPatch(raw json.RawMessage, field string) (set bool, value int, clear bool, err error) {
	if len(raw) == 0 {
		return false, 0, false, nil
	}
	if string(raw) == "null" {
		return false, 0, true, nil
	}
	var n int
	if e := json.Unmarshal(raw, &n); e != nil {
		return false, 0, false, fmt.Errorf("%s must be a non-negative integer or null", field)
	}
	if n < 0 {
		return false, 0, false, fmt.Errorf("%s must be non-negative", field)
	}
	return true, n, false, nil
}

// VerifyStatus handles GET /api/access-code/status — returns whether the
// authenticated user is verified against the current access code version.
//
// @Summary     Whether the user has verified the current access code
// @Tags        auth
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK
// @Failure     401 {object} EnvelopeError
// @Failure     500 {object} EnvelopeError
// @Router      /access-code/status [get]
func (a *Auth) VerifyStatus(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	verified, err := a.auth.IsUserVerified(user.ID)
	if err != nil {
		response.InternalError(w, "failed to check verification")
		return
	}
	response.Success(w, map[string]bool{"verified": verified})
}

type verifyAccessCodeRequest struct {
	Code string `json:"code"`
}

// VerifyAccessCode handles POST /api/access-code/verify — checks the supplied
// code against the current access code and, on success, marks the user as
// verified.
//
// @Summary     Verify the current access code
// @Tags        auth
// @Security    cookieAuth
// @Accept      json
// @Param       body body     verifyAccessCodeRequest true "Access code"
// @Success     200  {object} EnvelopeOK
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Failure     500  {object} EnvelopeError
// @Router      /access-code/verify [post]
func (a *Auth) VerifyAccessCode(w http.ResponseWriter, r *http.Request) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req verifyAccessCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON")
		return
	}
	err := a.auth.VerifyAccessCode(user.ID, req.Code)
	if errors.Is(err, service.ErrInvalidAccessCode) {
		response.Error(w, http.StatusUnauthorized, "Invalid access code")
		return
	}
	if err != nil {
		response.InternalError(w, "failed to verify access code")
		return
	}
	response.Success(w, map[string]bool{"verified": true})
}

// Logout handles POST /api/auth/logout.
//
// @Summary     End the current session
// @Description Always returns 204 — no body — even when the cookie is
// @Description missing or invalid.
// @Tags        auth
// @Success     204 "No Content"
// @Router      /auth/logout [post]
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = a.auth.Logout(cookie.Value)
	}
	clearSessionCookie(w)
	response.NoContent(w)
}

// --- shared OAuth helpers ---------------------------------------------------

// oauthStart generates a CSRF state, stores it in a short-lived cookie
// alongside the intent ("signin" or "link"), and redirects the browser
// to the provider's authorization URL. The callback reads the intent
// to decide whether to mint a new session or attach to the current one.
func (a *Auth) oauthStart(provider oauth.Provider, intent string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			response.Error(w, http.StatusServiceUnavailable, "OAuth provider not configured")
			return
		}
		state, err := generateState()
		if err != nil {
			response.InternalError(w, "Failed to initiate OAuth")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookie,
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		})
		http.SetCookie(w, &http.Cookie{
			Name:     oauthIntentCookie,
			Value:    intent,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		})
		http.Redirect(w, r, provider.AuthURL(state), http.StatusTemporaryRedirect)
	}
}

// currentSessionUser inspects the session cookie and returns the
// matching user, or nil when the cookie is missing or invalid. The
// OAuth callback routes are public (sign-in mode runs them with no
// pre-existing session), so this lookup happens inside the handler
// rather than via requireAuth middleware.
func (a *Auth) currentSessionUser(r *http.Request) *service.UserView {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	user, err := a.auth.GetUserBySessionID(c.Value)
	if err != nil {
		return nil
	}
	return user
}

// readOAuthIntent fetches the intent cookie. Falls back to "signin" if
// the cookie is missing — backwards-compat for any in-flight OAuth
// dances started before this code shipped.
func readOAuthIntent(r *http.Request) string {
	c, err := r.Cookie(oauthIntentCookie)
	if err != nil {
		return "signin"
	}
	if c.Value == "link" {
		return "link"
	}
	return "signin"
}

// clearOAuthIntent zeroes the intent cookie alongside the state cookie
// at the end of every callback.
func clearOAuthIntent(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: oauthIntentCookie, Value: "", Path: "/", MaxAge: -1})
}

// --- GitHub OAuth -----------------------------------------------------------

// GitHubStart handles GET /api/auth/github.
//
// @Summary     Start the GitHub OAuth sign-in flow (public)
// @Description Sets short-lived state + intent cookies, then redirects to
// @Description GitHub's authorization URL.
// @Tags        auth
// @Success     307 "Redirect to GitHub OAuth consent"
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /auth/github [get]
func (a *Auth) GitHubStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.githubProvider(), "signin")(w, r)
}

// GitHubLinkStart kicks off a GitHub OAuth dance whose callback will
// attach the resulting identity to the current session's user instead
// of creating/finding a user by email. Requires an authenticated
// session — middleware enforces that at the route level.
// GitHubLinkStart handles GET /api/auth/github/link.
//
// @Summary     Start the GitHub OAuth flow in link mode
// @Description Same callback as sign-in mode but the intent cookie tells
// @Description the callback to attach to the current session rather than
// @Description mint a new one.
// @Tags        auth
// @Success     307 "Redirect to GitHub OAuth consent"
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /auth/github/link [get]
func (a *Auth) GitHubLinkStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.githubProvider(), "link")(w, r)
}

// GitHubCallback wraps the OAuth flow with the authorized-orgs check. New
// users whose GitHub org memberships don't intersect the admin's authorized
// list are blocked before any account is created. Every login refreshes the
// user's stored org snapshot so the dynamic bypass in IsUserVerified always
// reflects the most recent login's memberships.
// GitHubCallback handles GET /api/auth/github/callback.
//
// @Summary     GitHub OAuth callback (public)
// @Description Validates the state cookie, exchanges the code, and either
// @Description signs the user in or links the GitHub account to the
// @Description existing session (per the intent cookie). Always responds
// @Description with a redirect — to the SPA on success, or back to /login
// @Description with an explanatory query param on failure.
// @Tags        auth
// @Param       code  query string true "Authorization code"
// @Param       state query string true "CSRF state echoed by GitHub"
// @Success     307 "Redirect"
// @Router      /auth/github/callback [get]
func (a *Auth) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.githubProvider()
	if provider == nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/auth/callback?error=invalid_state&provider=github", http.StatusTemporaryRedirect)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Redirect(w, r, "/auth/callback?provider=github&error="+url.QueryEscape(errParam), http.StatusTemporaryRedirect)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/auth/callback?error=missing_code&provider=github", http.StatusTemporaryRedirect)
		return
	}

	userInfo, err := provider.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	// Org gate: when the admin has configured ANY authorized orgs, every
	// GitHub OAuth login is restricted to members of those orgs — including
	// users that already exist (e.g. created via email/password). They can
	// still sign in through the password form. Empty list disables the gate.
	hasGate, err := a.auth.HasAuthorizedGitHubOrgs()
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}
	if hasGate {
		ok, err := a.auth.IsGitHubOrgAuthorized(userInfo.Orgs)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
			return
		}
		if !ok {
			// Revoke the just-issued token so the user's next attempt
			// presents GitHub's consent screen — without this, GitHub
			// silently re-issues the same authorization to the same
			// account and the user is stuck on the rejection page.
			if gh, ok := provider.(*oauth.GitHub); ok {
				_ = gh.RevokeToken(r.Context(), userInfo.Token)
			}
			http.Redirect(w, r, "/auth/callback?error=org_not_authorized&provider=github", http.StatusTemporaryRedirect)
			return
		}
	}

	intent := readOAuthIntent(r)
	clearOAuthIntent(w)

	// Link mode attaches the just-completed identity to the
	// already-signed-in user — UpsertGitHubOAuthUser still runs (it's
	// idempotent for existing accounts and refreshes the org snapshot)
	// but we route success through the /account page instead of the
	// sign-in callback. A link request with no current session is a
	// misuse — the route requires auth — but check defensively.
	if intent == "link" {
		current := a.currentSessionUser(r)
		if current == nil {
			http.Redirect(w, r, "/auth/callback?error=link_unauthenticated&provider=github", http.StatusTemporaryRedirect)
			return
		}
		// Conflict guard: if this GitHub identity is already bound to
		// a different Nimbus user, refuse the link rather than silently
		// overwriting either side's binding.
		if userInfo.ProviderID != "" {
			existing, err := a.auth.FindUserByOAuthIdentity("github", userInfo.ProviderID)
			if err != nil {
				log.Printf("github link: lookup by id: %v", err)
				http.Redirect(w, r, "/account?error=link_failed&provider=github", http.StatusTemporaryRedirect)
				return
			}
			if existing != nil && existing.ID != current.ID {
				http.Redirect(w, r, "/account?error=already_linked_other&provider=github", http.StatusTemporaryRedirect)
				return
			}
		}
		// Update the user's org snapshot (so the dynamic org bypass
		// reflects this login) without minting a session. We touch the
		// row directly rather than going through UpsertGitHubOAuthUser
		// so we can't accidentally split the user record by email.
		orgs := strings.Join(userInfo.Orgs, ",")
		if orgs == "" {
			orgs = "-" // sentinel — distinguishes "linked, no orgs" from "never linked"
		}
		if err := a.auth.UpdateGitHubLinkSnapshot(current.ID, orgs); err != nil {
			log.Printf("github link: update snapshot: %v", err)
			http.Redirect(w, r, "/account?error=link_failed&provider=github", http.StatusTemporaryRedirect)
			return
		}
		if err := a.auth.MarkGitHubConnected(current.ID, userInfo.ProviderID); err != nil {
			log.Printf("github link: mark connected: %v", err)
			http.Redirect(w, r, "/account?error=link_failed&provider=github", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/account?linked=github", http.StatusTemporaryRedirect)
		return
	}

	// allowCreate is intentionally permissive here — the gate above already
	// rejected unauthorized users when the gate is on. When the gate is off,
	// new accounts are allowed (and will fall into the access code flow).
	user, _, err := a.auth.UpsertGitHubOAuthUser(userInfo.Name, userInfo.Email, userInfo.ProviderID, userInfo.Orgs, nil)
	if err != nil {
		if errors.Is(err, service.ErrUserSuspended) {
			http.Redirect(w, r, "/auth/callback?error=account_suspended&provider=github", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	sessionID, err := a.auth.CreateSession(user.ID)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=session_failed&provider=github", http.StatusTemporaryRedirect)
		return
	}

	setSessionCookie(w, sessionID)
	a.kickReconcile()

	q := url.Values{}
	q.Set("provider", "github")
	q.Set("login", userInfo.Login)
	http.Redirect(w, r, "/auth/callback?"+q.Encode(), http.StatusTemporaryRedirect)
}

// --- Google OAuth -----------------------------------------------------------

// GoogleStart handles GET /api/auth/google.
//
// @Summary     Start the Google OAuth sign-in flow (public)
// @Tags        auth
// @Success     307 "Redirect to Google OAuth consent"
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /auth/google [get]
func (a *Auth) GoogleStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider(r), "signin")(w, r)
}

// GoogleLinkStart is the link-mode counterpart of GoogleStart; the
// callback attaches the identity to the current session's user. See
// GitHubLinkStart for the rationale.
// GoogleLinkStart handles GET /api/auth/google/link.
//
// @Summary     Start the Google OAuth flow in link mode
// @Tags        auth
// @Success     307 "Redirect to Google OAuth consent"
// @Failure     500 {object} EnvelopeError
// @Failure     503 {object} EnvelopeError
// @Router      /auth/google/link [get]
func (a *Auth) GoogleLinkStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider(r), "link")(w, r)
}

// GoogleCallback wraps the shared OAuth callback flow with the
// authorized-domain check. New users whose domain is not on the admin's
// authorized-domain list are blocked before any account is created;
// returning users whose domain IS authorized are auto-verified against the
// current access code version so they bypass the /verify form.
// GoogleCallback handles GET /api/auth/google/callback.
//
// @Summary     Google OAuth callback (public)
// @Description See the GitHub callback for the shared sign-in/link semantics.
// @Tags        auth
// @Param       code  query string true "Authorization code"
// @Param       state query string true "CSRF state echoed by Google"
// @Success     307 "Redirect"
// @Router      /auth/google/callback [get]
func (a *Auth) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.googleProvider(r)
	if provider == nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/auth/callback?error=invalid_state&provider=google", http.StatusTemporaryRedirect)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Redirect(w, r, "/auth/callback?provider=google&error="+url.QueryEscape(errParam), http.StatusTemporaryRedirect)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/auth/callback?error=missing_code&provider=google", http.StatusTemporaryRedirect)
		return
	}

	userInfo, err := provider.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=exchange_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	// Domain gate: when the admin has configured ANY authorized domains, the
	// Google OAuth path is restricted to those domains for every login —
	// including users that already exist (e.g. created via email/password).
	// They can still sign in through the password form; Google OAuth itself
	// is the gated path. An empty list means the gate is off.
	hasGate, err := a.auth.HasAuthorizedGoogleDomains()
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}
	if hasGate {
		ok, err := a.auth.IsGoogleDomainAuthorized(userInfo.Email)
		if err != nil {
			http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
			return
		}
		if !ok {
			http.Redirect(w, r, "/auth/callback?error=domain_not_authorized&provider=google", http.StatusTemporaryRedirect)
			return
		}
	}

	intent := readOAuthIntent(r)
	clearOAuthIntent(w)

	if intent == "link" {
		current := a.currentSessionUser(r)
		if current == nil {
			http.Redirect(w, r, "/auth/callback?error=link_unauthenticated&provider=google", http.StatusTemporaryRedirect)
			return
		}
		// Refuse to link a Google identity that's already bound to a
		// different Nimbus account — would silently steal sign-in
		// from the other user. Also surfaced on the /account page so
		// the operator gets a clear message about the conflict.
		if userInfo.ProviderID != "" {
			existing, err := a.auth.FindUserByOAuthIdentity("google", userInfo.ProviderID)
			if err != nil {
				log.Printf("google link: lookup by sub: %v", err)
				http.Redirect(w, r, "/account?error=link_failed&provider=google", http.StatusTemporaryRedirect)
				return
			}
			if existing != nil && existing.ID != current.ID {
				http.Redirect(w, r, "/account?error=already_linked_other&provider=google", http.StatusTemporaryRedirect)
				return
			}
		}
		if err := a.auth.MarkGoogleConnected(current.ID, userInfo.ProviderID); err != nil {
			log.Printf("google link: mark connected: %v", err)
			http.Redirect(w, r, "/account?error=link_failed&provider=google", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/account?linked=google", http.StatusTemporaryRedirect)
		return
	}

	user, err := a.auth.UpsertOAuthUser(userInfo.Name, userInfo.Email, userInfo.ProviderID)
	if err != nil {
		if errors.Is(err, service.ErrUserSuspended) {
			http.Redirect(w, r, "/auth/callback?error=account_suspended&provider=google", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	// Sign-in mode also records the connection — every Google login,
	// new or returning, sets google_connected=true and stamps the
	// google_sub so the /account page, the passwordless straggler
	// check, and the next sign-in's identity lookup all stay in sync.
	if err := a.auth.MarkGoogleConnected(user.ID, userInfo.ProviderID); err != nil {
		log.Printf("google sign-in: mark connected: %v", err)
		// Non-fatal; sign-in continues.
	}

	// No DB write for the domain bypass — IsUserVerified consults the
	// authorized-domain list dynamically on every request, so the bypass
	// follows admin changes (add/remove) without lag.

	sessionID, err := a.auth.CreateSession(user.ID)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=session_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	setSessionCookie(w, sessionID)
	a.kickReconcile()

	q := url.Values{}
	q.Set("provider", "google")
	q.Set("login", userInfo.Login)
	http.Redirect(w, r, "/auth/callback?"+q.Encode(), http.StatusTemporaryRedirect)
}

// --- misc -------------------------------------------------------------------

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
