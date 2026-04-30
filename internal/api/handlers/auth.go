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

// NewAuth creates a new Auth handler. appURL is used for the Google OAuth
// redirect URI. reconciler may be nil — when set, a successful login kicks an
// async reconcile so the IP pool catches up with cross-instance changes
// without the user waiting on background-loop cadence.
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
// Returns nil if credentials are not configured.
func (a *Auth) googleProvider() oauth.Provider {
	settings, err := a.auth.GetOAuthSettings()
	if err != nil || settings.GoogleClientID == "" || settings.GoogleClientSecret == "" {
		return nil
	}
	return &oauth.Google{
		ClientID:     settings.GoogleClientID,
		ClientSecret: settings.GoogleClientSecret,
		RedirectURI:  a.appURL + "/api/auth/google/callback",
	}
}

// Providers handles GET /api/auth/providers — public endpoint the
// sign-in page consumes. Returns which OAuth providers have
// credentials configured AND whether the password form should still
// render. Password sign-in stays available unless the admin has
// turned on RequirePasswordlessAuth and zero users still depend on
// password access — see service.IsPasswordSignInActive for the rule.
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

// Login handles POST /api/auth/login.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
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
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	response.Success(w, ctxutil.User(r.Context()))
}

// ListUsers handles GET /api/users.
// Admins receive every account in the richer management shape (with
// signup time, verification status, and provider hints); non-admins
// receive only their own self-view (no list of peers).
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

// SetPasswordlessAuth handles PUT /api/settings/oauth/passwordless —
// admin-only toggle for the "remove password sign-in once everyone
// has OAuth" intent. The service guards against the requester
// locking themselves out (must have OAuth linked first).
type passwordlessToggleRequest struct {
	Enabled bool `json:"enabled"`
}

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
		if errors.Is(err, service.ErrRequesterNotLinked) {
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

// PasswordlessStatus handles GET /api/settings/oauth/passwordless —
// admin-only read of the current setting + straggler count, so the
// /users page can render the toggle and the explanatory banner.
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
	response.Success(w, map[string]any{
		"passwordless_goal": settings.RequirePasswordlessAuth,
		"stragglers":        stragglers,
		"password_active":   active,
	})
}

// VerifyStatus handles GET /api/access-code/status — returns whether the
// authenticated user is verified against the current access code version.
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

func (a *Auth) GitHubStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.githubProvider(), "signin")(w, r)
}

// GitHubLinkStart kicks off a GitHub OAuth dance whose callback will
// attach the resulting identity to the current session's user instead
// of creating/finding a user by email. Requires an authenticated
// session — middleware enforces that at the route level.
func (a *Auth) GitHubLinkStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.githubProvider(), "link")(w, r)
}

// GitHubCallback wraps the OAuth flow with the authorized-orgs check. New
// users whose GitHub org memberships don't intersect the admin's authorized
// list are blocked before any account is created. Every login refreshes the
// user's stored org snapshot so the dynamic bypass in IsUserVerified always
// reflects the most recent login's memberships.
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
			http.Redirect(w, r, "/auth/callback?error=link_failed&provider=github", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/account?linked=github", http.StatusTemporaryRedirect)
		return
	}

	// allowCreate is intentionally permissive here — the gate above already
	// rejected unauthorized users when the gate is on. When the gate is off,
	// new accounts are allowed (and will fall into the access code flow).
	user, _, err := a.auth.UpsertGitHubOAuthUser(userInfo.Name, userInfo.Email, userInfo.Orgs, nil)
	if err != nil {
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

func (a *Auth) GoogleStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider(), "signin")(w, r)
}

// GoogleLinkStart is the link-mode counterpart of GoogleStart; the
// callback attaches the identity to the current session's user. See
// GitHubLinkStart for the rationale.
func (a *Auth) GoogleLinkStart(w http.ResponseWriter, r *http.Request) {
	a.oauthStart(a.googleProvider(), "link")(w, r)
}

// GoogleCallback wraps the shared OAuth callback flow with the
// authorized-domain check. New users whose domain is not on the admin's
// authorized-domain list are blocked before any account is created;
// returning users whose domain IS authorized are auto-verified against the
// current access code version so they bypass the /verify form.
func (a *Auth) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	provider := a.googleProvider()
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
		if err := a.auth.MarkGoogleConnected(current.ID); err != nil {
			log.Printf("google link: mark connected: %v", err)
			http.Redirect(w, r, "/auth/callback?error=link_failed&provider=google", http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/account?linked=google", http.StatusTemporaryRedirect)
		return
	}

	user, err := a.auth.UpsertOAuthUser(userInfo.Name, userInfo.Email)
	if err != nil {
		http.Redirect(w, r, "/auth/callback?error=user_failed&provider=google", http.StatusTemporaryRedirect)
		return
	}

	// Sign-in mode also records the connection — every Google login,
	// new or returning, sets google_connected=true so the /account
	// page and the passwordless straggler check see consistent state.
	if err := a.auth.MarkGoogleConnected(user.ID); err != nil {
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
